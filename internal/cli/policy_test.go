package cli

import (
	"bytes"
	"strings"
	"testing"
)

// runPolicyTest 以给定参数执行 `sshmgr policy test`,返回其输出。
func runPolicyTest(t *testing.T, args ...string) (*bytes.Buffer, error) {
	t.Helper()
	full := append([]string{"policy", "test"}, args...)
	root := NewRootCmd()
	root.SetArgs(full)
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	err := root.Execute()
	return out, err
}

// TestPolicyTest_BlockedCommand:拦截命令打印处置、规则编号、风险说明
// 与替代做法四要素。
func TestPolicyTest_BlockedCommand(t *testing.T) {
	out, err := runPolicyTest(t, "--shell", "posix", "mkfs.ext4 /dev/sdb")
	if err != nil {
		t.Fatalf("policy test: %v", err)
	}
	got := out.String()
	for _, want := range []string{"拦截", "POSIX-FORMAT", "why:", "instead:", "formatting erases everything"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output must contain %q, got:\n%s", want, got)
		}
	}
}

// TestPolicyTest_CombinedCommand:验收用例——危险命令藏在组合命令与前缀
// 之后,试算仍报拦截与正确规则编号。
func TestPolicyTest_CombinedCommand(t *testing.T) {
	out, err := runPolicyTest(t, "--shell", "posix", "cd /x && sudo rm -rf /etc; ls")
	if err != nil {
		t.Fatalf("policy test: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "拦截") || !strings.Contains(got, "POSIX-CRITICAL-DELETE") {
		t.Fatalf("output must report 拦截/POSIX-CRITICAL-DELETE, got:\n%s", got)
	}
}

// TestPolicyTest_AllowedCommand:放行命令只报处置,不报规则编号。
func TestPolicyTest_AllowedCommand(t *testing.T) {
	out, err := runPolicyTest(t, "--shell", "posix", "rm -r ./build")
	if err != nil {
		t.Fatalf("policy test: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "放行") {
		t.Fatalf("output must report 放行, got:\n%s", got)
	}
	if strings.Contains(got, "rule:") {
		t.Fatalf("放行不该报规则编号, got:\n%s", got)
	}
}

// TestPolicyTest_DefaultShellIsPOSIX:不传 --shell 时按 POSIX 拆解。
func TestPolicyTest_DefaultShellIsPOSIX(t *testing.T) {
	out, err := runPolicyTest(t, "rm -rf /etc")
	if err != nil {
		t.Fatalf("policy test: %v", err)
	}
	if !strings.Contains(out.String(), "POSIX-CRITICAL-DELETE") {
		t.Fatalf("default shell must be posix, got:\n%s", out.String())
	}
}

// TestPolicyTest_PowerShellAndCmdShells:`--shell powershell` 与
// `--shell cmd` 两类 Windows shell 的试算可用,输出同样的四要素面。
func TestPolicyTest_PowerShellAndCmdShells(t *testing.T) {
	tests := []struct {
		name    string
		shell   string
		command string
		want    string
		notWant string
	}{
		{"powershell 拦截", "powershell", "Remove-Item -Recurse C:\\Windows", "WIN-CRITICAL-DELETE", ""},
		{"cmd 拦截", "cmd", "rd /s C:\\Windows", "WIN-CRITICAL-DELETE", ""},
		{"powershell 需确认", "powershell", "Stop-Service sshd", "需确认", ""},
		{"powershell 放行", "powershell", "Get-ChildItem", "放行", "rule:"},
		{"cmd 放行", "cmd", "dir", "放行", "rule:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := runPolicyTest(t, "--shell", tt.shell, tt.command)
			if err != nil {
				t.Fatalf("policy test: %v", err)
			}
			got := out.String()
			if !strings.Contains(got, tt.want) {
				t.Fatalf("output must contain %q, got:\n%s", tt.want, got)
			}
			if tt.notWant != "" && strings.Contains(got, tt.notWant) {
				t.Fatalf("output must not contain %q, got:\n%s", tt.notWant, got)
			}
		})
	}
}

// TestPolicyTest_RejectsUnknownShell:未知 shell 类型直接报错。
func TestPolicyTest_RejectsUnknownShell(t *testing.T) {
	_, err := runPolicyTest(t, "--shell", "fish", "ls")
	if err == nil || !strings.Contains(err.Error(), "shell type") {
		t.Fatalf("unknown shell must error mentioning shell type, got %v", err)
	}
}

// TestPolicyTest_RequiresExactlyOneCommand:没有命令文本时报错。
func TestPolicyTest_RequiresExactlyOneCommand(t *testing.T) {
	if _, err := runPolicyTest(t); err == nil {
		t.Fatal("policy test without a command must error")
	}
}
