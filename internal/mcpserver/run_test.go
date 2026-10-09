package mcpserver

// Plan 48 §2.2 wiring — pins that the RunStdioCache host-key store binder
// actually reaches the TOFU path (the injection seam of NewServerFromSource),
// and that the per-generation forward-device metadata rides every hydration
// (§2.3). These tests stay inside the mcpserver package with a LOCAL fake:
// the forwarding wrapper itself is clientops's (clientops imports mcpserver,
// so this package's tests cannot import clientops) — the wrapper behavior is
// covered there, and the full e2e in cli (the package where clientops and
// mcpserver meet).

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/mcpserver/hotswap"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
	"ssh-manager-mcp/internal/updater"
)

// recordingHK wraps the current store and counts TOFU reads — the observable
// that proves the binder's HostKeyStore (not the raw store) serves the TOFU
// path.
type recordingHK struct {
	cur  func() *store.Store
	gets *int32
}

func (r *recordingHK) GetHostKey(host string, port int) (*store.Pin, error) {
	atomic.AddInt32(r.gets, 1)
	return r.cur().GetHostKey(host, port)
}

func (r *recordingHK) SaveHostKey(host string, port int, marshaledKey []byte) error {
	return r.cur().SaveHostKey(host, port, marshaledKey)
}

// seedTofuTargetSnap builds a snapshot with one granted server pointing at a
// REAL testsshd and NO pinned key: the handshake reaches the TOFU callback
// (the injection under test), which then refuses the unknown key on the
// read-only store. Returns (snapshot, token, serverID).
func seedTofuTargetSnap(t *testing.T, addr string) (*store.Snapshot, string, string) {
	t.Helper()
	st := newStore(t)
	cid, err := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.AddServer(&models.Server{
		Name: "target", Host: addr[:indexByte(addr, ':')], Port: portOfAddr(addr),
		User: "u", AuthMethod: models.AuthPassword, CredentialID: cid,
	})
	if err != nil {
		t.Fatal(err)
	}
	pid, err := st.AddProfile("p")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(pid, []string{id}); err != nil {
		t.Fatal(err)
	}
	_, token, err := st.AddProject("proj", pid)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := st.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	return snap, token, id
}

// TestRunStdioCacheBinder_ReachesTOFU pins the injection seam: with a binder
// wired, the TOFU callback consults the BINDER's host-key store (the read
// counter increments); with a nil binder the raw store serves (counter stays
// zero because the raw store's reads cannot be observed — the assertion is
// the positive case). A regression that drops the hkFn threading would leave
// the override unwired and the counter at zero.
func TestRunStdioCacheBinder_ReachesTOFU(t *testing.T) {
	addr, _, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	snap, token, serverID := seedTofuTargetSnap(t, addr)
	auditPath := filepath.Join(t.TempDir(), "audit.log")

	var gets int32
	binder := func(cur func() *store.Store) sshbroker.HostKeyStore {
		return &recordingHK{cur: cur, gets: &gets}
	}

	srv, tunnels, tasks, cleanup2, err := NewCacheBroker(token, snap, auditPath, nil, binder, "laptop", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup2)
	t.Cleanup(func() { tunnels.CloseAll(); tasks.CloseAll() })

	client := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "v0"}, nil)
	t1, t2 := mcp.NewInMemoryTransports()
	srvSess, err := srv.Connect(context.Background(), t1, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srvSess.Close() })
	cliSess, err := client.Connect(context.Background(), t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cliSess.Close() })

	res, err := cliSess.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      BrokerTools[1], // exec_command
		Arguments: map[string]any{"server_id": serverID, "command": "true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("exec without a pinned key on a read-only store must fail closed")
	}
	if n := atomic.LoadInt32(&gets); n == 0 {
		t.Fatal("the TOFU path never consulted the binder's host-key store — the NewServerFromSource injection is unwired")
	}
}

