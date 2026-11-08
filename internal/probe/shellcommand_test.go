package probe

import (
	"encoding/base64"
	"encoding/binary"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

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
		if shell == models.ShellWindowsCmd {
			command = decodeWindowsProbeCommand(t, command)
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

// TestBuildShellProbeCommandWindowsCmdUsesEncodedCommand: cmd 分支用
// -EncodedCommand 传送按 UTF-16LE 编码的 Base64 脚本；解码后应得到带换
// 行的完整原脚本，而不是被 cmd 命令行截断的形式。
func TestBuildShellProbeCommandWindowsCmdUsesEncodedCommand(t *testing.T) {
	command, err := BuildShellProbeCommand(models.ShellWindowsCmd, commandItems())
	if err != nil {
		t.Fatal(err)
	}
	const prefix = "powershell -NoProfile -NonInteractive -EncodedCommand "
	if !strings.HasPrefix(command, prefix) {
		t.Fatalf("cmd command must use EncodedCommand:\n%s", command)
	}
	script := decodeWindowsProbeCommand(t, command)
	if !strings.Contains(script, "\n") {
		t.Fatalf("decoded script must keep newlines:\n%s", script)
	}
	for _, want := range []string{
		"function probe-one",
		"probe-one 0 192.0.2.10 445 1500",
		"probe-one 1 nas-a.local 22 1500",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("decoded script missing %q:\n%s", want, script)
		}
	}
}

// decodeWindowsProbeCommand 解码 cmd 分支的 -EncodedCommand 载荷，供测试
// 断言脚本内容。
func decodeWindowsProbeCommand(t *testing.T, command string) string {
	t.Helper()
	const prefix = "powershell -NoProfile -NonInteractive -EncodedCommand "
	if !strings.HasPrefix(command, prefix) {
		t.Fatalf("cmd command must use EncodedCommand:\n%s", command)
	}
	blob, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(command, prefix))
	if err != nil {
		t.Fatalf("EncodedCommand payload is not Base64: %v", err)
	}
	if len(blob) == 0 || len(blob)%2 != 0 {
		t.Fatalf("EncodedCommand payload is not UTF-16LE bytes: %d bytes", len(blob))
	}
	units := make([]uint16, len(blob)/2)
	for index := range units {
		units[index] = binary.LittleEndian.Uint16(blob[index*2:])
	}
	return string(utf16.Decode(units))
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
		{"address label starting with dash", []ShellCommandItem{{Address: "-nas.local", Port: 445, Timeout: time.Second}}},
		{"address label ending with dash", []ShellCommandItem{{Address: "nas-.local", Port: 445, Timeout: time.Second}}},
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

// TestBuildShellProbeCommandPOSIXTimeoutWithoutScientificNotation: 超时超
// 过 1000 秒时，POSIX 命令仍使用十进制写法，避免 nc 收到 1e+03 这类值。
func TestBuildShellProbeCommandPOSIXTimeoutWithoutScientificNotation(t *testing.T) {
	command, err := BuildShellProbeCommand(models.ShellPOSIX, []ShellCommandItem{
		{Address: "192.0.2.10", Port: 445, Timeout: 1001 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(command, "sshmgr_probe_one 0 192.0.2.10 445 1001") {
		t.Fatalf("posix command must format the timeout as 1001:\n%s", command)
	}
	if strings.Contains(command, "e+") {
		t.Fatalf("posix command must not use scientific notation:\n%s", command)
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
