package mcpserver

// 命令策略·执行工具接入拦截 (ticket #10 / ADR 0007) 的行为测试:
// exec_command 与 exec_background 对拦截档命令——
//   - 返回一行错误:规则编号 + 为什么危险 + 该怎么做,不列其他规则;
//   - 进程内测试 sshd 一个 TCP 连接都没收到 (Connections 计数器);
//   - 审计落新动作行,Status 存命中规则编号,Command 保持命令原文;
//   - 客户端离线 (只读 store) 时审计行进现有离线侧文件;
//   - sudo 开关不影响处置;放行的命令行为与现状完全一致。
//
// 测试切入点沿用现有执行工具分支测试的写法 (函数级 ForProfile, 见
// bgtools_test.go 的 TestExecBackground* 与 core_test.go 的 TestExecCommand*)。

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

// blockedCmd 是命中 POSIX-FORMAT 默认拦截规则的命令 (mkfs 家族)。
const blockedCmd = "mkfs.ext4 /dev/sdb"

// policyRows 取全部指定 action 的审计行 (计数防双写)。
func policyRows(t *testing.T, st *store.Store, action string) []store.AuditRow {
	t.Helper()
	rows, err := st.AuditRows(50)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.AuditRow
	for _, r := range rows {
		if r.Action == action {
			out = append(out, r)
		}
	}
	return out
}

// wantSolePolicyRow 断言恰一行该 action 的审计行且 Status(=命中规则编号)/
// Command(=命令原文)/Sudo/ServerID/ProjectID 全匹配——照抄 wantSoleStartRow
// 的「恰一行」防重复落行。
func wantSolePolicyRow(t *testing.T, st *store.Store, action, ruleID, command string, sudo bool, serverID, projectID string) {
	t.Helper()
	rows := policyRows(t, st, action)
	if len(rows) != 1 {
		t.Fatalf("%s rows = %d, want exactly 1: %+v", action, len(rows), rows)
	}
	r := rows[0]
	if r.Status != ruleID || r.Command != command || r.Sudo != sudo ||
		r.ServerID != serverID || r.ProjectID != projectID {
		t.Fatalf("row = {Status:%q Command:%q Sudo:%v Server:%q Proj:%q}, want {Status:%q Command:%q Sudo:%v Server:%q Proj:%q}",
			r.Status, r.Command, r.Sudo, r.ServerID, r.ProjectID,
			ruleID, command, sudo, serverID, projectID)
	}
}

// assertBlockedError 钉拦截错误的一行形态:含规则编号、为什么危险、该怎么做,
// 不换行,也不列出其他规则 (另一条规则的编号不得出现)。
func assertBlockedError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("want blocked error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "[POSIX-FORMAT]") {
		t.Fatalf("error %q does not name the rule id [POSIX-FORMAT]", msg)
	}
	if !strings.Contains(msg, "cannot be recovered") { // 为什么危险 (规则自带的风险说明)
		t.Fatalf("error %q does not say why it is dangerous", msg)
	}
	if !strings.Contains(msg, "instead") { // 该怎么做 (规则自带的替代做法)
		t.Fatalf("error %q does not say what to do instead", msg)
	}
	if strings.Contains(msg, "\n") {
		t.Fatalf("error must be one line, got %q", msg)
	}
	if strings.Contains(msg, "POSIX-RAW-DISK-WRITE") { // 不得列出其他规则
		t.Fatalf("error %q lists another rule", msg)
	}
}

