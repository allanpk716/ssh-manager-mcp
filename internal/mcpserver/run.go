package mcpserver

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/mcpserver/hotswap"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/updater"
	"ssh-manager-mcp/internal/vault"
)

// RunStdio resolves the token to a project+profile, builds the scoped server, and runs it over stdio.
// Returns an error if the store is locked or the token is unknown (caller prints to stderr + exits).
//
// The platform master-key KeyProvider is INJECTED by the caller (the cli/keychain
// seam) so this package stays OS-agnostic and doesn't import cli. vault.OpenStore
// resolves env → kp → FileProvider (3-tier, spec §5.6).
func RunStdio(token string, kp store.KeyProvider) error {
	st, err := vault.OpenStore(kp)
	if err != nil {
		return err
	}
	defer st.Close()
	project, err := st.VerifyToken(token)
	if err != nil {
		return err
	}
	if project == nil {
		return fmt.Errorf("invalid or unknown token")
	}
	srv, tunnels, tasks, err := NewServer(st, project.ProfileID, project.ID)
	if err != nil {
		return err
	}
	// MCP-shutdown teardown: when the agent disconnects (stdin closes) srv.Run
	// returns and the deferred CloseAlls tear down every open forward_port
	// tunnel — listener + owning ssh.Client — and every background task
	// (running task goroutines + their owning ssh.Clients) so the process exits
	// with no leaked SSH connections. Both sweeper goroutines are also stopped.
	// (The go-sdk MCP server has no per-server shutdown hook on the Run path —
	// its session onClose fires per-session; we teardown at Run-return instead,
	// which is the single-session stdio case.)
	defer tunnels.CloseAll()
	defer tasks.CloseAll()
	return serveBridge(context.Background(), srv)
}

