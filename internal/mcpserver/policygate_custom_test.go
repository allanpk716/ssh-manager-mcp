package mcpserver

// 命令策略·生效规则集接入执行工具(ticket #15 / ADR 0007):policyJudge
// 从执行的 store(权威库或客户端水合出的只读镜像库)读自定义规则与停用
// 记录,判定不再是「只有内置默认规则」。行为断言——
//   - 自定义规则(结构化与正则各一例)改变执行工具结果:原本放行的
//     命令被拦,一行错误含自定义规则编号;
//   - 停用一条默认规则后该规则不再命中(命令照常执行);恢复后重新命中;
//   - 旧快照(无策略段)水合出的客户端库上,内置默认规则照常拦截;
//   - 带自定义规则的快照水合出的只读库(客户端镜像)同样按规则判定。

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/policy"
	"ssh-manager-mcp/internal/policymanage"
	"ssh-manager-mcp/internal/testsshd"
)

// assertBlockedBy 钉拦截错误的一行形态:含给定规则编号、为什么危险、
// 该怎么做,不换行。
func assertBlockedBy(t *testing.T, err error, ruleID string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want blocked error naming %s, got nil", ruleID)
	}
	msg := err.Error()
	if !strings.Contains(msg, "["+ruleID+"]") {
		t.Fatalf("error %q does not name the rule id [%s]", msg, ruleID)
	}
	if !strings.Contains(msg, "instead") {
		t.Fatalf("error %q does not say what to do instead", msg)
	}
	if strings.Contains(msg, "\n") {
		t.Fatalf("error must be one line, got %q", msg)
	}
}

// TestExecPolicyCustomRules_ChangeExecToolResults:验收用例——增加一条
// 自定义规则后,试算与执行工具都按它判定;结构化字段与正则各一例,
// 两个执行工具各覆盖。原本放行的命令(reboot / docker system prune)
// 被自定义规则拦下,审计行 Status 存自定义规则编号。
func TestExecPolicyCustomRules_ChangeExecToolResults(t *testing.T) {
	const vh = "vault-custom.example.internal"
	st := newStore(t)
	// 结构化一例:按命令名拦 reboot。正则一例:拦「docker system prune -a」。
	if err := st.AddPolicyRule(policy.CustomRule{
		ID: "NO-REBOOT", Shells: []policy.ShellType{policy.ShellPOSIX},
		Commands: []string{"reboot"}, Disposition: policy.DispositionBlock,
		Explanation: "reboots are scheduled by the owner, never run by the agent",
		Alternative: "ask the owner to schedule a reboot window",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddPolicyRule(policy.CustomRule{
		ID: "NO-DOCKER-PRUNE", Regex: `docker\s+system\s+prune\s+.*-a`,
		Disposition: policy.DispositionBlock,
		Explanation: "pruning all docker state removes volumes this server needs",
		Alternative: "list what would go with `docker system df` and let the owner prune",
	}); err != nil {
		t.Fatal(err)
	}
	srvID, _ := st.AddServer(&models.Server{Name: "bare", Host: vh, Port: 22, User: "u"})
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})
	m := newTestTM(t, 4)
	defer m.CloseAll()
	ctx := context.Background()

	// 结构化规则 → exec_command。
	_, err := ExecCommandForProfile(ctx, st, "proj-c", pid, srvID, "reboot", false, 5*time.Second, ExecConfirmation{})
	assertBlockedBy(t, err, "NO-REBOOT")
	wantSolePolicyRow(t, st, "exec-blocked", "NO-REBOOT", "reboot", false, srvID, "proj-c")

	// 正则规则 → exec_command。
	_, err = ExecCommandForProfile(ctx, st, "proj-c", pid, srvID, "docker system prune -a --volumes", false, 5*time.Second, ExecConfirmation{})
	assertBlockedBy(t, err, "NO-DOCKER-PRUNE")

	// 结构化规则 → exec_background。
	_, err = ExecBackgroundForProfile(ctx, st, "proj-c", pid, srvID, "sudo reboot", false, 0, m, ExecConfirmation{})
	assertBlockedBy(t, err, "NO-REBOOT")
	wantSolePolicyRow(t, st, "exec-bg-blocked", "NO-REBOOT", "sudo reboot", false, srvID, "proj-c")

	// 未命中的命令不受影响:仍走默认规则面(此处放行到取凭据一步)。
	_, err = ExecCommandForProfile(ctx, st, "proj-c", pid, srvID, "uptime", false, 5*time.Second, ExecConfirmation{})
	if err == nil || strings.Contains(err.Error(), "NO-REBOOT") {
		t.Fatalf("uptime must not hit the custom rule, got %v", err)
	}
}

