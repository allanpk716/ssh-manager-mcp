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
	"ssh-manager-mcp/internal/snapshotdiff"
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
	srv, tunnels, tasks, err := newLocalVaultServer(st, project.ProfileID, project.ID)
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

// hydrateCacheStore 从快照构建新的临时只读数据库，并在其中校验设备码。
// 调用方负责通过返回值的 Close 方法清理；cacheStoreHolder 会登记每一代。
// 初始启动与每次热重建共用这里，保证两条路径不漂移。device 为空表示未
// 知；它是 ApplyForwardedEntryPin 写入按服务器条目存放的锚时携带的设备码
// 名字（Plan 48 §2.3）。每次换快照都重建临时数据库，元数据按缓存代保存。
func hydrateCacheStore(token string, snap *store.Snapshot, auditFile *os.File, device string) (store.TemporaryClientCacheStore, *models.Project, error) {
	temporary, err := store.OpenTemporaryClientCacheStore(snap, "sshmgr-cache-*.db")
	if err != nil {
		return store.TemporaryClientCacheStore{}, nil, err
	}
	temporary.Store.SetReadOnly(auditFile)   // 必须在导入快照后再设只读
	temporary.Store.SetForwardDevice(device) // Plan 48 §2.3：本地转发锚元数据
	// 快照推荐版本只挂在进程内元数据上：网络拓扑与共享存储工具用它识别
	// 旧权威端快照并按 ADR 0014 报错。
	temporary.Store.SetCacheRecommendedVersion(snap.RecommendedVersion)
	project, err := temporary.Store.VerifyToken(token)
	if err != nil {
		_ = temporary.Close()
		return store.TemporaryClientCacheStore{}, nil, err
	}
	if project == nil {
		_ = temporary.Close()
		return store.TemporaryClientCacheStore{}, nil, fmt.Errorf("invalid or unknown token")
	}
	return temporary, project, nil
}

// cacheStoreHolder owns the hot-reloading read-only store behind mcp --cache.
// Swapped-out stores are NOT closed on swap: the SDK dispatches tool calls on
// separate goroutines, so an in-flight call may still hold the old pointer —
// closing it would surface "sql: database is closed" as a tool error. They are
// registered in temporaryStores and torn down once at process exit instead
// (rebuilds are rare; the leak is bounded and harmless).
type cacheStoreHolder struct {
	reload    func() (*store.Snapshot, bool, error)
	token     string
	auditFile *os.File
	profileID string
	device    string // device-code name for ApplyForwardedEntryPin metadata

	mu       sync.Mutex // serializes rebuilds
	cur      atomic.Pointer[store.Store]
	lastSnap *store.Snapshot // guarded by mu — last snapshot successfully hydrated
	// 变更比较与提示:每次换上新快照时比较新旧快照,产生的
	// 变更提示保存在进程内存里,经会话提示通道按「会话 + 提示文本」
	// 去重。changes 为空时退回默认比较器,保证直接构造 holder 的测试
	// 路径与生产路径走同一套比较。
	changes     *snapshotdiff.Comparer
	changeHints []string // guarded by mu
	// temporaryStores 保存每一代已导入快照的临时客户端缓存；进程退出时统
	// 一关闭数据库并删除数据库文件和附属文件。
	temporaryStores []store.TemporaryClientCacheStore
}

// refresh 是桥唯一的「换上新快照」入口:先询问重新加载回调,变化才
// 解密快照并建出内存数据库。工具调用前的 Current 与后台轮询的 ReloadNow 都走这一
// 条路径,任何一方都不另写替换内存快照的逻辑。Every failure path keeps
// serving the previous store — Lazy revocation semantics: a session outlives
// its token until the next spawn.
func (h *cacheStoreHolder) refresh() *store.Store {
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
	temporary, project, err := hydrateCacheStore(h.token, snap, h.auditFile, h.device)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sshmgr: cache hot-reload failed (keeping current snapshot): %v\n", err)
		return h.cur.Load()
	}
	if project.ProfileID != h.profileID {
		// The owner rebound the project to a different profile mid-session; the
		// tool closures still scope by the startup profileID, so serving the new
		// store would show the wrong set. Keep the old store + log.
		fmt.Fprintf(os.Stderr, "sshmgr: cache snapshot changed the project's profile (keeping current snapshot to preserve scoping)\n")
		_ = temporary.Close()
		return h.cur.Load()
	}
	if hint := effectiveSnapshotComparer(h.changes).Hint(h.lastSnap, snap); hint != "" {
		h.changeHints = append(h.changeHints, hint)
	}
	h.lastSnap = snap
	h.temporaryStores = append(h.temporaryStores, temporary)
	h.cur.Store(temporary.Store) // 旧库刻意保持打开，避免打断仍在执行的调用
	return temporary.Store
}