// TestExecCommandPolicyBlockedBeforeConnect: 拦截档命令一行错误、测试 sshd
// 零新连接 (Connections 计数器;放行命令先跑一遍自证计数器会动)、审计恰一行
// 新动作;sudo 开关不改变处置。放行命令行为与现状一致作对照。
func TestExecCommandPolicyBlockedBeforeConnect(t *testing.T) {
	var conns atomic.Int64
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password:    "pw",
		Connections: &conns,
		Exec:        func(cmd string, _ io.Reader) (string, string, int) { return "RAN:" + cmd + "\n", "", 0 },
	})
	defer cleanup()
	st := newStore(t)
	srvID := seedRealServer(t, st, "real", addr, hk, "")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})
	ctx := context.Background()

	// 对照:放行命令照常执行 (同时证明计数器确实会计数,后面的零断言不空转)。
	out, err := ExecCommandForProfile(ctx, st, "proj-pol", pid, srvID, "echo ok", false, 5*time.Second)
	if err != nil {
		t.Fatalf("allowed exec: %v", err)
	}
	if out.Stdout != "RAN:echo ok\n" {
		t.Fatalf("allowed stdout = %q, want unchanged passthrough", out.Stdout)
	}
	if n := conns.Load(); n != 1 {
		t.Fatalf("connections = %d after allowed run, want 1", n)
	}

	// 拦截:一行错误,测试 sshd 未收到任何新连接。
	_, err = ExecCommandForProfile(ctx, st, "proj-pol", pid, srvID, blockedCmd, false, 5*time.Second)
	assertBlockedError(t, err)
	if n := conns.Load(); n != 1 {
		t.Fatalf("test sshd received %d connections after blocked call, want still 1 (no new connection)", n)
	}
	wantSolePolicyRow(t, st, "exec-blocked", "POSIX-FORMAT", blockedCmd, false, srvID, "proj-pol")

	// sudo 开关不影响处置:sudo=true 同样拦截、同一规则编号。
	_, err = ExecCommandForProfile(ctx, st, "proj-pol", pid, srvID, blockedCmd, true, 5*time.Second)
	assertBlockedError(t, err)
	if n := conns.Load(); n != 1 {
		t.Fatalf("test sshd received %d connections after sudo blocked call, want still 1", n)
	}
	if rows := policyRows(t, st, "exec-blocked"); len(rows) != 2 {
		t.Fatalf("exec-blocked rows = %d, want 2 (one per call)", len(rows))
	}
	for _, r := range policyRows(t, st, "exec-blocked") {
		if r.Status != "POSIX-FORMAT" {
			t.Fatalf("blocked row Status = %q, want rule id POSIX-FORMAT", r.Status)
		}
	}
}

// TestExecBackgroundPolicyBlockedBeforeConnect: exec_background 的镜像断言——
// 一行错误、零新连接、审计恰一行新动作、任务表不进任务、sudo 开关不影响处置。
func TestExecBackgroundPolicyBlockedBeforeConnect(t *testing.T) {
	var conns atomic.Int64
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password:    "pw",
		Connections: &conns,
		Exec:        func(cmd string, _ io.Reader) (string, string, int) { return "RAN:" + cmd + "\n", "", 0 },
	})
	defer cleanup()
	st := newStore(t)
	srvID := seedRealServer(t, st, "real", addr, hk, "")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})
	m := newTestTM(t, 4)
	defer m.CloseAll()
	ctx := context.Background()

	// 对照:放行命令照常起后台任务 (计数器自证)。
	out, err := ExecBackgroundForProfile(ctx, st, "proj-pol", pid, srvID, "echo ok", false, 2, m)
	if err != nil {
		t.Fatalf("allowed bg start: %v", err)
	}
	if out.TaskID == "" || out.Status != bgStatusRunning {
		t.Fatalf("allowed bg start = %+v, want a running task", out)
	}
	if n := conns.Load(); n != 1 {
		t.Fatalf("connections = %d after allowed bg run, want 1", n)
	}

	// 拦截:一行错误,零新连接,任务表不进任务。
	_, err = ExecBackgroundForProfile(ctx, st, "proj-pol", pid, srvID, blockedCmd, false, 2, m)
	assertBlockedError(t, err)
	if n := conns.Load(); n != 1 {
		t.Fatalf("test sshd received %d connections after blocked bg call, want still 1", n)
	}
	if m.Len() != 1 { // 只有对照那一个放行任务
		t.Fatalf("registry len = %d after blocked bg call, want 1 (only the allowed control task)", m.Len())
	}
	wantSolePolicyRow(t, st, "exec-bg-blocked", "POSIX-FORMAT", blockedCmd, false, srvID, "proj-pol")

	// sudo 开关不影响处置。
	_, err = ExecBackgroundForProfile(ctx, st, "proj-pol", pid, srvID, blockedCmd, true, 2, m)
	assertBlockedError(t, err)
	if rows := policyRows(t, st, "exec-bg-blocked"); len(rows) != 2 {
		t.Fatalf("exec-bg-blocked rows = %d, want 2 (one per call)", len(rows))
	}
}