// TestExecPolicyDisabledDefaultRule_NoLongerMatches:验收用例——停用一条
// 默认规则后不再命中(命令真的执行,测试 sshd 收到连接);恢复后重新
// 命中(又拦、零新连接)。
func TestExecPolicyDisabledDefaultRule_NoLongerMatches(t *testing.T) {
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

	// 停用前:默认规则拦截(ticket #10 的既有行为)。
	_, err := ExecCommandForProfile(ctx, st, "proj-d", pid, srvID, blockedCmd, false, 5*time.Second, ExecConfirmation{})
	assertBlockedError(t, err)
	if n := conns.Load(); n != 0 {
		t.Fatalf("connections = %d before disable, want 0", n)
	}

	// 停用默认规则:命令照常执行。
	if _, err := policymanage.DisableDefaultRule(st, "POSIX-FORMAT"); err != nil {
		t.Fatal(err)
	}
	out, err := ExecCommandForProfile(ctx, st, "proj-d", pid, srvID, blockedCmd, false, 5*time.Second, ExecConfirmation{})
	if err != nil {
		t.Fatalf("disabled default must let the command run: %v", err)
	}
	if out.Stdout != "RAN:"+blockedCmd+"\n" {
		t.Fatalf("stdout = %q, want passthrough of the blocked command", out.Stdout)
	}
	if n := conns.Load(); n != 1 {
		t.Fatalf("connections = %d after disabled-default run, want 1", n)
	}

	// 恢复:重新命中,又拦、零新连接。
	if _, err := policymanage.EnableDefaultRule(st, "POSIX-FORMAT"); err != nil {
		t.Fatal(err)
	}
	_, err = ExecCommandForProfile(ctx, st, "proj-d", pid, srvID, blockedCmd, false, 5*time.Second, ExecConfirmation{})
	assertBlockedError(t, err)
	if n := conns.Load(); n != 1 {
		t.Fatalf("connections = %d after re-enable, want still 1", n)
	}
}

