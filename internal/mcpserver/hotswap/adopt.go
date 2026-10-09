// Package hotswap 实现桥热升级的换手核心,与具体工具注册面解耦。
//
// 术语(与 .scratch/bridge-hot-upgrade/spec.md 一致):
//   - 桥:本进程——宿主机器上以标准输入输出(stdio)形态运行的 MCP 服务进程;
//   - 宿主:拉起桥的 AI agent 宿主(握有桥的标准输入输出的另一端);
//   - 继任:换手后接管已握手会话的新桥进程;
//   - 泵:退位后留在最外层、只双向搬运字节的哑进程;
//   - 首代:标准输入输出仍直连宿主的桥;后续代:标准输入输出对面已是泵侧
//     管道的桥;
//   - 退位:当前桥让出会话——首代化为泵,后续代直接退出。
//
// 本库是纯字节层:不解析任何 MCP/JSON-RPC 语义,会话参数只搬运不解释;
// 消息边界纪律(退位前应答写回、停读时机、无缓冲未消费字节)由上层接入层
// 负责保证后再调用 Handover。
package hotswap

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Generation 区分换手时的代次,决定继任标准输入输出的接线方式。
type Generation int

const (
	// GenerationFirst:首代——当前桥的标准输入输出还直连宿主。换手时新建
	// 一对泵侧管道接继任(继任不接触宿主侧句柄),本进程退位为泵。
	GenerationFirst Generation = iota + 1
	// GenerationLater:后续代——当前桥的标准输入输出对面已是泵侧管道。
	// 继任直接继承本进程的标准输入输出句柄;应答写回后本进程退出。
	GenerationLater
)

// ErrSuccessorExited 报告继任在写出就绪文件之前就已退出。
var ErrSuccessorExited = errors.New("successor exited before ready")

// Options 配置一次换手。
type Options struct {
	// Exe 是继任可执行文件路径(必填)。
	Exe string
	// Args 是继任的命令行参数(可选)。
	Args []string
	// Session 是领养会话参数(已协商 MCP 协议版本、宿主能力、桥形态与实例
	// 参数等),经 SSHMGR_HOTSWAP_* 环境变量传给继任;库只搬运不解释。
	Session Session
	// Generation 决定继任标准输入输出的接线方式(必填)。
	Generation Generation
	// Env 是附加给继任的环境变量(测试仪表等)。其中的 SSHMGR_HOTSWAP_*
	// 条目仍会被剥离并由 Session/ReadyPath 重建;同键后值覆盖前值。
	Env []string
	// ReadyPath 是就绪文件路径;留空则自动分配临时路径。
	ReadyPath string
	// ReadyTimeout 是就绪等待上限;非正值用默认 15 秒。
	ReadyTimeout time.Duration
	// PollInterval 是就绪轮询间隔;非正值用默认 100 毫秒。
	PollInterval time.Duration
	// HostIn/HostOut 是首代泵化用的宿主侧句柄;留空则 os.Stdin/os.Stdout。
	// 仅 GenerationFirst 使用——后续代的继任直接继承本进程句柄,与此无关。
	HostIn, HostOut *os.File
	// Stderr 是继任标准错误的去向;留空则接本进程同流 os.Stderr(首代语义:
	// 宿主可见日志不换轨)。
	Stderr *os.File
	// BeforeRetire 是退位前应答钩子:就绪成功后、泵化/退出前执行,供上层把
	// 发起调用的应答写回宿主。返回错误则整次换手回退(不退位、杀继任)。
	BeforeRetire func() error
}

// Handover 执行一次完整换手:以领养模式拉起继任、等它写出就绪文件、执行退位
// 前应答钩子,然后退位。
//
// 成功路径按代次分叉:GenerationFirst 化为泵并阻塞到终态(返回即本进程应
// 立即退出);GenerationLater 直接返回 nil(调用方应随即退出本进程)。
//
// 失败路径(继任起不来/就绪超时/继任未及就绪先退出/应答钩子失败)一律
// 回退(规格 D8):杀掉继任、释放泵侧管道,返回带原因的错误;本进程自身
// 状态未动,继续以服务进程应答。
//
// 需要在「就绪已确认」与「退位」之间插自定义动作的调用方(如 reload_self
// 先返回工具应答、再退位)用两段式 API:StartSuccessor + SuccessorHandle。
func Handover(opts Options) error {
	h, err := StartSuccessor(opts)
	if err != nil {
		return err
	}
	// 退位前应答钩子:失败即回退,不进入退位
	if opts.BeforeRetire != nil {
		if herr := opts.BeforeRetire(); herr != nil {
			return h.rollback(fmt.Errorf("hotswap: pre-retire hook: %w", herr))
		}
	}
	if opts.Generation == GenerationLater {
		// 继任已持有全部所需句柄;本进程的退出由调用方完成。
		return nil
	}
	hostIn, hostOut := opts.HostIn, opts.HostOut
	if hostIn == nil {
		hostIn = os.Stdin
	}
	if hostOut == nil {
		hostOut = os.Stdout
	}
	return h.PumpThen(hostIn, hostOut)
}

