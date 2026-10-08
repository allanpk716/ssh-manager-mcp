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

// Handover 执行一次换手:以领养模式拉起继任、等它写出就绪文件、执行退位
// 前应答钩子,然后退位。
//
// 成功路径按代次分叉:GenerationFirst 化为泵并阻塞到终态(返回即本进程应
// 立即退出);GenerationLater 直接返回 nil(调用方应随即退出本进程)。
//
// 失败路径(继任起不来/就绪超时/继任未及就绪先退出/应答钩子失败)一律
// 回退(规格 D8):杀掉继任、释放泵侧管道,返回带原因的错误;本进程自身
// 状态未动,继续以服务进程应答。
func Handover(opts Options) error {
	if opts.Exe == "" {
		return errors.New("hotswap: Options.Exe is required")
	}
	if opts.Generation != GenerationFirst && opts.Generation != GenerationLater {
		return errors.New("hotswap: Options.Generation must be GenerationFirst or GenerationLater")
	}
	readyPath := opts.ReadyPath
	if readyPath == "" {
		f, err := os.CreateTemp("", "sshmgr-hotswap-ready-*.json")
		if err != nil {
			return fmt.Errorf("hotswap: allocate ready file: %w", err)
		}
		readyPath = f.Name()
		f.Close()
		os.Remove(readyPath) // 就绪文件由继任写出;父侧只轮询
	}
	childEnv, err := BuildEnv(append(os.Environ(), opts.Env...), opts.Session, readyPath)
	if err != nil {
		return err
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
			return err
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
		return fmt.Errorf("hotswap: start successor %q: %w", opts.Exe, err)
	}
	// 子进程已持有自己那份句柄;父侧副本必须关,否则管道 EOF 语义失效。
	pair.closeChildEnds()

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	// exited 只发一次、只收一次:收过后标记,避免回退收尾二次等待死锁。
	consumedExit := false
	drainExited := func() {
		if !consumedExit {
			<-exited
			consumedExit = true
		}
	}
	rollback := func(cause error) error {
		pair.close()
		if kerr := cmd.Process.Kill(); kerr != nil && !errors.Is(kerr, os.ErrProcessDone) {
			cause = errors.Join(cause, fmt.Errorf("hotswap: cleanup: kill successor: %w", kerr))
		}
		drainExited()
		return cause
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
		if _, ok := readReady(readyPath); ok {
			break
		}
		select {
		case werr := <-exited:
			consumedExit = true
			return rollback(fmt.Errorf("hotswap: %w before writing the ready file: %v (ready file %q)", ErrSuccessorExited, werr, readyPath))
		case <-tick.C:
		case <-alarm.C:
			return rollback(fmt.Errorf("hotswap: ready file %q: %w after %v", readyPath, ErrReadyTimeout, timeout))
		}
	}

	// 退位前应答钩子:失败即回退,不进入退位
	if opts.BeforeRetire != nil {
		if herr := opts.BeforeRetire(); herr != nil {
			return rollback(fmt.Errorf("hotswap: pre-retire hook: %w", herr))
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
	pumpErr := Pump(hostIn, hostOut, pair.toSuccessor, pair.fromSuccessor)
	// 泵到终态后继任若仍活着(如只关标准输出的畸形继任),补一刀防孤儿。
	// kill 的结果不并入返回值:此刻继任必已死或将死,而 Windows 上对
	// 「已退出但尚未收割」的进程 Kill 可能报 Access is denied,并入会
	// 污染泵结果的错误语义(errors.Is 身份)。
	_ = cmd.Process.Kill()
	drainExited()
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
