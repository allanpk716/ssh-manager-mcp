package mcpserver

// reload_e2e_test.go — 换手端到端(伪宿主,黑盒,规格 Testing Decisions):
// 测试二进制自我再执行扮演桥(直连/缓存两形态),进程外伪宿主走 JSON-RPC
// 帧。覆盖:首代换手全链(应答写回 F5、泵存活、继任应答后续请求且自报新
// 版本、tools/list_changed 通知)、继任不可用回退、GitHub 查询失败字段。
//
// 角色分支经 SSHMGR_TEST_ROLE 进入(先例:internal/cli/multiinstance_
// e2e_test.go、internal/mcpserver/hotswap/main_test.go)。

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ssh-manager-mcp/internal/mcpserver/hotswap"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/updater"
)

const reloadRoleEnv = "SSHMGR_TEST_ROLE"

const (
	roleBridgeDirect = "reload_bridge_direct" // 直连形态桥(RunStdio)
	roleBridgeCache  = "reload_bridge_cache"  // 缓存形态桥(RunStdioCache)
	roleNeverReady   = "reload_neverready"    // 永不写就绪文件的继任
)

// TestMain 拦截角色分支:角色环境存在时不跑测试,进入对应角色。
func TestMain(m *testing.M) {
	switch os.Getenv(reloadRoleEnv) {
	case "":
		os.Exit(m.Run())
	case roleBridgeDirect:
		os.Exit(roleBridgeMain(roleBridgeDirect))
	case roleBridgeCache:
		os.Exit(roleBridgeMain(roleBridgeCache))
	case roleNeverReady:
		time.Sleep(90 * time.Second) // 永不就绪(父侧超时后会杀掉本进程)
		os.Exit(0)
	}
	fmt.Fprintf(os.Stderr, "reload e2e: unknown role %q\n", os.Getenv(reloadRoleEnv))
	os.Exit(2)
}

// roleBridgeMain 两种桥形态的公共角色体:按角色读各自的输入,起桥服务。
// 测试仪表缝(可选,不设即无行为):SSHMGR_TEST_STATE_DIR 存在时,角色在
// 起服务前把自身进程号写进 <dir>/pid-<版本>,在进程收尾时写 <dir>/exit-
// <版本>(版本取 SSHMGR_TEST_VERSION;换手链测试靠它逐代观测「谁还活着/
// 谁已退出」——链上每代自报版本唯一),并把完整命令行 os.Args 回显进
// <dir>/args-<版本>(票 06:继任必须继承原始命令行参数,探针是防回归的
// 观测点)。
func roleBridgeMain(role string) int {
	token := os.Getenv("SSHMGR_TEST_TOKEN")
	writeStateProbe("pid", os.Getpid())
	writeArgsProbe()
	defer writeStateProbe("exit", 0)
	switch role {
	case roleBridgeDirect:
		// 库:vault 走 env 缝(SSHMGR_STORE + SSHMGR_MASTERKEY_HEX,kp=nil
		// 跳过注入层)。
		if err := RunStdio(token, nil); err != nil {
			fmt.Fprintf(os.Stderr, "bridge-direct role: %v\n", err)
			return 3
		}
	case roleBridgeCache:
		raw, err := os.ReadFile(os.Getenv("SSHMGR_TEST_SNAPSHOT"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "bridge-cache role: read snapshot: %v\n", err)
			return 3
		}
		var snap store.Snapshot
		if err := json.Unmarshal(raw, &snap); err != nil {
			fmt.Fprintf(os.Stderr, "bridge-cache role: decode snapshot: %v\n", err)
			return 3
		}
		if err := RunStdioCache(token, &snap, os.Getenv("SSHMGR_TEST_AUDIT"), nil, nil, "reload-e2e", nil); err != nil {
			fmt.Fprintf(os.Stderr, "bridge-cache role: %v\n", err)
			return 3
		}
	}
	return 0
}

// writeStateProbe 写角色状态探针文件(pid/exit);未配置 SSHMGR_TEST_STATE_DIR
// 时是无操作。exit 的内容无意义,存在性即信号。
func writeStateProbe(kind string, content int) {
	dir := os.Getenv("SSHMGR_TEST_STATE_DIR")
	ver := os.Getenv("SSHMGR_TEST_VERSION")
	if dir == "" || ver == "" {
		return
	}
	name := fmt.Sprintf("%s-%s", kind, ver)
	if kind == "pid" {
		os.WriteFile(filepath.Join(dir, name), []byte(strconv.Itoa(content)), 0o600)
	} else {
		os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600)
	}
}