// TestHydrateCacheStore_ForwardDevicePerGeneration pins §2.3's metadata
// wiring: EVERY hydrated generation carries the device name given to the
// holder constructor, so ApplyForwardedHostKey stamps host_keys.pin_device
// with it — not just the initial hydration (hot rebuilds hydrate fresh temp
// dbs that would otherwise lose the metadata).
func TestHydrateCacheStore_ForwardDevicePerGeneration(t *testing.T) {
	addr, _, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	snap, token, _ := seedTofuTargetSnap(t, addr)
	af, err := os.OpenFile(filepath.Join(t.TempDir(), "audit.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { af.Close() })

	h, _, err := newCacheStoreHolderFromSnapshot(token, snap, af, nil, "laptop-7")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.cleanup)

	// Initial generation: the applied pin must carry the device name.
	gen1 := h.Current()
	if err := gen1.ApplyForwardedHostKey("10.1.1.1", 22, []byte("key-bytes")); err != nil {
		t.Fatal(err)
	}
	if got := rawForwardDevice(t, h.tmpPaths[0], "10.1.1.1:22"); got != "laptop-7" {
		t.Fatalf("generation 1 pin_device = %q, want laptop-7", got)
	}

	// A hot rebuild hydrates a FRESH temp db from a FRESH snapshot of the SAME
	// store (same project token) — the metadata must survive it.
	calls := 0
	h.reload = func() (*store.Snapshot, bool, error) {
		calls++
		if calls == 1 {
			fresh, err := reseedSnap(t, snap)
			if err != nil {
				return nil, false, err
			}
			return fresh, true, nil
		}
		return nil, false, nil
	}
	gen2 := h.Current()
	if gen2 == gen1 {
		t.Fatal("fixture self-check: the reload must swap generations")
	}
	if err := gen2.ApplyForwardedHostKey("10.1.1.2", 22, []byte("key-bytes")); err != nil {
		t.Fatal(err)
	}
	if got := rawForwardDevice(t, h.tmpPaths[1], "10.1.1.2:22"); got != "laptop-7" {
		t.Fatalf("generation 2 pin_device = %q, want laptop-7 (per-generation metadata)", got)
	}
}

// reseedSnap re-imports snap into a fresh temp db — a fresh store carrying the
// same project token, i.e. a genuine reload candidate for the holder.
func reseedSnap(t *testing.T, snap *store.Snapshot) (*store.Snapshot, error) {
	t.Helper()
	dup := *snap
	dup.Servers = append([]store.SnapshotServer{}, snap.Servers...)
	return &dup, nil
}

// rawForwardDevice reads host_keys.pin_device straight from a hydrated temp db.
func rawForwardDevice(t *testing.T, dbPath, hostPort string) string {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var dev string
	if err := db.QueryRow(`SELECT pin_device FROM host_keys WHERE host_port=?`, hostPort).Scan(&dev); err != nil {
		t.Fatalf("pin row for %s: %v", hostPort, err)
	}
	return dev
}

// assertArgsProbe 断言某代角色进程的命令行探针携带期望参数(os.Args[1:]
// 与 want 逐一相等;[0] 是可执行路径本身,不做字符串比较——Windows 上
// GetModuleFileName 的解析形态可能与父进程传入字面量差个大小写)。
func assertArgsProbe(t *testing.T, dir, version string, want []string) {
	t.Helper()
	waitForStateProbe(t, dir, "args", version, 15*time.Second)
	raw, err := os.ReadFile(stateProbePath(dir, "args", version))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("args probe %q not parseable: %v (%s)", version, err, raw)
	}
	if len(got) < 1 {
		t.Fatalf("args probe %q must at least carry the executable path: %s", version, raw)
	}
	if !slices.Equal(got[1:], want) {
		t.Fatalf("args probe %q: argv[1:] = %q, want %q (successor must inherit the original command line)", version, got[1:], want)
	}
}

