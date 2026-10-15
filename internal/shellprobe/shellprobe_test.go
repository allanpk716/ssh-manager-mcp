package shellprobe

// 命令策略诊断面(ticket #17)的判定内核测试:Detect 是「探测输出 →
// 实际 shell 类型」的纯函数,表格驱动覆盖三类 shell 的真实探测输出形态
// 与无法判定的兜底。连接与命令执行的接线(ProbeServer)在 CLI 层用
// 进程内测试 sshd 另测,不在这里。

import (
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// psVersionTableOutput 是 Windows PowerShell 5.1 对 `echo $PSVersionTable`
// 的真实输出形态(哈希表默认格式化;换行为 CRLF)。
const psVersionTableOutput = "Name                           Value\r\n" +
	"----                           -----\r\n" +
	"PSVersion                      5.1.26100.1\r\n" +
	"PSEdition                      Desktop\r\n" +
	"PSCompatibleVersions           {1.0, 2.0, 3.0, 4.0}\r\n" +
	"BuildVersion                   10.0.26100.1\r\n" +
	"CLRVersion                     4.0.30319.42000\r\n" +
	"WSManStackVersion              3.0\r\n" +
	"PSRemotingProtocolVersion      2.3\r\n" +
	"SerializationVersion           1.1.0.1\r\n"

func TestDetect(t *testing.T) {
	cases := []struct {
		name     string
		probes   Probes
		want     models.ShellType
		wantEvIn string // evidence 里应出现的关键词("" 只断言非空)
	}{
		{
			// Windows cmd:%OS% 是系统内建环境变量,只有 cmd 会展开。
			name:     "cmd expands percent variable",
			probes:   Probes{OSExpand: "Windows_NT\r\n", PSVar: "$PSVersionTable\r\n"},
			want:     models.ShellWindowsCmd,
			wantEvIn: "Windows_NT",
		},
		{
			// 环境变量值的大小写不由 shell 决定,判定不区分大小写。
			name:   "cmd lowercase expansion still cmd",
			probes: Probes{OSExpand: "windows_nt\n", PSVar: ""},
			want:   models.ShellWindowsCmd,
		},
		{
			// PowerShell:$PSVersionTable 是内建变量,打印出版本表;
			// %OS% 在 PowerShell 里原样打印。
			name:     "powershell prints version table",
			probes:   Probes{OSExpand: "%OS%\r\n", PSVar: psVersionTableOutput},
			want:     models.ShellPowerShell,
			wantEvIn: "PSVersion",
		},
		{
			// POSIX shell:两个探测都原样/空——$PSVersionTable 未定义,
			// 展开为空(echo 打印空行)。
			name:     "posix expands undefined variable empty",
			probes:   Probes{OSExpand: "%OS%\n", PSVar: "\n"},
			want:     models.ShellPOSIX,
			wantEvIn: "undefined",
		},
		{
			// POSIX shell、输出连换行都没有的极端形态。
			name:   "posix truly empty variable output",
			probes: Probes{OSExpand: "%OS%", PSVar: ""},
			want:   models.ShellPOSIX,
		},
		{
			// 两边都不符合三类已知 shell(如 %OS% 未定义的 cmd、或未
			// 预期的第四种 shell):如实返回无法判定,不猜。字面量里也含
			// "PSVersion" 一词——单靠它不能算 PowerShell,得看整张表。
			name:   "unknown shell stays undetermined",
			probes: Probes{OSExpand: "%OS%\n", PSVar: "$PSVersionTable\n"},
			want:   "",
		},
		{
			// 输出带前后空白(某些 shell 的 echo 行为)不影响判定。
			name:   "surrounding whitespace tolerated",
			probes: Probes{OSExpand: "  Windows_NT  \r\n", PSVar: "  \r\n"},
			want:   models.ShellWindowsCmd,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, evidence := Detect(c.probes)
			if got != c.want {
				t.Fatalf("Detect(%+v) = %q, want %q (evidence: %s)", c.probes, got, c.want, evidence)
			}
			if strings.TrimSpace(evidence) == "" {
				t.Fatalf("Detect(%+v): evidence must never be empty — the owner needs the reason", c.probes)
			}
			if c.wantEvIn != "" && !strings.Contains(evidence, c.wantEvIn) {
				t.Fatalf("evidence %q missing %q", evidence, c.wantEvIn)
			}
		})
	}
}