// hydrateCacheStore builds a fresh temporary read-only store from snap and
// verifies token against it. The caller owns closing the store and removing
// tmpPath (cacheStoreHolder registers both). Shared by initial startup and
// every hot rebuild, so the two paths CANNOT drift. device ("" = unknown) is
// the instance's device-code name stamped onto host_keys rows written via
// ApplyForwardedHostKey (Plan 48 §2.3) — per-generation, because every
// hydrated store starts from a fresh temp db.
func hydrateCacheStore(token string, snap *store.Snapshot, auditFile *os.File, device string) (*store.Store, *models.Project, string, error) {
	mk, err := store.GenerateMasterKey() // throwaway key: creds re-sealed per hydration
	if err != nil {
		return nil, nil, "", err
	}
	tmp, err := os.CreateTemp("", "sshmgr-cache-*.db")
	if err != nil {
		return nil, nil, "", err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	st, err := store.Open(tmpPath, mk)
	if err != nil {
		os.Remove(tmpPath)
		return nil, nil, "", err
	}
	if err := st.ImportSnapshot(snap); err != nil {
		st.Close()
		os.Remove(tmpPath)
		return nil, nil, "", err
	}
	st.SetReadOnly(auditFile)   // AFTER ImportSnapshot: mutations → ErrReadOnly
	st.SetForwardDevice(device) // Plan 48 §2.3: local forwarded-pin metadata
	project, err := st.VerifyToken(token)
	if err != nil {
		st.Close()
		os.Remove(tmpPath)
		return nil, nil, "", err
	}
	if project == nil {
		st.Close()
		os.Remove(tmpPath)
		return nil, nil, "", fmt.Errorf("invalid or unknown token")
	}
	return st, project, tmpPath, nil
}

// cacheStoreHolder owns the hot-reloading read-only store behind mcp --cache.
// Swapped-out stores are NOT closed on swap: the SDK dispatches tool calls on
// separate goroutines, so an in-flight call may still hold the old pointer —
// closing it would surface "sql: database is closed" as a tool error. They are
// registered in stores/tmpPaths and torn down once at process exit instead
// (rebuilds are rare; the leak is bounded and harmless).
type cacheStoreHolder struct {
	reload    func() (*store.Snapshot, bool, error)
	token     string
	auditFile *os.File
	profileID string
	device    string // device-code name for ApplyForwardedHostKey metadata (Plan 48 §2.3)

	mu       sync.Mutex // serializes rebuilds
	cur      atomic.Pointer[store.Store]
	lastSnap *store.Snapshot // guarded by mu — last snapshot successfully hydrated
	stores   []*store.Store  // every hydrated store, closed once in cleanup
	tmpPaths []string        // every temp db, removed once in cleanup
}

// Current returns the store to serve THIS tool call from, rebuilding first if
// the reload callback reports a change. Every failure path keeps serving the
// previous store — Lazy revocation semantics: a session outlives its token
// until the next spawn.
func (h *cacheStoreHolder) Current() *store.Store {
	if h.reload == nil {
		return h.cur.Load()
	}
	// Consult + memoize + hydrate must all happen under h.mu: consulting the
	// reloader outside the lock allows a goroutine holding a stale snapshot
	// pointer to be preempted, another goroutine to hydrate a NEWER change,
	// and the stale one to then re-hydrate over it — silently serving an
	// older snapshot until the next disk change. Serializing the whole
	// sequence prevents out-of-order stale serving.
	h.mu.Lock()
	defer h.mu.Unlock()
	snap, changed, err := h.reload()
	if err != nil {
		fmt.Fprintf(os.Stderr, "sshmgr: cache reload check failed (keeping current snapshot): %v\n", err)
		return h.cur.Load()
	}
	if !changed {
		return h.cur.Load()
	}
	if snap == nil {
		// changed=true with a nil snapshot is a broken reloader; hydrateCacheStore
		// would nil-deref in ImportSnapshot. Log + keep serving the old store.
		fmt.Fprintf(os.Stderr, "sshmgr: cache reload reported a change with a nil snapshot (keeping current snapshot)\n")
		return h.cur.Load()
	}
	// A concurrent rebuild may have consumed this exact snapshot already (the
	// reloader advances its baseline on a successful load, but a one-shot
	// change report must not be dropped by a recheck, and must not trigger a
	// second hydration of the same snapshot either): memoize the pointer of
	// the last successfully hydrated snapshot. Pointer identity is sound — a
	// real reloader mints a fresh *Snapshot for every genuine change.
	if snap == h.lastSnap {
		return h.cur.Load()
	}
	st, project, tmpPath, err := hydrateCacheStore(h.token, snap, h.auditFile, h.device)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sshmgr: cache hot-reload failed (keeping current snapshot): %v\n", err)
		return h.cur.Load()
	}
	if project.ProfileID != h.profileID {
		// The owner rebound the project to a different profile mid-session; the
		// tool closures still scope by the startup profileID, so serving the new
		// store would show the wrong set. Keep the old store + log.
		fmt.Fprintf(os.Stderr, "sshmgr: cache snapshot changed the project's profile (keeping current snapshot to preserve scoping)\n")
		st.Close()
		os.Remove(tmpPath)
		return h.cur.Load()
	}
	h.lastSnap = snap
	h.stores = append(h.stores, st)
	h.tmpPaths = append(h.tmpPaths, tmpPath)
	h.cur.Store(st) // old store intentionally left open (in-flight calls)
	return st
}

// cleanup closes every hydrated store and removes every temp db. Called once,
// deferred from RunStdioCache.
//
// Reading stores/tmpPaths without the mutex is safe: cleanup is invoked exactly
// once via defer after srv.Run returns, at which point no in-flight tool calls
// remain to race a rebuild.
func (h *cacheStoreHolder) cleanup() {
	for _, s := range h.stores {
		s.Close()
	}
	h.stores = nil
	for _, p := range h.tmpPaths {
		os.Remove(p)
	}
	h.tmpPaths = nil
}