// TestAutoSwapSuccessorInheritsArgs — 生产阻断缺陷的防回归(票 06 内修复):
// 继任必须以「同一可执行 + 原始命令行」拉起(生产形态 `sshmgr mcp --cache
// --instance <名>`)——若以裸二进制拉起,真机形态的继任不成桥,就绪超时
// 必然回退,热升级在真机不可用。角色进程显式携带 argv,各代探针回显自身
// os.Args,断言继任(经自动换手)的命令行参数与首代完全一致。
func TestAutoSwapSuccessorInheritsArgs(t *testing.T) {
	storePath, masterHex, token := buildTestVault(t)
	stateDir := t.TempDir()
	argv := []string{"mcp", "--cache", "--instance", "laptop-args"}
	h := spawnBridgeRoleWithArgs(t, roleBridgeDirect, map[string]string{
		"SSHMGR_STORE":                     storePath,
		"SSHMGR_MASTERKEY_HEX":             masterHex,
		"SSHMGR_TEST_TOKEN":                token,
		"SSHMGR_TEST_STATE_DIR":            stateDir,
		"SSHMGR_TEST_VERSION":              "v1-argv",
		"SSHMGR_TEST_SUCCESSOR_ENV":        "SSHMGR_TEST_VERSION=v2-argv",
		"SSHMGR_TEST_AUTOSWAP_INTERVAL_MS": "300",
	}, argv)
	h.handshake(t)
	base := autoSwapNotifBase(t, h)

	// 首代自身的命令行探针:确证观测机制(显式 argv 原样落盘)。
	assertArgsProbe(t, stateDir, "v1-argv", argv)

	// 落新代际信号 → 自动换手;继任的命令行探针必须携带同一组参数。
	exe := testBinaryPath(t)
	t.Cleanup(func() { os.Remove(updater.SignalPath(exe)) })
	if _, err := updater.WriteGenerationSignal(exe, "v2-argv-disk"); err != nil {
		t.Fatal(err)
	}
	awaitNotifAbove(t, h, "notifications/tools/list_changed", base, 15*time.Second)
	assertArgsProbe(t, stateDir, "v2-argv", argv)

	// 继任接管同一会话:自报 v2-argv 应答(换手确实完成)。
	out := h.callReloadSelf(t, 3, 30*time.Second)
	if out.Version != "v2-argv" {
		t.Fatalf("successor must answer with v2-argv, got %+v (stderr:\n%s)", out, h.stderrFn())
	}

	// 宿主断开:链路干净退出。
	h.stdinW.Close()
	done := make(chan error, 1)
	go func() { done <- h.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatalf("bridge did not exit after host disconnect; stderr:\n%s", h.stderrFn())
	}
}

// --- 票 06:自动换手循环的接线与端到端(run.go serveBridge 的对应测试)。 ---

// TestAutoSwapBusyClosure pins the serveBridge busy wiring for the auto-swap
// loop: the closure comes from the SAME registry entry reload_self uses
// (serverRuntimes via busyTrackerFor) and reads the BusyTracker's or-aggregated
// snapshot; an unregistered bare server degrades to nil (never busy).
func TestAutoSwapBusyClosure(t *testing.T) {
	bare := mcp.NewServer(&mcp.Implementation{Name: "bare", Version: "0"}, nil)
	if c := autoSwapBusyClosure(bare); c != nil {
		t.Fatal("unregistered server must degrade to a nil busy closure")
	}

	var tunnels atomic.Int32
	srv := mcp.NewServer(&mcp.Implementation{Name: "reg", Version: "0"}, nil)
	serverRuntimes.Store(srv, NewBusyTracker(func() int { return int(tunnels.Load()) }, func() int { return 0 }))
	t.Cleanup(func() { serverRuntimes.Delete(srv) })

	c := autoSwapBusyClosure(srv)
	if c == nil {
		t.Fatal("registered server must yield a busy closure")
	}
	if c() {
		t.Fatal("idle bridge (0 tunnels, 0 tasks, 0 in-flight) must report not busy")
	}
	tunnels.Store(1)
	if !c() {
		t.Fatal("one active tunnel must report busy (or-aggregated snapshot)")
	}
}