// SuccessorHandle 是一次已武装的换手继任:继任进程已拉起、就绪文件已确认,
// 泵侧管道两端由本结构持有,等待调用方在退位时机二选一——Rollback(放弃
// 退位,回退)或 PumpThen(首代退位为泵)。GenerationLater 的继任直接继承
// 本进程句柄(handle 不持管道),退位动作由调用方退出本进程完成。
type SuccessorHandle struct {
	cmd       *exec.Cmd
	pair      *pipePair // GenerationFirst 的泵侧管道;GenerationLater 为 nil
	readyPath string
	exited    chan error
	exitOnce  sync.Once
	// Version 是继任就绪文件里自报的版本。
	Version string
}

// StartSuccessor 执行换手的前半程:以领养模式拉起继任并等待它写出就绪
// 文件。任何失败(拉起失败/就绪超时/继任未及就绪先退出)都已就地回退
// (杀继任 + 释放泵侧管道)后返回带原因的错误;成功返回 SuccessorHandle,
// 调用方此后择机 Rollback 或 PumpThen。参数语义与 Handover 相同
// (BeforeRetire/HostIn/HostOut 在这一段不参与)。
func StartSuccessor(opts Options) (*SuccessorHandle, error) {
	if opts.Exe == "" {
		return nil, errors.New("hotswap: Options.Exe is required")
	}
	if opts.Generation != GenerationFirst && opts.Generation != GenerationLater {
		return nil, errors.New("hotswap: Options.Generation must be GenerationFirst or GenerationLater")
	}
	readyPath := opts.ReadyPath
	if readyPath == "" {
		f, err := os.CreateTemp("", "sshmgr-hotswap-ready-*.json")
		if err != nil {
			return nil, fmt.Errorf("hotswap: allocate ready file: %w", err)
		}
		readyPath = f.Name()
		f.Close()
		os.Remove(readyPath) // 就绪文件由继任写出;父侧只轮询
	}
	childEnv, err := BuildEnv(append(os.Environ(), opts.Env...), opts.Session, readyPath)
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(opts.Exe, opts.Args...)
	cmd.Env = childEnv
	if opts.Stderr != nil {
		cmd.Stderr = opts.Stderr
	} else {
		cmd.Stderr = os.Stderr
	}
	applyHiddenWindow(cmd)

	var pair *pipePair
	if opts.Generation == GenerationFirst {
		if pair, err = newPipePair(); err != nil {
			return nil, err
		}
		cmd.Stdin = pair.childStdin
		cmd.Stdout = pair.childStdout
	} else {
		// 后续代:继任直接继承本进程当前的标准输入输出句柄
		//(Windows 上 0/1 号句柄继承原生支持;规格 Further Notes)。
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
	}
	if err := cmd.Start(); err != nil {
		pair.close()
		return nil, fmt.Errorf("hotswap: start successor %q: %w", opts.Exe, err)
	}
	// 子进程已持有自己那份句柄;父侧副本必须关,否则管道 EOF 语义失效。
	pair.closeChildEnds()

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	h := &SuccessorHandle{cmd: cmd, pair: pair, readyPath: readyPath, exited: exited}

	// exited 只发一次、只收一次:收过后标记,避免回退收尾二次等待死锁。
	drainExited := func() { h.exitOnce.Do(func() { <-h.exited }) }
	rollback := func(cause error) (*SuccessorHandle, error) {
		pair.close()
		// Kill 只是「确保继任消失」的手段:Windows 对「已退出但尚未收割」
		// 的进程会报 ERROR_ACCESS_DENIED,收割能完成即目标已达成,故 Kill
		// 失败不并入错误(与 PumpThen 的补刀同一语义);进程若真还活着,
		// 下面的收割会一直等,问题不会静默。
		_ = cmd.Process.Kill()
		drainExited()
		os.Remove(readyPath) // 就绪文件生命周期止于确认,回退即收走
		return nil, cause
	}

	poll := opts.PollInterval
	if poll <= 0 {
		poll = DefaultPollInterval
	}
	timeout := opts.ReadyTimeout
	if timeout <= 0 {
		timeout = DefaultReadyTimeout
	}
	tick := time.NewTicker(poll)
	defer tick.Stop()
	alarm := time.NewTimer(timeout)
	defer alarm.Stop()
	for {
		if r, ok := readReady(readyPath); ok {
			h.Version = r.Version
			return h, nil
		}
		select {
		case werr := <-exited:
			h.exitOnce.Do(func() {}) // 本 select 已收走 exited 的唯一一次发送
			return rollback(fmt.Errorf("hotswap: %w before writing the ready file: %v (ready file %q)", ErrSuccessorExited, werr, readyPath))
		case <-tick.C:
		case <-alarm.C:
			return rollback(fmt.Errorf("hotswap: ready file %q: %w after %v", readyPath, ErrReadyTimeout, timeout))
		}
	}
}