// newCacheStoreHolderFromSnapshot constructs the holder EXACTLY as RunStdioCache
// needs it: hydrates the initial snapshot, verifies the token, and pins the
// holder's profileID to the VERIFIED project's profile. That pin is load-bearing:
// Current()'s drift guard compares every hot-reload candidate's ProfileID against
// it, so a holder built with profileID unset rejects EVERY genuine change — hot
// reload silently disabled (the production-path bug this constructor exists to
// make impossible). Shared with the seam test so the production construction and
// the tested construction can never drift apart.
func newCacheStoreHolderFromSnapshot(token string, snap *store.Snapshot, af *os.File, reload func() (*store.Snapshot, bool, error), device string) (*cacheStoreHolder, *models.Project, error) {
	h := &cacheStoreHolder{reload: reload, token: token, auditFile: af, device: device}
	st, project, tmpPath, err := hydrateCacheStore(token, snap, af, device)
	if err != nil {
		return nil, nil, err
	}
	h.profileID = project.ProfileID // set BEFORE the holder is served: Current()'s drift guard depends on it
	h.cur.Store(st)
	h.stores = append(h.stores, st)
	h.tmpPaths = append(h.tmpPaths, tmpPath)
	return h, project, nil
}

// RunStdioCache hydrates a Snapshot into a temporary read-only store, verifies the SAME
// project token against the cached projects (iron rule + profile scoping intact offline), and
// runs the broker over stdio — identical agent surface to RunStdio. Offline audit lands in
// auditPath (a JSONL sidecar); every mutation is refused (ErrReadOnly). An UNKNOWN host key
// goes through fwdBinder when one is wired (Plan 48 §2.1: the key is forwarded to the broker's
// audited /pin-hostkey endpoint, applied locally, and the handshake continues); with a nil
// binder the pre-Plan-48 behavior stands (SaveHostKey → ErrReadOnly → fail closed). The temp
// store is deleted on exit; creds in it are sealed under a throwaway master key.
//
// reload != nil enables hot-reload: before every tool call the callback is consulted
// ((snap,true,nil) = rebuild; (nil,false,nil) = unchanged; error = keep serving the old
// store). Each genuine change MUST be reported with a FRESH *Snapshot pointer (pointer
// identity is what dedupes concurrent rebuilds); re-reporting the same pointer with
// changed=true is skipped. reload == nil disables it. Swapped-out stores stay open until
// process exit (in-flight SDK tool calls may hold the old pointer); cacheStoreHolder.cleanup
// tears every hydrated store down once.
//
// fwdBinder is the host-key store binder cli builds via
// clientops.ForwardingHostKeys(NewPinForwarder(cred)) — it is invoked ONCE here with the
// holder's Current resolver (Plan 48 §2.2: the wrapper re-resolves the current generation
// inside every callback, never capturing a store pointer). nil binder = no override. The
// binder (and any forwarder capability) is NEVER defaulted here: reading an instance's
// credential stays at the --instance resolver in cli (Plan 40/46 iron rule).
//
// device ("" = unknown) is the instance's device-code name stamped onto locally applied
// forwarded pins (host_keys.pin_device, Plan 48 §2.3) — metadata only, never load-bearing.
//
// Agent-surface invariant: the broker reads the cache via the exact same
// list_servers / exec_command / download_file / upload_file / forward_port / close_port
// tools, gated by the SAME profile scoping (profileID from the verified project) and attributing
// audit to the SAME project id. The only difference from RunStdio is that the store is read-only
// (mutations refused) and audit is sidecar'd (per-machine, single-direction, zero-merge).
func RunStdioCache(token string, snap *store.Snapshot, auditPath string, reload func() (*store.Snapshot, bool, error), fwdBinder HostKeyStoreBinder, device string, metaEditor MetadataEditor) error {
	srv, tunnels, tasks, cleanup, err := NewCacheBroker(token, snap, auditPath, reload, fwdBinder, device, metaEditor)
	if err != nil {
		return err
	}
	defer cleanup()
	defer tunnels.CloseAll()
	defer tasks.CloseAll()
	return serveBridge(context.Background(), srv)
}