// writeArgsProbe 把本进程的完整命令行(os.Args,JSON 数组)回显进
// <dir>/args-<版本>;未配置 SSHMGR_TEST_STATE_DIR 时无操作。换手继任是否
// 以「同一可执行+原始命令行」拉起的观测点(票 06 生产阻断缺陷的防回归)。
func writeArgsProbe() {
	dir := os.Getenv("SSHMGR_TEST_STATE_DIR")
	ver := os.Getenv("SSHMGR_TEST_VERSION")
	if dir == "" || ver == "" {
		return
	}
	raw, err := json.Marshal(os.Args)
	if err != nil {
		return
	}
	os.WriteFile(filepath.Join(dir, fmt.Sprintf("args-%s", ver)), raw, 0o600)
}

func testBinaryPath(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// buildTestVault 建一个含 profile+project 的临时 vault,返回库路径、主密钥
// 十六进制与项目 token(直连形态桥的角色凭 env 缝打开它)。
func buildTestVault(t *testing.T) (storePath, masterHex, token string) {
	t.Helper()
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	storePath = filepath.Join(t.TempDir(), "vault.db")
	st, err := store.Open(storePath, mk)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	pid, err := st.AddProfile("reload-e2e")
	if err != nil {
		t.Fatal(err)
	}
	_, token, err = st.AddProject("proj", pid)
	if err != nil {
		t.Fatal(err)
	}
	return storePath, hex.EncodeToString(mk), token
}

// buildTestSnapshotFile 建同构的临时 vault 并导出快照,落盘供缓存形态桥
// 的角色读取;返回快照文件路径、审计文件路径与 token。
func buildTestSnapshotFile(t *testing.T) (snapPath, auditPath, token string) {
	t.Helper()
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "vault.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	pid, err := st.AddProfile("reload-e2e")
	if err != nil {
		t.Fatal(err)
	}
	_, token, err = st.AddProject("proj", pid)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := st.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	snapPath = filepath.Join(t.TempDir(), "snapshot.json")
	if err := os.WriteFile(snapPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	auditPath = filepath.Join(t.TempDir(), "audit.jsonl")
	return snapPath, auditPath, token
}

// wireMsg 是伪宿主收到的一帧(JSON-RPC 消息,按行到达)。
type wireMsg struct {
	raw    string
	id     *int64
	method string // 非空 = 通知(服务端不发请求)
	resp   bool   // 带结果或错误的应答
	result json.RawMessage
	errMsg string
}

// fakeHost 是进程外伪宿主:持有桥进程的标准输入输出管道,记录每一帧。
type fakeHost struct {
	cmd    *exec.Cmd
	stdinW *os.File

	mu       sync.Mutex
	seen     []wireMsg
	wakeup   chan struct{}
	stderrFn func() string

	stdoutEOF    chan struct{} // 桥侧标准输出断开(读循环 EOF)时关闭,一次
	stdoutEOFOne sync.Once
}

// spawnBridgeRole 拉起一个桥角色进程(无额外命令行参数——测试角色经环境
// 变量选定,argv[1:] 为空)并开始读它的标准输出。
func spawnBridgeRole(t *testing.T, role string, extraEnv map[string]string) *fakeHost {
	return spawnBridgeRoleWithArgs(t, role, extraEnv, nil)
}

// spawnBridgeRoleWithArgs 是 spawnBridgeRole 的显式命令行形态:argv 原样传给
// 角色进程(角色分支经 TestMain 在环境变量上分派,argv 不进 cobra,仅由
// serveBridge 的继任装配继承)——票 06 继任继承原始命令行的 e2e 用它。
func spawnBridgeRoleWithArgs(t *testing.T, role string, extraEnv map[string]string, args []string) *fakeHost {
	t.Helper()
	exe := testBinaryPath(t)
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrPath := filepath.Join(t.TempDir(), "stderr.log")
	stderrF, err := os.Create(stderrPath)
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(exe, args...)
	cmd.Stdin = stdinR
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrF
	// 继承完整环境(TMP 等系统变量缺失会让角色的临时文件落进不可写的
	// 系统目录),再叠加角色与仪表变量。
	env := append(os.Environ(),
		reloadRoleEnv+"="+role,
		// GitHub 查询缝:指向必拒的环回 → latest 字段带错误(不联网)。
		"SSHMGR_UPDATE_BASE=http://127.0.0.1:1",
		// 换手时限缝:失败场景等不起 15s/6min 默认值。
		"SSHMGR_TEST_READY_TIMEOUT_MS=10000",
		"SSHMGR_TEST_POLL_MS=20",
		"SSHMGR_TEST_QUIESCE_TIMEOUT_MS=30000",
		"SSHMGR_TEST_SESSION_END_TIMEOUT_MS=8000",
		"SSHMGR_TEST_COPIER_DRAIN_TIMEOUT_MS=8000",
	)
	for k, v := range extraEnv {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stdinR.Close()
	stdoutW.Close()

	h := &fakeHost{cmd: cmd, stdinW: stdinW, wakeup: make(chan struct{}, 1),
		stdoutEOF: make(chan struct{}),
		stderrFn: func() string {
			b, _ := os.ReadFile(stderrPath)
			return string(b)
		}}
	t.Cleanup(func() {
		stdinW.Close()
		stderrF.Close()
		// 兜底:任何断言路径都不留孤儿桥进程。
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		cmd.Wait()
	})
	go h.readLoop(stdoutR)
	return h
}

func (h *fakeHost) readLoop(r *os.File) {
	rd := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := rd.ReadString('\n')
		if line != "" {
			h.record(line)
		}
		if err != nil {
			h.stdoutEOFOne.Do(func() { close(h.stdoutEOF) })
			return
		}
	}
}

// frames 返回已记录帧的快照。
func (h *fakeHost) frames() []wireMsg {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]wireMsg, len(h.seen))
	copy(out, h.seen)
	return out
}

