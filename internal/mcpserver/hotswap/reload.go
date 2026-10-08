package hotswap

// reload.go — reload_self 的换手编排(桥热升级 spec 实施决策第 4 条)。
//
// 编排分两段(规格 F5 的应答时序由这两段保证):
//
//   - Arm(同步,由工具处理器调用):单飞占位 → 快照前任握手参数 →
//     StartSuccessor(拉起继任 + 等就绪)。失败就地回退并把原因如实带回,
//     处理器把它写进 reload_self 的应答(宿主可见);成功则处理器返回
//     「handover started」应答,由 SDK 照常写出。
//   - retire(goroutine):Hold(字节面冻结)→ 静默判据(发起调用的
//     应答已落盘,证明见 BridgeIO.Quiescent)→ 字节面交割(Retire:
//     SDK 读 EOF、feeder 切泵侧、copier 切泵侧)→ 等会话收场 →
//     PumpThen(化为泵,阻塞到终态)。任何一段失败都在「交割前」放弃
//     (ReleaseHold + 杀继任),旧桥继续应答(规格 D8);交割后不再可回,
//     泵按 F4 收场。

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/updater"
)

// ToolReloadSelf 是 reload_self 工具的名称(BrokerTools 第 14 把的单一
// 命名源;mcpserver 包的注册与热升级集成共用)。
const ToolReloadSelf = "reload_self"

// ReloadStatus 是 reload_self 的版本情报(不含忙判据——那是工具面自己
// 的实时快照)。
type ReloadStatus struct {
	CurrentVersion string // 本进程版本(buildinfo / 测试缝)
	DiskGeneration int64  // 盘上代际(信号缺失=0)
	DiskVersion    string // 盘上版本(信号缺失="" )
	Latest         string // GitHub 最新 release 的 tag(查询成功时)
	LatestError    string // 查询失败时的如实说明(成功时为空)
}

// ArmState 是 Arm 的结果状态。
type ArmState string

const (
	ArmArmed        ArmState = "armed"         // 继任已就绪,退位编排已启动
	ArmAlreadyArmed ArmState = "already_armed" // 已有一次换手在途(单飞)
	ArmFailed       ArmState = "failed"        // 拉起/就绪失败,已回退,原因在 Err
)

// ArmResult 是 Arm 的返回。
type ArmResult struct {
	State            ArmState
	SuccessorVersion string // Armed 时继任就绪文件自报的版本
	Err              error  // Failed 时的原因(带身份,可直接进错误文案)
}

// ReloadConfig 配置一个桥进程的 reload 编排。
type ReloadConfig struct {
	// IO 是字节面接管(必填)。
	IO *BridgeIO
	// Srv 用于换手前快照前任握手参数(必填)。
	Srv *mcp.Server
	// Exe/Args 是继任可执行与命令行(生产:os.Executable()+os.Args[1:])。
	Exe  string
	Args []string
	// Env 是附加给继任的环境(测试仪表;SSHMGR_HOTSWAP_* 会被库剥离重建)。
	Env []string
	// Adopted 报告本桥自身是否被领养拉起(后续代)。true 时 Arm 一律拒绝
	// ——后续代换手(直接继承句柄形态)是后续票的接线,本票只做首代。
	Adopted bool
	// BirthGeneration 是桥启动时记下的出生代际(票 01:updater.BirthGeneration)。
	BirthGeneration int64
	// SessionEnded 在 stdio 会话收场时关闭(ServeSession 返回处),退位
	// 编排等它确认 SDK 会话已结束。
	SessionEnded <-chan struct{}
	// Latest 只读查询最新 release 版本 tag(复用 updater 发现逻辑;nil =
	// 未接线,Report 如实报「未配置」)。
	Latest func(ctx context.Context) (string, error)
	// 各时限的非正值用默认:
	LatestTimeout  time.Duration // GitHub 查询预算,默认 10s
	ReadyTimeout   time.Duration // 继任就绪等待,默认 15s(库默认)
	PollInterval   time.Duration // 就绪轮询,默认 100ms(库默认)
	QuiesceTimeout time.Duration // 静默判据等待,默认 6min(盖住最长的
	//   在飞工具调用:exec_command 上限 300s + 余量)
	SessionEndTimeout  time.Duration // 交割后等 SDK 会话收场,默认 30s
	CopierDrainTimeout time.Duration // 等 copier 排干 SDK 应答,默认 30s
}

