package probe

import (
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
)

func commandItems() []ShellCommandItem {
	return []ShellCommandItem{
		{Address: "192.0.2.10", Port: 445, Timeout: 1500 * time.Millisecond},
		{Address: "nas-a.local", Port: 22, Timeout: 1500 * time.Millisecond},
	}
}

// TestBuildShellProbeCommandContainsOnlyGeneratedValues: 三类 shell 的固定
// 命令只包含由 sshmgr 生成的地址、端口与超时；地址会经过合法性校验。
func TestBuildShellProbeCommandContainsOnlyGeneratedValues(t *testing.T) {
	for _, shell := range []models.ShellType{models.ShellPOSIX, models.ShellPowerShell, models.ShellWindowsCmd} {
		command, err := BuildShellProbeCommand(shell, commandItems())
		if err != nil {
			t.Fatalf("shell %s: %v", shell, err)
		}
		for _, want := range []string{"192.0.2.10", "nas-a.local", "445", "SSHMGR-PROBE"} {
			if !strings.Contains(command, want) {
				t.Fatalf("shell %s command missing %q:\n%s", shell, want, command)
			}
		}
		if strings.Contains(command, "; rm") || strings.Contains(command, "`") {
			t.Fatalf("shell %s command contains unexpected free text:\n%s", shell, command)
		}
	}
}

// TestBuildShellProbeCommandRejectsInvalidValues: 非法地址、端口或超时在
// 生成命令前被拒绝，任何自由文本都不会进入固定命令。
func TestBuildShellProbeCommandRejectsInvalidValues(t *testing.T) {
	cases := []struct {
		name  string
		items []ShellCommandItem
	}{
		{"address with space", []ShellCommandItem{{Address: "192.0.2.10; rm -rf /", Port: 445, Timeout: time.Second}}},
		{"address with substitution", []ShellCommandItem{{Address: "$(reboot)", Port: 445, Timeout: time.Second}}},
		{"empty address", []ShellCommandItem{{Address: "", Port: 445, Timeout: time.Second}}},
		{"port zero", []ShellCommandItem{{Address: "192.0.2.10", Port: 0, Timeout: time.Second}}},
		{"port too large", []ShellCommandItem{{Address: "192.0.2.10", Port: 65536, Timeout: time.Second}}},
		{"zero timeout", []ShellCommandItem{{Address: "192.0.2.10", Port: 445}}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			for _, shell := range []models.ShellType{models.ShellPOSIX, models.ShellPowerShell, models.ShellWindowsCmd} {
				if _, err := BuildShellProbeCommand(shell, testCase.items); err == nil {
					t.Fatalf("shell %s accepted invalid input %+v", shell, testCase.items)
				}
			}
		})
	}
}

// TestParseShellProbeOutputHandlesProtocolLines: 固定协议行按序号回填；不认
// 识的行忽略；缺少的序号记为「服务器未返回结果」；乱行不影响结果。
func TestParseShellProbeOutputHandlesProtocolLines(t *testing.T) {
	output := "banner line\n" +
		"SSHMGR-PROBE 1 fail ? missing-tool\n" +
		"SSHMGR-PROBE 0 ok 3 \n" +
		"SSHMGR-PROBE 2 fail 1500 timeout\n" +
		"SSHMGR-PROBE 9 ok 1 \n" +
		"noise\n"

	got := ParseShellProbeOutput(output, 3)
	want := []ShellProbeResult{
		{OK: true, Category: models.ProbeErrorNone, Duration: 3 * time.Millisecond},
		{OK: false, Category: models.ProbeErrorMissingTool, Duration: -1},
		{OK: false, Category: models.ProbeErrorTimeout, Duration: 1500 * time.Millisecond},
	}
	if len(got) != len(want) {
		t.Fatalf("results = %+v, want %d entries", got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("result %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestParseShellProbeOutputMissingLinesAndUnknownCategories: 缺行记为「服务
// 器未返回结果」；未知分类归入「其他」。
func TestParseShellProbeOutputMissingLinesAndUnknownCategories(t *testing.T) {
	got := ParseShellProbeOutput("SSHMGR-PROBE 0 fail 7 weird-category\n", 2)
	if got[0].OK || got[0].Category != models.ProbeErrorOther || got[0].Duration != 7*time.Millisecond {
		t.Fatalf("result 0 = %+v, want other with duration", got[0])
	}
	if got[1].OK || got[1].Category != models.ProbeErrorNoResult {
		t.Fatalf("result 1 = %+v, want the no-result category", got[1])
	}
}