// NewCacheBroker assembles the hot-reloading read-only cache broker without
// running it: the same hydration, token verification, binder wiring, and
// profile scoping RunStdioCache serves, as a directly drivable *mcp.Server
// (RunStdioCache runs it over stdio; the Plan 48 cache-forwarding integration
// tests drive it over in-memory transports so the tested wiring IS the served
// wiring). cleanup releases the hydrated stores, their temp dbs, and the audit
// sidecar handle exactly once; the caller SHOULD also defer tunnels.CloseAll /
// tasks.CloseAll (MCP-shutdown teardown, as in RunStdio). Parameters carry the
// same contract as RunStdioCache.
func NewCacheBroker(token string, snap *store.Snapshot, auditPath string, reload func() (*store.Snapshot, bool, error), fwdBinder HostKeyStoreBinder, device string, metaEditor MetadataEditor) (*mcp.Server, *TunnelManager, *TaskManager, func(), error) {
	af, err := os.OpenFile(auditPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	srv, tunnels, tasks, h, err := newCacheBroker(token, snap, af, reload, fwdBinder, device, metaEditor)
	if err != nil {
		af.Close()
		return nil, nil, nil, nil, err
	}
	return srv, tunnels, tasks, func() { h.cleanup(); af.Close() }, nil
}

// newCacheBroker assembles the hot-reloading read-only broker exactly as
// RunStdioCache serves it — the single construction path shared with the
// exported NewCacheBroker so the tested wiring cannot drift from the served
// wiring. The binder is invoked once with the holder's Current; the resulting
// wrapper is a process-lifetime value (it captures no store pointer).
func newCacheBroker(token string, snap *store.Snapshot, af *os.File, reload func() (*store.Snapshot, bool, error), fwdBinder HostKeyStoreBinder, device string, metaEditor MetadataEditor) (*mcp.Server, *TunnelManager, *TaskManager, *cacheStoreHolder, error) {
	h, project, err := newCacheStoreHolderFromSnapshot(token, snap, af, reload, device)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	var hkFn func() sshbroker.HostKeyStore
	if fwdBinder != nil {
		hk := fwdBinder(h.Current)
		hkFn = func() sshbroker.HostKeyStore { return hk }
	}
	srv, tunnels, tasks, err := NewServerFromSource(h.Current, project.ProfileID, project.ID, hkFn, metaEditor)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return srv, tunnels, tasks, h, nil
}

// serveBridge 在一个已构造好的 broker 服务面上接入桥热升级(stdio 两形态
// 入口 RunStdio/RunStdioCache 的共同收尾,规格实施决策第 3/4 条):
//
//   - 领养检测:SSHMGR_HOTSWAP_* 环境存在即本桥是被领养拉起的继任——以
//     注入的「已握手」会话状态起服务(不再收 initialize),写出就绪文件,
//     并向已领养会话补发一次 tools/list_changed(重新注册 reload_self 触发
//     SDK 的清单变更通知)。
//   - 字节面接管:真实标准输入输出的读写收进 hotswap.BridgeIO(细节见
//     该文件头注释),为可能的退位做准备——首代化泵、后续代停读退出,
//     两条退位路径共用这一接管面。
//   - reload_self 注册:工具面第 14 把(BrokerTools[13]),忙判据经
//     serverRuntimes 登记表取本服务的 BusyTracker。
//   - 自动换手循环(票 06,spec 实施决策第 1/2 条):定期(默认约 30 秒,
//     SSHMGR_TEST_AUTOSWAP_INTERVAL_MS 缝)比对盘上代际与出生代际,新且
//     不忙时经与 reload_self 同一个 Arm 入口静默换手;忙时静默跳过本轮。
//     多把桥(不同缓存实例)各自起一个循环,互不干扰(实施决策第 5 条)。
//   - 收尾次序:会话结束(宿主断开或退位交割)→ 等换手编排终态(退位为
//     泵的进程要等泵终态才退;未退位的进程立即返回)→ 调用方的 defer
//     (CloseAll 等)照常执行。
func serveBridge(ctx context.Context, srv *mcp.Server) error {
	exe, exeErr := os.Executable()

	sessEnv, readyPath, adopted := hotswap.ParseEnv(os.Environ())
	var adoptState *mcp.ServerSessionState
	if adopted {
		var err error
		adoptState, err = hotswap.AdoptedSessionState(sessEnv)
		if err != nil {
			// 领养环境存在但无法解读:起一个「会拒绝一切请求」的假握手服务
			// 没有意义(宿主已握手,不会再发 initialize),宁可失败退出——
			// 父桥会在就绪超时后回退。
			return err
		}
	}

	// 首代与被领养的后续代一律经 feeder 接管读侧:后续代退位(Park 停读
	// → EndSdkRead)以此为前提,直读真实标准输入的形态给不出「停读」
	// 的确定点。
	bio, err := hotswap.NewBridgeIO(os.Stdin, os.Stdout, true)
	if err != nil {
		return err
	}
	defer bio.Close()

	sessionEnded := make(chan struct{})
	cfg := hotswap.ReloadConfig{
		IO:           bio,
		Srv:          srv,
		SessionEnded: sessionEnded,
		// 测试仪表缝:换手端到端测试用同一测试二进制扮两代,经
		// SSHMGR_TEST_SUCCESSOR_ENV 给继任注入版本/角色环境(生产为空)。
		Env: successorEnvSeam(),
		// 票 05:被领养桥(后续代)照常换手——GenerationLater 继承句柄
		// 形态(继任继承本进程的标准输入输出=泵侧管道,应答写回后本进程
		// 退出,不化泵);工具面不因领养身份拒绝,Adopted 只留给取不到
		// 可执行路径、无力拉继任的桥。
		LaterGeneration: adopted,
		Latest:          latestReleaseTag,
	}
	if exeErr == nil {
		cfg.Exe = exe
		// 继任 = 同一可执行 + 原始命令行(os.Args[1:] 继承子命令与旗标,
		// 如 mcp --cache --instance <名>)——裸二进制拉起的继任不成桥,
		// 就绪超时必然回退,热升级在真机不可用(票 06 修复的生产阻断缺陷;
		// 测试角色进程 argv[1:] 为空,此默认对测试形态无影响)。
		cfg.Args = os.Args[1:]
		// 出生代际快照(票 01):此后 reload_self 与自动换手循环都以
		// 「盘上代际 > 出生代际」判新(不比版本大小,降级同样触发;文件
		// 缺失/损坏=无信号)。
		cfg.BirthGeneration = updater.BirthGeneration(exe)
	} else {
		fmt.Fprintf(os.Stderr, "sshmgr: hotswap disabled (os.Executable: %v)\n", exeErr)
		cfg.Adopted = true // 无可执行路径即无可拉继任:Arm 拒绝、工具面报 not_first_generation
	}
	// 票 06:自动换手循环的忙判据——与 reload_self 同一登记表、同一快照
	// 语义(busyTrackerFor 未登记的裸构造退化为 nil=永不忙)。
	cfg.Busy = autoSwapBusyClosure(srv)
	applyReloadTimingSeams(&cfg)
	rs := hotswap.NewReloadService(cfg)

	register := func() { registerReloadSelfTool(srv, busyTrackerFor(srv), rs) }
	register()

	// 自动换手循环(票 06):与工具触发共用 rs.Arm 单飞入口。ctx 随本函数
	// 返回取消——退位为泵的进程要到泵终态才返回,泵阶段循环空转(Arm 单飞
	// 必然 already_armed),未退位的进程照常退出。
	autoCtx, cancelAuto := context.WithCancel(ctx)
	defer cancelAuto()
	rs.StartAutoSwap(autoCtx)

	serveErr := hotswap.ServeSession(ctx, srv, bio.Transport(), adoptState, func(ss *mcp.ServerSession) {
		if !adopted {
			return
		}
		// 继任收尾三连:就绪文件(父桥在等它确认后才开始退位)→ 补发清单
		// 变更通知(重新注册同一工具,SDK 的 changeAndNotify 会向本会话发
		// 一次 tools/list_changed;幂等替换,清单内容不变,规格 D3)。
		if werr := hotswap.WriteReady(readyPath, hotswap.BridgeVersion()); werr != nil {
			fmt.Fprintf(os.Stderr, "sshmgr: hotswap ready file: %v (parent will roll back)\n", werr)
			return
		}
		register()
	})
	close(sessionEnded)
	rs.WaitTerminal()
	return serveErr
}

// latestReleaseTag 是 reload_self 的「GitHub 最新版本」只读查询:复用
// internal/updater 的发现逻辑(同一传输与白名单,SSHMGR_UPDATE_BASE 缝),
// 不下载任何资产,只取 release 的 tag。
func latestReleaseTag(ctx context.Context) (string, error) {
	rel, err := updater.LatestRelease(ctx)
	if err != nil {
		return "", err
	}
	return rel.Tag, nil
}

// successorEnvSeam 解析 SSHMGR_TEST_SUCCESSOR_ENV(分号分隔的 K=V 列表)
// 为继任的附加环境。这是换手端到端测试的仪表缝(同一测试二进制扮两代,
// 靠它给继任注入 SSHMGR_TEST_VERSION 等覆盖);生产不设此变量,返回 nil。
func successorEnvSeam() []string {
	raw := os.Getenv("SSHMGR_TEST_SUCCESSOR_ENV")
	if raw == "" {
		return nil
	}
	var out []string
	for _, kv := range strings.Split(raw, ";") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// autoSwapBusyClosure 返回自动换手循环(hotswap.ReloadService.StartAutoSwap)
// 的忙判据闭包:直取 serverRuntimes 登记表里本服务的 BusyTracker(与
// reload_self 工具同一登记、同一快照语义——活跃隧道/运行中任务/在飞未答
// 请求的或聚合)。未登记(裸构造的测试服务)返回 nil:循环按永不忙处理。
func autoSwapBusyClosure(srv *mcp.Server) func() bool {
	busy := busyTrackerFor(srv)
	if busy == nil {
		return nil
	}
	return func() bool { return busy.Report().Busy }
}

// applyReloadTimingSeams 应用换手时限的测试缝(env 覆盖;生产不设,默认
// 值见 hotswap.NewReloadService)。
func applyReloadTimingSeams(cfg *hotswap.ReloadConfig) {
	if ms := envMillis("SSHMGR_TEST_READY_TIMEOUT_MS"); ms > 0 {
		cfg.ReadyTimeout = ms
	}
	if ms := envMillis("SSHMGR_TEST_POLL_MS"); ms > 0 {
		cfg.PollInterval = ms
	}
	if ms := envMillis("SSHMGR_TEST_QUIESCE_TIMEOUT_MS"); ms > 0 {
		cfg.QuiesceTimeout = ms
	}
	if ms := envMillis("SSHMGR_TEST_SESSION_END_TIMEOUT_MS"); ms > 0 {
		cfg.SessionEndTimeout = ms
	}
	if ms := envMillis("SSHMGR_TEST_COPIER_DRAIN_TIMEOUT_MS"); ms > 0 {
		cfg.CopierDrainTimeout = ms
	}
	if ms := envMillis("SSHMGR_TEST_AUTOSWAP_INTERVAL_MS"); ms > 0 {
		cfg.AutoSwapInterval = ms
	}
}

func envMillis(name string) time.Duration {
	v, err := strconv.Atoi(os.Getenv(name))
	if err != nil || v <= 0 {
		return 0
	}
	return time.Duration(v) * time.Millisecond
}