// Current returns the store to serve THIS tool call from, rebuilding first if
// the reload callback reports a change.
func (h *cacheStoreHolder) Current() *store.Store { return h.refresh() }

// ReloadNow 经由与工具调用相同的统一换快照入口立即热加载一次。供快照修订号
// 后台轮询循环在拉到新快照(或发现共享缓存已被别的桥更新)后安装新快照;不
// 产生任何用户可见输出,失败路径与工具调用路径一致(保留旧快照并记日志)。
func (h *cacheStoreHolder) ReloadNow() { h.refresh() }

// CurrentRevision 返回当前内存快照的快照修订号(旧快照缺该字段时为 0),供
// 后台轮询与远端修订号比较。
func (h *cacheStoreHolder) CurrentRevision() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lastSnap == nil {
		return 0
	}
	return h.lastSnap.SnapshotRevision
}

// cleanup closes every hydrated store and removes every temp db. Called once,
// deferred from RunStdioCache.
//
// Reading temporaryStores without the mutex is safe: cleanup is invoked exactly
// once via defer after srv.Run returns, at which point no in-flight tool calls
// remain to race a rebuild.
func (h *cacheStoreHolder) cleanup() {
	for i := range h.temporaryStores {
		_ = h.temporaryStores[i].Close()
	}
	h.temporaryStores = nil
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
	h := &cacheStoreHolder{reload: reload, token: token, auditFile: af, device: device, changes: snapshotdiff.Default()}
	temporary, project, err := hydrateCacheStore(token, snap, af, device)
	if err != nil {
		return nil, nil, err
	}
	h.profileID = project.ProfileID // set BEFORE the holder is served: Current()'s drift guard depends on it
	h.lastSnap = snap
	h.cur.Store(temporary.Store)
	h.temporaryStores = append(h.temporaryStores, temporary)
	return h, project, nil
}

// changeHint 返回启动以来产生的全部变更提示,按发生顺序排列。提示保存
// 在进程内存里,由会话提示通道按「会话 + 提示文本」去重,每个会话各看到
// 一次;桥在 agent 两次调用之间换上多份新快照时,每条提示都不会丢。
func (h *cacheStoreHolder) changeHint() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.changeHints) == 0 {
		return nil
	}
	return append([]string(nil), h.changeHints...)
}

// versionHint 先刷新当前快照，再比较桥自身版本与快照携带的推荐版本。这里
// 先调用 Current，缓存重载后的第一次工具调用就能报告新版本对，而不是晚一次。
func (h *cacheStoreHolder) versionHint() []string {
	h.Current()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lastSnap == nil {
		return nil
	}
	if text := updater.VersionMismatchHint(hotswap.BridgeVersion(), h.lastSnap.RecommendedVersion); text != "" {
		return []string{text}
	}
	return nil
}

