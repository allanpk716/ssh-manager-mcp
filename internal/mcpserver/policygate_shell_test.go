package mcpserver

// 命令策略·服务器 shell 类型字段(ticket #14)接到执行工具后的行为测试:
//   - 标注为 PowerShell 的服务器上 Format-Volume 拦截(WIN-FORMAT),标注为
//     POSIX 的服务器上 mkfs 拦截(POSIX-FORMAT)——两个执行工具各验一遍,
//     拦截时不建立 SSH 连接、审计行 Status 存命中规则编号;
//   - 无法识别的 shell 标注(标注错误/坏数据)判需确认,不悄悄放行——
//     需确认档的一次性确认码由命令策略后续的 GitHub Issue 接,本票只钉
//     判定结论的方向。
//
// 写法沿用 policygate_test.go(ticket #10)的镜像断言。

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/policy"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

// winBlockedCmd 命中 WIN-FORMAT 默认拦截规则的命令(Format-Volume 家族)。
const winBlockedCmd = "Format-Volume -DriveLetter D"

// seedRealServerShell 建一台指向测试 sshd、带指定 shell 标注的服务器。
func seedRealServerShell(t *testing.T, st *store.Store, name, addr string, hk ssh.PublicKey, shell models.ShellType) string {
	t.Helper()
	id := seedRealServer(t, st, name, addr, hk, "")
	srv, err := st.GetServer(id)
	if err != nil || srv == nil {
		t.Fatalf("re-read seeded server: %v %v", srv, err)
	}
	srv.ShellType = shell
	if err := st.UpdateServer(srv); err != nil {
		t.Fatal(err)
	}
	return id
}

// assertWinBlockedError 钉 PowerShell 侧拦截错误的一行形态(规则编号在)。
func assertWinBlockedError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("want blocked error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "[WIN-FORMAT]") {
		t.Fatalf("error %q does not name the rule id [WIN-FORMAT]", msg)
	}
	if strings.Contains(msg, "\n") {
		t.Fatalf("error must be one line, got %q", msg)
	}
}

// TestExecCommandShellTypePicksGrammar: 同一条 Format-Volume,在标注为
// PowerShell 的服务器上被拦截(WIN-FORMAT)、零新连接;对照——POSIX 标注
// 的服务器上放行(POSIX 语法里它只是个普通命令名,拆解按标注进行;
// 标注错误的诊断提示是票 #17 的事)。mkfs 在 POSIX 标注上拦截由
// policygate_test.go 的 POSIX 用例覆盖。
func TestExecCommandShellTypePicksGrammar(t *testing.T) {
	var conns atomic.Int64
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password:    "pw",
		Connections: &conns,
		Exec:        func(cmd string, _ io.Reader) (string, string, int) { return "RAN:" + cmd + "\n", "", 0 },
	})
	defer cleanup()
	st := newStore(t)
	psID := seedRealServerShell(t, st, "ps", addr, hk, models.ShellPowerShell)
	nixID := seedRealServerShell(t, st, "nix", addr, hk, models.ShellPOSIX)
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{psID, nixID})
	ctx := context.Background()

	// PowerShell 标注:拦截,一行错误,零新连接,审计行带规则编号。
	_, err := ExecCommandForProfile(ctx, st, "proj-shell", pid, psID, winBlockedCmd, false, 5*time.Second)
	assertWinBlockedError(t, err)
	if n := conns.Load(); n != 0 {
		t.Fatalf("test sshd received %d connections after blocked call, want 0", n)
	}
	wantSolePolicyRow(t, st, "exec-blocked", "WIN-FORMAT", winBlockedCmd, false, psID, "proj-shell")

	// 对照:POSIX 标注上同一条命令放行(照常执行)。
	out, err := ExecCommandForProfile(ctx, st, "proj-shell", pid, nixID, winBlockedCmd, false, 5*time.Second)
	if err != nil {
		t.Fatalf("posix-labeled Format-Volume must pass through: %v", err)
	}
	if !strings.HasPrefix(out.Stdout, "RAN:") {
		t.Fatalf("allowed stdout = %q", out.Stdout)
	}
}

// TestExecBackgroundShellTypePicksGrammar: exec_background 的镜像断言。
func TestExecBackgroundShellTypePicksGrammar(t *testing.T) {
	var conns atomic.Int64
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password:    "pw",
		Connections: &conns,
		Exec:        func(cmd string, _ io.Reader) (string, string, int) { return "RAN:" + cmd + "\n", "", 0 },
	})
	defer cleanup()
	st := newStore(t)
	psID := seedRealServerShell(t, st, "ps", addr, hk, models.ShellPowerShell)
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{psID})
	m := newTestTM(t, 4)
	defer m.CloseAll()
	ctx := context.Background()

	_, err := ExecBackgroundForProfile(ctx, st, "proj-shell", pid, psID, winBlockedCmd, false, 2, m)
	assertWinBlockedError(t, err)
	if n := conns.Load(); n != 0 {
		t.Fatalf("test sshd received %d connections after blocked bg call, want 0", n)
	}
	if m.Len() != 0 {
		t.Fatalf("registry len = %d after blocked bg call, want 0", m.Len())
	}
	wantSolePolicyRow(t, st, "exec-bg-blocked", "WIN-FORMAT", winBlockedCmd, false, psID, "proj-shell")
}

// TestPolicyJudgeUnknownShellLeansConfirm: 标注值无法识别时,判定结论是
// 需确认——拆解不准的命令偏向需确认,不悄悄放行(规格 user story 42)。
// 存储层会把空值归一为 POSIX,能到这里的是坏数据/未知值。
func TestPolicyJudgeUnknownShellLeansConfirm(t *testing.T) {
	st := newStore(t)
	for _, bad := range []models.ShellType{"fish", "POSIX", "powershel"} {
		d := policyJudge(st, bad, "rm -rf /data")
		if d.Disposition != policy.DispositionConfirmRequired {
			t.Fatalf("policyJudge(shell=%q) = %+v, want 需确认", bad, d.Disposition)
		}
		if d.Explanation == "" {
			t.Fatalf("需确认必须带说明,shell=%q", bad)
		}
	}
	// 已知三类值正常分派(不落入保守分支;空库即纯内置默认规则)。
	if d := policyJudge(st, models.ShellPowerShell, winBlockedCmd); d.Disposition != policy.DispositionBlock || d.RuleID != "WIN-FORMAT" {
		t.Fatalf("powershell dispatch = %+v, want WIN-FORMAT 拦截", d)
	}
	if d := policyJudge(st, models.ShellPOSIX, blockedCmd); d.Disposition != policy.DispositionBlock || d.RuleID != "POSIX-FORMAT" {
		t.Fatalf("posix dispatch = %+v, want POSIX-FORMAT 拦截", d)
	}
	if d := policyJudge(st, models.ShellWindowsCmd, "echo ok"); d.Disposition != policy.DispositionAllow {
		t.Fatalf("cmd dispatch = %+v, want 放行", d)
	}
}