// TestAutoSwapTimingSeam pins the SSHMGR_TEST_AUTOSWAP_INTERVAL_MS seam: the
// auto-swap poll interval is injectable for the e2e tests below (production
// default stays ~30s inside the hotswap package).
func TestAutoSwapTimingSeam(t *testing.T) {
	t.Setenv("SSHMGR_TEST_AUTOSWAP_INTERVAL_MS", "250")
	var cfg hotswap.ReloadConfig
	applyReloadTimingSeams(&cfg)
	if cfg.AutoSwapInterval != 250*time.Millisecond {
		t.Fatalf("AutoSwapInterval = %v want 250ms", cfg.AutoSwapInterval)
	}
	// 非法值:不碰配置(0 交由 NewReloadService 填默认)。
	t.Setenv("SSHMGR_TEST_AUTOSWAP_INTERVAL_MS", "not-a-number")
	cfg = hotswap.ReloadConfig{}
	applyReloadTimingSeams(&cfg)
	if cfg.AutoSwapInterval != 0 {
		t.Fatalf("invalid seam value must leave the config untouched, got %v", cfg.AutoSwapInterval)
	}
}

// countNotifs 统计伪宿主已记录的某方法通知数。
func countNotifs(h *fakeHost, method string) int {
	n := 0
	for _, w := range h.frames() {
		if w.method == method {
			n++
		}
	}
	return n
}

// autoSwapNotifBase 让「与换手无关」的清单变更通知先尘埃落定,并返回当前
// 计数作基线。SDK 的 changeAndNotify 带 10ms 去抖延迟:serveBridge 启动时的
// 首次工具注册,若会话在延迟窗口内已连上,也会向宿主发一次
// tools/list_changed——先等这笔早到的通知落地,此后计数再增加才能归因于
// 换手继任的接管注册。
func autoSwapNotifBase(t *testing.T, h *fakeHost) int {
	t.Helper()
	time.Sleep(250 * time.Millisecond)
	return countNotifs(h, "notifications/tools/list_changed")
}