// TestExecPolicyBlockedBeforeCredentialLookup: 判定必须先于取凭据——无凭据
// server 上拦截档命令拿到的是拦截错误,不是 no_credential。两个工具各验一遍。
func TestExecPolicyBlockedBeforeCredentialLookup(t *testing.T) {
	const vh = "vault-policy.example.internal"
	st := newStore(t)
	srvID, _ := st.AddServer(&models.Server{Name: "bare", Host: vh, Port: 22, User: "u"})
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})
	m := newTestTM(t, 4)
	defer m.CloseAll()
	ctx := context.Background()

	_, err := ExecCommandForProfile(ctx, st, "proj-pol", pid, srvID, blockedCmd, false, 5*time.Second)
	assertBlockedError(t, err)
	wantSolePolicyRow(t, st, "exec-blocked", "POSIX-FORMAT", blockedCmd, false, srvID, "proj-pol")

	_, err = ExecBackgroundForProfile(ctx, st, "proj-pol", pid, srvID, blockedCmd, false, 0, m)
	assertBlockedError(t, err)
	wantSolePolicyRow(t, st, "exec-bg-blocked", "POSIX-FORMAT", blockedCmd, false, srvID, "proj-pol")
}

// TestExecBlockedAuditGoesOfflineSidecar: 客户端离线 (只读 store) 时拦截审计行
// 进现有离线审计侧文件 (JSONL),不碰缓存库。两个工具各一行。
func TestExecBlockedAuditGoesOfflineSidecar(t *testing.T) {
	// 可写 fixture 库 → 快照 → 只读水合库 + 侧文件 (现有 cache 桥的装配形态)。
	st := newStore(t)
	srvID, _ := st.AddServer(&models.Server{
		Name: "off", Host: "offline.example.internal", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st),
	})
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})
	snap, err := st.ExportSnapshot()
	if err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	hyd := newStoreAt(t, filepath.Join(t.TempDir(), "cache.db"))
	if err := hyd.ImportSnapshot(snap); err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	sidePath := filepath.Join(t.TempDir(), "cache-audit.log")
	af, err := os.OpenFile(sidePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = af.Close() }) // 提前 Fatal 时也不拦住 TempDir 清理 (Windows 文件占用)
	hyd.SetReadOnly(af)
	m := newTestTM(t, 4)
	defer m.CloseAll()
	ctx := context.Background()

	_, err = ExecCommandForProfile(ctx, hyd, "proj-off", pid, srvID, blockedCmd, false, 5*time.Second)
	assertBlockedError(t, err)
	_, err = ExecBackgroundForProfile(ctx, hyd, "proj-off", pid, srvID, blockedCmd, false, 0, m)
	assertBlockedError(t, err)
	_ = af.Close() // 落盘后立即关句柄再读回 (Cleanup 里的二次 Close 忽错)

	// 侧文件恰两行 JSONL:action 与 Status(=规则编号) 逐行匹配,Command 是命令原文。
	b, err := os.ReadFile(sidePath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("sidecar lines = %d, want 2: %q", len(lines), string(b))
	}
	wantActions := []string{"exec-blocked", "exec-bg-blocked"}
	for i, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("sidecar line %d is not JSON: %v", i, err)
		}
		if rec["action"] != wantActions[i] {
			t.Fatalf("sidecar line %d action = %v, want %q", i, rec["action"], wantActions[i])
		}
		if rec["status"] != "POSIX-FORMAT" {
			t.Fatalf("sidecar line %d status = %v, want POSIX-FORMAT", i, rec["status"])
		}
		if rec["command"] != blockedCmd {
			t.Fatalf("sidecar line %d command = %v, want %q", i, rec["command"], blockedCmd)
		}
	}
	// 缓存库里零审计行 (离线审计绝不写库)。
	if rows, err := hyd.AuditRows(10); err != nil || len(rows) != 0 {
		t.Fatalf("hydrated db audit rows = %d (err %v), want 0 (offline audit never touches the db)", len(rows), err)
	}
}