// RunStdioCache hydrates a Snapshot into a temporary read-only store, verifies the SAME
// project token against the cached projects (iron rule + profile scoping intact offline), and
// runs the broker over stdio — identical agent surface to RunStdio. Offline audit lands in
// auditPath (a JSONL sidecar); every mutation is refused (ErrReadOnly). An UNKNOWN host key
// goes through fwdBinder when one is wired (Plan 48 §2.1: the key is forwarded to the broker's
// audited /pin-hostkey endpoint, applied locally, and the handshake continues); with a nil
// binder the pre-Plan-48 behavior stands (SaveEntryPin → ErrReadOnly → fail closed). The temp
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
// forwarded pins (entry_host_keys.pin_device) — metadata only, never load-bearing.
//
// Agent-surface invariant: the broker reads the cache via the exact same
// list_servers / exec_command / download_file / upload_file / forward_port / close_port
// tools, gated by the SAME profile scoping (profileID from the verified project) and attributing
// audit to the SAME project id. The only difference from RunStdio is that the store is read-only
// (mutations refused) and audit is sidecar'd (per-machine, single-direction, zero-merge).
func RunStdioCache(token string, snap *store.Snapshot, auditPath string, reload func() (*store.Snapshot, bool, error), fwdBinder HostKeyStoreBinder, device string, metaEditor MetadataEditor) error {
	return RunStdioCacheWithRefresher(token, snap, auditPath, reload, fwdBinder, device, metaEditor, nil)
}

// CacheRefresher 是桥统一换快照入口的触发器:后台轮询
// 循环用它读取当前内存快照的修订号,并在拉到新快照或发现共享缓存更新后,
// 经由与工具调用完全相同的路径安装新快照。
type CacheRefresher interface {
	// CurrentRevision 返回当前内存快照的快照修订号(旧快照缺该字段时为 0)。
	CurrentRevision() int64
	// ReloadNow 立即经统一换快照入口热加载一次;失败保留旧快照并记日志。
	ReloadNow()
}

// RunStdioCacheWithRefresher 是 RunStdioCache 的扩展入口:桥装配完成后、开始
// 服务前,把统一换快照入口的触发器交给 startBackground;其返回的停止函数在
// 服务结束(宿主断开或热升级交割)后调用,先于桥的清理执行。startBackground
// 为 nil 时行为与 RunStdioCache 完全一致。
func RunStdioCacheWithRefresher(token string, snap *store.Snapshot, auditPath string, reload func() (*store.Snapshot, bool, error), fwdBinder HostKeyStoreBinder, device string, metaEditor MetadataEditor, startBackground func(CacheRefresher) func()) error {
	af, err := os.OpenFile(auditPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	srv, tunnels, tasks, h, err := newCacheBroker(token, snap, af, reload, fwdBinder, device, metaEditor)
	if err != nil {
		af.Close()
		return err
	}
	defer af.Close()
	defer h.cleanup()
	defer tunnels.CloseAll()
	defer tasks.CloseAll()
	if startBackground != nil {
		if stop := startBackground(h); stop != nil {
			// 注册在其余收尾之后:停止顺序为 先停后台循环,再关任务/隧道/桥。
			defer stop()
		}
	}
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
	srv, tunnels, tasks, _, cleanup, err := NewCacheBrokerWithRefresher(token, snap, auditPath, reload, fwdBinder, device, metaEditor)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return srv, tunnels, tasks, cleanup, nil
}

// NewCacheBrokerWithRefresher 按 NewCacheBroker 的同一装配构建桥,并额外交出
// 统一换快照入口的触发器:端到端测试用它驱动后台轮询循环(短间隔注入),
// `mcp --cache` 的生产路径则经 RunStdioCacheWithRefresher 拿到同一个触发器。
// cleanup 的契约与 NewCacheBroker 相同。
func NewCacheBrokerWithRefresher(token string, snap *store.Snapshot, auditPath string, reload func() (*store.Snapshot, bool, error), fwdBinder HostKeyStoreBinder, device string, metaEditor MetadataEditor) (*mcp.Server, *TunnelManager, *TaskManager, CacheRefresher, func(), error) {
	af, err := os.OpenFile(auditPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	srv, tunnels, tasks, h, err := newCacheBroker(token, snap, af, reload, fwdBinder, device, metaEditor)
	if err != nil {
		af.Close()
		return nil, nil, nil, nil, nil, err
	}
	return srv, tunnels, tasks, h, func() { h.cleanup(); af.Close() }, nil
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
	// 变更提示来源注册在版本提示来源之前:两种提示同时出现时,变更
	// 提示排在前面。
	srv, tunnels, tasks, err := NewServerFromSourceWithHints(h.Current, project.ProfileID, project.ID, hkFn, metaEditor, h.changeHint, h.versionHint)
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