// awaitNotificationCount 等到第 n 个 method 通知到达(链式换手中继任会
// 各发一次 tools/list_changed,靠计数区分代次)。独立轮询,不走 wait 的
// 持锁回调(cond 内不得再取帧锁)。
func (h *fakeHost) awaitNotificationCount(t *testing.T, method string, n int, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		h.mu.Lock()
		count := 0
		for _, w := range h.seen {
			if w.method == method {
				count++
			}
		}
		h.mu.Unlock()
		if count >= n {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("%d x notification %s: not seen within %v; bridge stderr:\n%s", n, method, d, h.stderrFn())
		case <-h.wakeup:
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// stateProbePath 是角色状态探针文件的路径(见 roleBridgeMain)。
func stateProbePath(dir, kind, version string) string {
	return filepath.Join(dir, fmt.Sprintf("%s-%s", kind, version))
}

func (h *fakeHost) record(line string) {
	var w wireMsg
	w.raw = line
	var probe struct {
		ID     *int64          `json:"id"`
		Method string          `json:"method"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &probe); err == nil {
		w.id = probe.ID
		w.method = probe.Method
		w.result = probe.Result
		if probe.Error != nil {
			w.resp = true
			w.errMsg = probe.Error.Message
		} else if probe.ID != nil && probe.Method == "" {
			w.resp = true
		}
	}
	h.mu.Lock()
	h.seen = append(h.seen, w)
	h.mu.Unlock()
	select {
	case h.wakeup <- struct{}{}:
	default:
	}
}

// sendReq 发一个带 ID 的请求。
func (h *fakeHost) sendReq(id int64, method string, params any) {
	tmsg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
	b, _ := json.Marshal(tmsg)
	h.stdinW.Write(append(b, '\n'))
}

func (h *fakeHost) notify(method string) {
	tmsg := map[string]any{"jsonrpc": "2.0", "method": method}
	b, _ := json.Marshal(tmsg)
	h.stdinW.Write(append(b, '\n'))
}

// wait 等第一帧满足 cond(按到达顺序扫描全部已记录帧)。
func (h *fakeHost) wait(t *testing.T, what string, d time.Duration, cond func(wireMsg) bool) wireMsg {
	t.Helper()
	deadline := time.After(d)
	for {
		h.mu.Lock()
		for _, w := range h.seen {
			if cond(w) {
				h.mu.Unlock()
				return w
			}
		}
		h.mu.Unlock()
		select {
		case <-deadline:
			var sb strings.Builder
			for _, w := range h.frames() {
				sb.WriteString(w.raw)
			}
			t.Fatalf("%s: not seen within %v; seen frames:\n%s\nbridge stderr:\n%s", what, d, sb.String(), h.stderrFn())
			return wireMsg{}
		case <-h.wakeup:
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (h *fakeHost) awaitResponse(t *testing.T, id int64, d time.Duration) wireMsg {
	t.Helper()
	return h.wait(t, fmt.Sprintf("response for id %d", id), d, func(w wireMsg) bool {
		return w.resp && w.id != nil && *w.id == id
	})
}

func (h *fakeHost) awaitNotification(t *testing.T, method string, d time.Duration) wireMsg {
	t.Helper()
	return h.wait(t, "notification "+method, d, func(w wireMsg) bool {
		return w.method == method
	})
}

// callReloadSelf 调 reload_self 并解出结构化输出。
func (h *fakeHost) callReloadSelf(t *testing.T, id int64, d time.Duration) ReloadSelfOutput {
	t.Helper()
	h.sendReq(id, "tools/call", map[string]any{"name": "reload_self", "arguments": map[string]any{}})
	w := h.awaitResponse(t, id, d)
	if w.errMsg != "" {
		t.Fatalf("reload_self (id %d) errored: %s", id, w.errMsg)
	}
	var envelope struct {
		StructuredContent ReloadSelfOutput `json:"structuredContent"`
	}
	if err := json.Unmarshal(w.result, &envelope); err != nil {
		t.Fatalf("reload_self (id %d) result not parseable: %v (%s)", id, err, w.raw)
	}
	return envelope.StructuredContent
}

func (h *fakeHost) listToolNames(t *testing.T, id int64) []string {
	t.Helper()
	h.sendReq(id, "tools/list", map[string]any{})
	w := h.awaitResponse(t, id, 15*time.Second)
	if w.errMsg != "" {
		t.Fatalf("tools/list errored: %s", w.errMsg)
	}
	var res struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(w.result, &res); err != nil {
		t.Fatalf("tools/list result not parseable: %v (%s)", err, w.raw)
	}
	names := make([]string, 0, len(res.Tools))
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	return names
}

// handshake 完成 initialize 握手(继任不再收 initialize——首代必须完成它)。
func (h *fakeHost) handshake(t *testing.T) {
	t.Helper()
	h.sendReq(1, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "reload-e2e-fakehost", "version": "0"},
	})
	if w := h.awaitResponse(t, 1, 15*time.Second); w.errMsg != "" {
		t.Fatalf("initialize errored: %s", w.errMsg)
	}
	h.notify("notifications/initialized")
}

// runReloadHappyPath 驱动一次成功换手:握手 → 新代际落盘 → reload_self
// (started)→ list_changed 通知 → 继任自报新版本应答后续 reload_self →
// 继任的 tools/list 仍是全工具面。两种桥形态共用。
func runReloadHappyPath(t *testing.T, h *fakeHost, v1, v2 string) {
	t.Helper()
	h.handshake(t)

	names := h.listToolNames(t, 2)
	if len(names) != len(authorityTools)+1 {
		t.Fatalf("bridge face = %d tools, want %d (authority + reload_self): %v", len(names), len(authorityTools)+1, names)
	}
	found := false
	for _, n := range names {
		if n == hotswap.ToolReloadSelf {
			found = true
		}
	}
	if !found {
		t.Fatalf("bridge face missing reload_self: %v", names)
	}

	// 桥已启动(出生代际已快照):此刻写新代际信号(降级/升级同构,版本
	// 串只做展示)。
	exe := testBinaryPath(t)
	t.Cleanup(func() { os.Remove(updater.SignalPath(exe)) })
	if _, err := updater.WriteGenerationSignal(exe, v2); err != nil {
		t.Fatal(err)
	}

	// reload_self:旧桥应答 started;应答里 GitHub 查询失败如实带错误
	//(SSHMGR_UPDATE_BASE 指向必拒环回)。
	out := h.callReloadSelf(t, 3, 30*time.Second)
	if out.Handover != "started" {
		t.Fatalf("handover = %q want started, error %q (stderr:\n%s)", out.Handover, out.Error, h.stderrFn())
	}
	if out.Version != v1 {
		t.Fatalf("first reload_self version = %q want %q", out.Version, v1)
	}
	if out.SuccessorVersion != v2 {
		t.Fatalf("successor_version = %q want %q", out.SuccessorVersion, v2)
	}
	if out.DiskVersion != v2 || out.DiskGeneration <= 0 {
		t.Fatalf("disk fields: %+v", out)
	}
	if out.LatestError == "" {
		t.Fatalf("latest_error must carry the failure (test base is unreachable): %+v", out)
	}

	// 换手完成后,宿主仍能收到继任的 tools/list_changed 通知(且晚于
	// reload_self 的应答——应答先写回、后退位,F5)。
	w := h.awaitNotification(t, "notifications/tools/list_changed", 20*time.Second)
	if w.raw == "" {
		t.Fatal("unreachable")
	}

	// 继任接管:后续 reload_self 由继任应答,自报 v2 证明换代;被领养桥
	// 换手已接线(票 05)——此刻盘上无更新代际,如实报 no_new_generation
	// (再换代的三代链见 reload_later_e2e_test.go)。
	out2 := h.callReloadSelf(t, 4, 30*time.Second)
	if out2.Version != v2 {
		t.Fatalf("second reload_self version = %q want %q (successor must answer)", out2.Version, v2)
	}
	if out2.Handover != "no_new_generation" {
		t.Fatalf("successor reload_self handover = %q want no_new_generation", out2.Handover)
	}

	// 继任的工具面完整。
	names2 := h.listToolNames(t, 5)
	if len(names2) != len(authorityTools)+1 {
		t.Fatalf("successor face = %d tools, want %d: %v", len(names2), len(authorityTools)+1, names2)
	}

	// 宿主断开:泵收干,桥进程干净退出。
	h.stdinW.Close()
	done := make(chan error, 1)
	go func() { done <- h.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatalf("bridge did not exit after host disconnect; stderr:\n%s", h.stderrFn())
	}
}

// TestReloadSelfE2EDirect:直连形态(RunStdio)换手全链。
func TestReloadSelfE2EDirect(t *testing.T) {
	storePath, masterHex, token := buildTestVault(t)
	h := spawnBridgeRole(t, roleBridgeDirect, map[string]string{
		"SSHMGR_STORE":              storePath,
		"SSHMGR_MASTERKEY_HEX":      masterHex,
		"SSHMGR_TEST_TOKEN":         token,
		"SSHMGR_TEST_VERSION":       "v1-direct",
		"SSHMGR_TEST_SUCCESSOR_ENV": "SSHMGR_TEST_VERSION=v2-successor",
	})
	runReloadHappyPath(t, h, "v1-direct", "v2-successor")
}

// TestReloadSelfE2ECache:缓存形态(RunStdioCache)换手全链。
func TestReloadSelfE2ECache(t *testing.T) {
	snapPath, auditPath, token := buildTestSnapshotFile(t)
	h := spawnBridgeRole(t, roleBridgeCache, map[string]string{
		"SSHMGR_TEST_SNAPSHOT":      snapPath,
		"SSHMGR_TEST_AUDIT":         auditPath,
		"SSHMGR_TEST_TOKEN":         token,
		"SSHMGR_TEST_VERSION":       "v1-cache",
		"SSHMGR_TEST_SUCCESSOR_ENV": "SSHMGR_TEST_VERSION=v2-successor",
	})
	runReloadHappyPath(t, h, "v1-cache", "v2-successor")
}

// TestReloadSelfE2ERollback:继任永不就绪 → reload_self 带失败与原因返回,
// 原桥继续应答后续请求(tools/list 与再一次 reload_self)。
func TestReloadSelfE2ERollback(t *testing.T) {
	storePath, masterHex, token := buildTestVault(t)
	h := spawnBridgeRole(t, roleBridgeDirect, map[string]string{
		"SSHMGR_STORE":         storePath,
		"SSHMGR_MASTERKEY_HEX": masterHex,
		"SSHMGR_TEST_TOKEN":    token,
		"SSHMGR_TEST_VERSION":  "v1-rollback",
		// 继任角色换成永不就绪,且就绪等待收短(等不起 15s 默认)。
		"SSHMGR_TEST_SUCCESSOR_ENV":    reloadRoleEnv + "=" + roleNeverReady,
		"SSHMGR_TEST_READY_TIMEOUT_MS": "800",
	})
	h.handshake(t)

	exe := testBinaryPath(t)
	t.Cleanup(func() { os.Remove(updater.SignalPath(exe)) })
	if _, err := updater.WriteGenerationSignal(exe, "v2-never"); err != nil {
		t.Fatal(err)
	}

	out := h.callReloadSelf(t, 3, 30*time.Second)
	if out.Handover != "failed" {
		t.Fatalf("handover = %q want failed (stderr:\n%s)", out.Handover, h.stderrFn())
	}
	if !strings.Contains(out.Error, "not ready in time") {
		t.Fatalf("failure must carry the ready-timeout identity: %q", out.Error)
	}

	// 原桥继续应答:tools/list 正常,再一次 reload_self 仍是本桥版本、
	// 再次如实失败(armed 复位)。
	if names := h.listToolNames(t, 4); len(names) != len(authorityTools)+1 {
		t.Fatalf("old bridge face = %d tools, want %d", len(names), len(authorityTools)+1)
	}
	out2 := h.callReloadSelf(t, 5, 30*time.Second)
	if out2.Version != "v1-rollback" {
		t.Fatalf("old bridge must keep answering: version = %q", out2.Version)
	}
	if out2.Handover != "failed" {
		t.Fatalf("retry handover = %q want failed", out2.Handover)
	}

	// 断开收场:桥进程干净退出(没有换手发生,没有泵)。
	h.stdinW.Close()
	done := make(chan error, 1)
	go func() { done <- h.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatalf("bridge did not exit after host disconnect; stderr:\n%s", h.stderrFn())
	}
}