// awaitNotifAbove 等待某方法的通知计数超过 base(至少 +1)。继任接管时的
// 重注册经 10ms 去抖可能合一或分立成两笔,故不锁死精确值。
func awaitNotifAbove(t *testing.T, h *fakeHost, method string, base int, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		if countNotifs(h, method) > base {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("notification %s count did not exceed %d within %v; bridge stderr:\n%s", method, base, d, h.stderrFn())
		case <-h.wakeup:
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// TestAutoSwapE2EIdle — 伪宿主挂机:桥空闲、盘上落一个较出生代际新的信号,
// 此后不调用任何工具,一个轮询周期(测试缝 300ms)内自动完成换手;宿主侧
// 收到继任补发的清单变更通知,继任自报新版本应答 reload_self,自动换手全程
// 无失败输出(成功路径零输出)。
func TestAutoSwapE2EIdle(t *testing.T) {
	storePath, masterHex, token := buildTestVault(t)
	h := spawnBridgeRole(t, roleBridgeDirect, map[string]string{
		"SSHMGR_STORE":                     storePath,
		"SSHMGR_MASTERKEY_HEX":             masterHex,
		"SSHMGR_TEST_TOKEN":                token,
		"SSHMGR_TEST_VERSION":              "v1-idle",
		"SSHMGR_TEST_SUCCESSOR_ENV":        "SSHMGR_TEST_VERSION=v2-idle",
		"SSHMGR_TEST_AUTOSWAP_INTERVAL_MS": "300",
	})
	h.handshake(t)
	if names := h.listToolNames(t, 2); len(names) != len(authorityTools)+1 {
		t.Fatalf("bridge face = %d tools, want %d: %v", len(names), len(authorityTools)+1, names)
	}
	base := autoSwapNotifBase(t, h)

	// 桥已启动(出生代际已快照):落新代际信号——此后唯一的动作是等待。
	exe := testBinaryPath(t)
	t.Cleanup(func() { os.Remove(updater.SignalPath(exe)) })
	if _, err := updater.WriteGenerationSignal(exe, "v2-idle-disk"); err != nil {
		t.Fatal(err)
	}

	// 一个轮询周期内自动完成换手:继任接管后补发的清单变更通知到达宿主。
	awaitNotifAbove(t, h, "notifications/tools/list_changed", base, 15*time.Second)

	// 继任接管同一会话:reload_self 由继任应答,自报 v2-idle;盘上无更新
	// 代际(继任的出生代际=刚落的信号),如实报 no_new_generation。
	out := h.callReloadSelf(t, 3, 30*time.Second)
	if out.Version != "v2-idle" {
		t.Fatalf("successor must answer with v2-idle, got %+v (stderr:\n%s)", out, h.stderrFn())
	}
	if out.Handover != "no_new_generation" {
		t.Fatalf("successor handover = %q want no_new_generation", out.Handover)
	}
	assertFrameIntegrity(t, h, []int64{1, 2, 3})

	// 自动换手静默约束:无自动换手失败日志(成功路径零输出,忙路径未触发)。
	if s := h.stderrFn(); strings.Contains(s, "auto handover attempt failed") {
		t.Fatalf("idle auto handover must not log failures:\n%s", s)
	}

	// 宿主断开:链路干净退出。
	h.stdinW.Close()
	done := make(chan error, 1)
	go func() { done <- h.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatalf("bridge did not exit after host disconnect; stderr:\n%s", h.stderrFn())
	}
}

// TestAutoSwapE2EDualInstance — 多实例独立性(同机两把桥,直连+缓存形态
// 组合,共用同一份测试二进制=共用同一个代际信号文件):各自记出生代际、
// 各自换血。先后落两个代际信号,断言每轮两把桥各自换代成功(各自自报专属
// 的继任版本——换手环境没有串),会话不掉线、帧序完整、无自动换手失败。
func TestAutoSwapE2EDualInstance(t *testing.T) {
	storePath, masterHex, tokenA := buildTestVault(t)
	snapPath, auditPath, tokenB := buildTestSnapshotFile(t)
	// 继任环境逐代链式(v1→v2→v3,同键后值覆盖前值,见 BuildEnv):每代的
	// 继任自报版本唯一,「换成谁」可观测。
	const chainA = "SSHMGR_TEST_VERSION=v2-a;SSHMGR_TEST_SUCCESSOR_ENV=SSHMGR_TEST_VERSION=v3-a"
	const chainB = "SSHMGR_TEST_VERSION=v2-b;SSHMGR_TEST_SUCCESSOR_ENV=SSHMGR_TEST_VERSION=v3-b"
	hA := spawnBridgeRole(t, roleBridgeDirect, map[string]string{
		"SSHMGR_STORE":                     storePath,
		"SSHMGR_MASTERKEY_HEX":             masterHex,
		"SSHMGR_TEST_TOKEN":                tokenA,
		"SSHMGR_TEST_VERSION":              "v1-a",
		"SSHMGR_TEST_SUCCESSOR_ENV":        chainA,
		"SSHMGR_TEST_AUTOSWAP_INTERVAL_MS": "300",
	})
	hB := spawnBridgeRole(t, roleBridgeCache, map[string]string{
		"SSHMGR_TEST_SNAPSHOT":             snapPath,
		"SSHMGR_TEST_AUDIT":                auditPath,
		"SSHMGR_TEST_TOKEN":                tokenB,
		"SSHMGR_TEST_VERSION":              "v1-b",
		"SSHMGR_TEST_SUCCESSOR_ENV":        chainB,
		"SSHMGR_TEST_AUTOSWAP_INTERVAL_MS": "300",
	})
	hA.handshake(t)
	hB.handshake(t)
	for _, h := range []*fakeHost{hA, hB} {
		if names := h.listToolNames(t, 2); len(names) != len(authorityTools)+1 {
			t.Fatalf("bridge face = %d tools, want %d: %v", len(names), len(authorityTools)+1, names)
		}
	}
	baseA := autoSwapNotifBase(t, hA)
	baseB := autoSwapNotifBase(t, hB)

	exe := testBinaryPath(t)
	t.Cleanup(func() { os.Remove(updater.SignalPath(exe)) })

	// 第一个代际信号:两把桥各自自动换到自己的继任(A→v2-a,B→v2-b)。
	// 过渡窗内的 reload_self 可能被退位中的旧代完整应答(retireLater 文档
	// 化的双读者过渡窗边界),经 callReloadSelfRetry 有界重试到继任版本。
	if _, err := updater.WriteGenerationSignal(exe, "v2-disk"); err != nil {
		t.Fatal(err)
	}
	awaitNotifAbove(t, hA, "notifications/tools/list_changed", baseA, 20*time.Second)
	awaitNotifAbove(t, hB, "notifications/tools/list_changed", baseB, 20*time.Second)
	nextA, nextB := int64(3), int64(3)
	outA, idsA := hA.callReloadSelfRetry(t, &nextA, "v2-a", 3)
	outB, idsB := hB.callReloadSelfRetry(t, &nextB, "v2-b", 3)
	if outA.Version != "v2-a" || outB.Version != "v2-b" {
		t.Fatalf("round 1 successors: A=%q want v2-a, B=%q want v2-b (A stderr:\n%s\nB stderr:\n%s)",
			outA.Version, outB.Version, hA.stderrFn(), hB.stderrFn())
	}

	// 第二个代际信号:被领养的二代们(票 05 形态)照常自动换手(A→v3-a,
	// B→v3-b)——先后两个信号,各链各换两次,互不串扰。
	baseA, baseB = autoSwapNotifBase(t, hA), autoSwapNotifBase(t, hB)
	if _, err := updater.WriteGenerationSignal(exe, "v3-disk"); err != nil {
		t.Fatal(err)
	}
	awaitNotifAbove(t, hA, "notifications/tools/list_changed", baseA, 20*time.Second)
	awaitNotifAbove(t, hB, "notifications/tools/list_changed", baseB, 20*time.Second)
	outA2, idsA2 := hA.callReloadSelfRetry(t, &nextA, "v3-a", 3)
	outB2, idsB2 := hB.callReloadSelfRetry(t, &nextB, "v3-b", 3)
	idsA = append(idsA, idsA2...)
	idsB = append(idsB, idsB2...)
	if outA2.Version != "v3-a" || outB2.Version != "v3-b" {
		t.Fatalf("round 2 successors: A=%q want v3-a, B=%q want v3-b (A stderr:\n%s\nB stderr:\n%s)",
			outA2.Version, outB2.Version, hA.stderrFn(), hB.stderrFn())
	}

	// 互不串扰的收尾证据:两把桥会话都活着、工具面完整、帧序无损、无自动
	// 换手失败日志。
	if names := hA.listToolNames(t, nextA); len(names) != len(authorityTools)+1 {
		t.Fatalf("final face (A) = %d tools, want %d", len(names), len(authorityTools)+1)
	}
	if names := hB.listToolNames(t, nextB); len(names) != len(authorityTools)+1 {
		t.Fatalf("final face (B) = %d tools, want %d", len(names), len(authorityTools)+1)
	}
	for _, h := range []*fakeHost{hA, hB} {
		if s := h.stderrFn(); strings.Contains(s, "auto handover attempt failed") {
			t.Fatalf("auto handovers must not log failures:\n%s", s)
		}
	}
	assertFrameIntegrity(t, hA, append([]int64{1, 2, nextA}, idsA...))
	assertFrameIntegrity(t, hB, append([]int64{1, 2, nextB}, idsB...))

	// 断开两把桥:各自整链(首代泵→三代)干净退出。
	for _, h := range []*fakeHost{hA, hB} {
		h.stdinW.Close()
	}
	for _, h := range []*fakeHost{hA, hB} {
		done := make(chan error, 1)
		host := h
		go func() { done <- host.cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Fatalf("bridge did not exit after host disconnect; stderr:\n%s", host.stderrFn())
		}
	}
}