// ReloadService 是一个桥进程的换手编排器。
type ReloadService struct {
	cfg           ReloadConfig
	armed         atomic.Bool
	retireStarted atomic.Bool
	done          chan struct{}
	doneOnce      sync.Once
}

// NewReloadService 构造编排器并填默认时限。
func NewReloadService(cfg ReloadConfig) *ReloadService {
	if cfg.LatestTimeout <= 0 {
		cfg.LatestTimeout = 10 * time.Second
	}
	if cfg.QuiesceTimeout <= 0 {
		cfg.QuiesceTimeout = 6 * time.Minute
	}
	if cfg.SessionEndTimeout <= 0 {
		cfg.SessionEndTimeout = 30 * time.Second
	}
	if cfg.CopierDrainTimeout <= 0 {
		cfg.CopierDrainTimeout = 30 * time.Second
	}
	return &ReloadService{cfg: cfg, done: make(chan struct{})}
}

// Adopted 报告本桥是否被领养拉起(工具面用来决定 handover 字段文案)。
func (s *ReloadService) Adopted() bool { return s.cfg.Adopted }

// BirthGen 返回出生代际(工具面用来比对盘上代际)。
func (s *ReloadService) BirthGen() int64 { return s.cfg.BirthGeneration }

// Report 情报收集:当前/盘上版本与代际 + GitHub 最新(联网一次,失败
// 如实带错误,不致命)。
func (s *ReloadService) Report(ctx context.Context) ReloadStatus {
	st := ReloadStatus{CurrentVersion: BridgeVersion()}
	if sig, ok := updater.ReadGenerationSignal(s.cfg.Exe); ok {
		st.DiskGeneration = sig.Gen
		st.DiskVersion = sig.Version
	}
	if s.cfg.Latest == nil {
		st.LatestError = "latest-version lookup is not configured"
		return st
	}
	cctx, cancel := context.WithTimeout(ctx, s.cfg.LatestTimeout)
	defer cancel()
	tag, err := s.cfg.Latest(cctx)
	if err != nil {
		st.LatestError = err.Error()
		return st
	}
	st.Latest = tag
	return st
}

// Arm 武装一次换手(同步段):单飞 → 快照握手参数 → 拉继任 + 等就绪。
// 失败已就地回退并带原因返回(调用方写进工具应答);成功则内部已启动
// 退位 goroutine,调用方应答「started」后即由 goroutine 接管时序。
func (s *ReloadService) Arm() ArmResult {
	if s.cfg.Adopted {
		return ArmResult{State: ArmFailed, Err: fmt.Errorf("this bridge was itself adopted; later-generation handover is not wired yet")}
	}
	if !s.armed.CompareAndSwap(false, true) {
		return ArmResult{State: ArmAlreadyArmed}
	}
	var init *mcp.InitializeParams
	for ss := range s.cfg.Srv.Sessions() {
		init = ss.InitializeParams()
		break
	}
	if init == nil {
		s.armed.Store(false) // 允许下次重试
		return ArmResult{State: ArmFailed, Err: fmt.Errorf("no active MCP session to hand over")}
	}
	sess, err := SessionFromInitializeParams(init)
	if err != nil {
		s.armed.Store(false)
		return ArmResult{State: ArmFailed, Err: err}
	}
	h, err := StartSuccessor(Options{
		Exe:          s.cfg.Exe,
		Args:         s.cfg.Args,
		Session:      sess,
		Generation:   GenerationFirst,
		Env:          s.cfg.Env,
		ReadyTimeout: s.cfg.ReadyTimeout,
		PollInterval: s.cfg.PollInterval,
	})
	if err != nil {
		s.armed.Store(false) // 失败已回退,允许下次重试
		return ArmResult{State: ArmFailed, Err: err}
	}
	go s.retire(h)
	s.retireStarted.Store(true)
	return ArmResult{State: ArmArmed, SuccessorVersion: h.Version}
}