// TestExecPolicyClientMirror_JudgesBySnapshotRules:客户端只读镜像——快照
// 里的自定义规则与停用记录经水合库生效(客户端离线执行侧与权威端同一条
// 判定代码路径)。
func TestExecPolicyClientMirror_JudgesBySnapshotRules(t *testing.T) {
	src := newStore(t)
	if err := src.AddPolicyRule(policy.CustomRule{
		ID: "NO-REBOOT", Shells: []policy.ShellType{policy.ShellPOSIX},
		Commands: []string{"reboot"}, Disposition: policy.DispositionBlock,
		Explanation: "reboots are scheduled by the owner, never run by the agent",
		Alternative: "ask the owner to schedule a reboot window",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := src.DisableDefaultRule("POSIX-FORMAT"); err != nil {
		t.Fatal(err)
	}
	srvID, _ := src.AddServer(&models.Server{Name: "bare", Host: "vault-custom.example.internal", Port: 22, User: "u"})
	pid, _ := src.AddProfile("p")
	_ = src.GrantServers(pid, []string{srvID})
	snap, err := src.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	hyd := newStoreAt(t, t.TempDir()+"/cache.db")
	if err := hyd.ImportSnapshot(snap); err != nil {
		t.Fatal(err)
	}
	hyd.SetReadOnly(nil)
	ctx := context.Background()

	// 自定义规则在客户端执行侧命中。
	_, err = ExecCommandForProfile(ctx, hyd, "proj-m", pid, srvID, "reboot", false, 5*time.Second, ExecConfirmation{})
	assertBlockedBy(t, err, "NO-REBOOT")

	// 停用的默认规则在客户端执行侧不再命中:此处断言「不再是拦截错误」
	// ——无凭据服务器上走到取凭据一步(no_credential)而不是拦。
	_, err = ExecCommandForProfile(ctx, hyd, "proj-m", pid, srvID, "mkfs.ext4 /dev/sdb", false, 5*time.Second, ExecConfirmation{})
	if err == nil || strings.Contains(err.Error(), "POSIX-FORMAT") {
		t.Fatalf("disabled default must not block on the client mirror, got %v", err)
	}
}

// TestExecPolicyOldSnapshot_DefaultsStillBlock:验收用例——旧快照(策略
// 段尚不存在的形态)导入后,内置默认规则照常生效。
func TestExecPolicyOldSnapshot_DefaultsStillBlock(t *testing.T) {
	src := newStore(t)
	srvID, _ := src.AddServer(&models.Server{Name: "old", Host: "old.example.internal", Port: 22, User: "u"})
	pid, _ := src.AddProfile("p")
	_ = src.GrantServers(pid, []string{srvID})
	snap, err := src.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	// 模拟旧快照:策略段置空(旧程序导出的 JSON 里没有这两个键,解码
	// 结果就是 nil)。
	snap.PolicyRules = nil
	snap.PolicyDisabledDefaults = nil
	hyd := newStoreAt(t, t.TempDir()+"/old.db")
	if err := hyd.ImportSnapshot(snap); err != nil {
		t.Fatal(err)
	}
	hyd.SetReadOnly(nil)
	ctx := context.Background()

	_, err = ExecCommandForProfile(ctx, hyd, "proj-o", pid, srvID, blockedCmd, false, 5*time.Second, ExecConfirmation{})
	assertBlockedError(t, err)
}

// TestExecPolicyCustomRule_WindowsShellAnnotatedServer:合并票 #14 后,
// 执行工具按服务器条目的 shell 类型选择语法,自定义规则的适用 shell 集合
// 在执行侧同样分派——PowerShell 标注的服务器上 PowerShell 专用自定义规则
// 拦下 Restart-Computer(零连接);同一台机器若标注 POSIX 则该规则不适用,
// 命令照常执行。停用 WIN- 前缀默认规则在执行侧同样不再命中。
func TestExecPolicyCustomRule_WindowsShellAnnotatedServer(t *testing.T) {
	var conns atomic.Int64
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password:    "pw",
		Connections: &conns,
		Exec:        func(cmd string, _ io.Reader) (string, string, int) { return "RAN:" + cmd + "\n", "", 0 },
	})
	defer cleanup()
	st := newStore(t)
	if err := st.AddPolicyRule(policy.CustomRule{
		ID: "NO-RESTART", Shells: []policy.ShellType{policy.ShellPowerShell},
		Commands: []string{"Restart-Computer"}, Disposition: policy.DispositionBlock,
		Explanation: "reboots are scheduled by the owner, never run by the agent",
		Alternative: "ask the owner to schedule a reboot window",
	}); err != nil {
		t.Fatal(err)
	}
	psID := seedRealServerShell(t, st, "ps", addr, hk, models.ShellPowerShell)
	nixID := seedRealServerShell(t, st, "nix", addr, hk, models.ShellPOSIX)
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{psID, nixID})
	ctx := context.Background()

	// PowerShell 标注:PowerShell 专用自定义规则命中,零连接。
	_, err := ExecCommandForProfile(ctx, st, "proj-w", pid, psID, "Restart-Computer -Force", false, 5*time.Second, ExecConfirmation{})
	assertBlockedBy(t, err, "NO-RESTART")
	if n := conns.Load(); n != 0 {
		t.Fatalf("connections = %d after blocked windows custom rule, want 0", n)
	}
	wantSolePolicyRow(t, st, "exec-blocked", "NO-RESTART", "Restart-Computer -Force", false, psID, "proj-w")

	// 同一台测试 sshd、POSIX 标注:同一条命令按 POSIX 语法不适用 PowerShell
	// 专用规则,但跨 shell 试判用同一份生效规则集——规则在 PowerShell 侧
	// 命中,改判需确认(SHELL-MISMATCH),零新连接。
	_, err = ExecCommandForProfile(ctx, st, "proj-w", pid, nixID, "Restart-Computer -Force", false, 5*time.Second, ExecConfirmation{})
	if err == nil || !strings.Contains(err.Error(), "[SHELL-MISMATCH]") {
		t.Fatalf("posix-annotated server must surface the powershell-only rule via the cross-judgement, got %v", err)
	}
	if n := conns.Load(); n != 0 {
		t.Fatalf("connections = %d after mismatch call, want 0", n)
	}

	// 停用跨 shell 试判规则后,POSIX 标注确实跳过 PowerShell 专用规则,
	// 照常执行(连接数 +1)。
	if _, err := st.DisableDefaultRule(policy.RuleIDShellMismatch); err != nil {
		t.Fatal(err)
	}
	out, err := ExecCommandForProfile(ctx, st, "proj-w", pid, nixID, "Restart-Computer -Force", false, 5*time.Second, ExecConfirmation{})
	if err != nil {
		t.Fatalf("posix-annotated server must not hit the powershell-only rule once the cross-judgement is disabled: %v", err)
	}
	if want := "RAN:Restart-Computer -Force\n"; out.Stdout != want {
		t.Fatalf("stdout = %q, want %q", out.Stdout, want)
	}
	if n := conns.Load(); n != 1 {
		t.Fatalf("connections = %d after posix-annotated run, want 1", n)
	}

	// 停用 WIN- 前缀默认规则在执行侧同样不再命中:PowerShell 标注的
	// Format-Volume 原被 WIN-FORMAT 拦,停用后照常执行。
	if _, err := ExecCommandForProfile(ctx, st, "proj-w", pid, psID, winBlockedCmd, false, 5*time.Second, ExecConfirmation{}); err == nil || !strings.Contains(err.Error(), "WIN-FORMAT") {
		t.Fatalf("Format-Volume on powershell must be blocked by WIN-FORMAT, got %v", err)
	}
	if _, err := policymanage.DisableDefaultRule(st, "WIN-FORMAT"); err != nil {
		t.Fatal(err)
	}
	if _, err := ExecCommandForProfile(ctx, st, "proj-w", pid, psID, winBlockedCmd, false, 5*time.Second, ExecConfirmation{}); err != nil {
		t.Fatalf("disabled WIN-FORMAT must let the command run: %v", err)
	}
	if n := conns.Load(); n != 2 {
		t.Fatalf("connections = %d after disabled WIN-FORMAT run, want 2", n)
	}
}