// Rollback 放弃退位并回退:杀掉继任、释放泵侧管道(幂等,重复调用无害)。
// 用于就绪已确认之后、退位动作尚未开始的放弃路径(规格 D8 的回退语义
// 覆盖到「退位动作开始前」为止——开始后本进程字节面已交出,不再可回)。
// 返回值只含收尾错误(杀失败等);「主动放弃」本身不是错误。
func (h *SuccessorHandle) Rollback() error {
	return h.rollback(nil)
}

func (h *SuccessorHandle) rollback(cause error) error {
	h.pair.close()
	// Kill 只是「确保继任消失」的手段(Windows 对已退出未收割的进程报
	// ERROR_ACCESS_DENIED,收割能完成即目标已达成;见 StartSuccessor 内
	// 同一处理)。
	_ = h.cmd.Process.Kill()
	h.exitOnce.Do(func() { <-h.exited })
	os.Remove(h.readyPath) // 就绪文件生命周期止于确认,回退即收走
	return cause
}

// cleanupReady 收走就绪文件(就绪确认之后文件已无读者;避免临时目录
// 遗留垃圾——票 03 评审留档)。幂等。
func (h *SuccessorHandle) cleanupReady() {
	if h.readyPath != "" {
		os.Remove(h.readyPath)
	}
}

// PumpThen 执行首代退位:化为泵,在宿主侧句柄与泵侧管道之间双向搬运,
// 阻塞到泵终态(返回即本进程应立即退出)。GenerationLater 的 handle 不持
// 泵侧管道,调用它报错——后续代的退位是调用方直接退出本进程。
func (h *SuccessorHandle) PumpThen(hostIn, hostOut *os.File) error {
	if h.pair == nil {
		return errors.New("hotswap: PumpThen on a GenerationLater successor (caller exits instead)")
	}
	defer h.cleanupReady()
	pumpErr := Pump(hostIn, hostOut, h.pair.toSuccessor, h.pair.fromSuccessor)
	// 泵到终态后继任若仍活着(如只关标准输出的畸形继任),补一刀防孤儿。
	// kill 的结果不并入返回值:此刻继任必已死或将死,而 Windows 上对
	// 「已退出但尚未收割」的进程 Kill 可能报 Access is denied,并入会
	// 污染泵结果的错误语义(errors.Is 身份)。
	_ = h.cmd.Process.Kill()
	h.exitOnce.Do(func() { <-h.exited })
	return pumpErr
}

// pipePair 是首代换手新建的一对泵侧管道:子端交给继任作标准输入输出,
// 泵端由退位后的本进程持有(Pump 的对端)。
type pipePair struct {
	childStdin    *os.File // 继任的标准输入(读端)——交给 exec,Start 后父侧即关
	childStdout   *os.File // 继任的标准输出(写端)——交给 exec,Start 后父侧即关
	toSuccessor   *os.File // 泵侧写端:喂继任标准输入
	fromSuccessor *os.File // 泵侧读端:收继任标准输出
}

func newPipePair() (*pipePair, error) {
	succInR, succInW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("hotswap: successor stdin pipe: %w", err)
	}
	succOutR, succOutW, err := os.Pipe()
	if err != nil {
		succInR.Close()
		succInW.Close()
		return nil, fmt.Errorf("hotswap: successor stdout pipe: %w", err)
	}
	return &pipePair{
		childStdin:    succInR,
		childStdout:   succOutW,
		toSuccessor:   succInW,
		fromSuccessor: succOutR,
	}, nil
}

// closeChildEnds 关掉父侧持有的子端句柄(子进程已有自己的副本)。
func (p *pipePair) closeChildEnds() {
	if p == nil {
		return
	}
	p.childStdin.Close()
	p.childStdout.Close()
}

// close 释放全部四端(重复关闭无害),回退路径用。
func (p *pipePair) close() {
	if p == nil {
		return
	}
	p.closeChildEnds()
	p.toSuccessor.Close()
	p.fromSuccessor.Close()
}