// WaitTerminal 阻塞到退位编排终态;从未启动过编排(Arm 未成功过)则立即
// 返回。stdio 入口在会话收场后调用它:退位为泵的进程要等泵终态才退出,
// 未退位的进程立即返回照常退出。(与迟到的 Arm 并发时以「进程正在退出」
// 为准——宿主已断,泵随进程消亡,继任读标准输入 EOF 自灭,规格 F4。)
func (s *ReloadService) WaitTerminal() {
	if !s.retireStarted.Load() {
		s.markDone() // 未启动过编排:此刻即终态
		return
	}
	<-s.done
}

func (s *ReloadService) markDone() {
	s.doneOnce.Do(func() { close(s.done) })
}

// logf 是编排器的统一 stderr 出口(宿主可见,规格:标准错误流不换轨)。
func (s *ReloadService) logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "sshmgr hotswap: "+format+"\n", args...)
}

// retire 是退位编排 goroutine(时序与回退边界见文件头注释)。
func (s *ReloadService) retire(h *SuccessorHandle) {
	defer s.markDone()

	// 1. 冻结字节面:此后到达的请求不再进 SDK(由继任经泵服务)。
	//    宿主已断(feeder 退役)则换手无意义,杀继任收场。
	if !s.cfg.IO.Hold() {
		if err := h.Rollback(); err != nil {
			s.logf("rollback after dead host side: %v", err)
		}
		s.logf("retire aborted: host side already closed")
		return
	}

	// 2. 静默判据:发起调用的应答(以及一切在途应答)已写出。
	//    超时放弃:解冻字节面、杀继任,旧桥继续应答(规格 D8)。
	quiesced := false
	deadline := time.Now().Add(s.cfg.QuiesceTimeout)
	for {
		if s.cfg.IO.Quiescent() {
			quiesced = true
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !quiesced {
		s.cfg.IO.ReleaseHold()
		if err := h.Rollback(); err != nil {
			s.logf("rollback after quiesce timeout: %v", err)
		}
		s.logf("retire aborted: in-flight requests did not drain within %v; bridge keeps serving", s.cfg.QuiesceTimeout)
		return
	}

	// 3. 交割字节面(此后不再可回)。
	hostInR, hostInW, err := os.Pipe()
	if err != nil {
		s.cfg.IO.ReleaseHold()
		if rerr := h.Rollback(); rerr != nil {
			s.logf("rollback after pipe creation failure: %v", rerr)
		}
		s.logf("retire aborted: pump-side host-in pipe: %v", err)
		return
	}
	hostOutR, hostOutW, err := os.Pipe()
	if err != nil {
		hostInR.Close()
		hostInW.Close()
		s.cfg.IO.ReleaseHold()
		if rerr := h.Rollback(); rerr != nil {
			s.logf("rollback after pipe creation failure: %v", rerr)
		}
		s.logf("retire aborted: pump-side host-out pipe: %v", err)
		return
	}
	s.cfg.IO.Retire(hostInW, hostOutR)

	// 4. 等 SDK 会话收场(静默已过,收场是即时的;超时只记日志——交割
	//    已完成,进程的出路只剩泵)。
	if s.cfg.SessionEnded != nil {
		select {
		case <-s.cfg.SessionEnded:
		case <-time.After(s.cfg.SessionEndTimeout):
			s.logf("session end wait timed out after %v (continuing to pump)", s.cfg.SessionEndTimeout)
		}
	}
	// 5. 等 copier 排干 SDK 应答(旧桥全部输出落到真实标准输出)。顺序性
	//    由 copier 单 goroutine 顺序搬运保证,超时只记日志。
	if !s.cfg.IO.WaitCopierPhase1(s.cfg.CopierDrainTimeout) {
		s.logf("copier drain wait timed out after %v (ordering is still sequential)", s.cfg.CopierDrainTimeout)
	}

	// 6. 化泵,阻塞到终态。
	if err := h.PumpThen(hostInR, hostOutW); err != nil {
		s.logf("pump ended: %v", err)
	}
}
