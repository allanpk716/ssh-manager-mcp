package softwareinventory

import (
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"unicode/utf16"

	"ssh-manager-mcp/internal/models"
)

// WindowsCommandSetVersion 是当前 Windows 固定命令集的版本号。
const WindowsCommandSetVersion = "windows-v1"

// windowsV1Sections 是 Windows 命令集按顺序输出的全部节名。
var windowsV1Sections = []string{
	"os", "installed", "listen-tcp", "listen-udp",
	"processes", "services", "firmware", "gpu-driver",
}

// windowsCommandSetV1 是随二进制发布的 Windows 固定命令集原文
// （PowerShell 5.1 可用）。与 Linux 命令集一样，它不拼接任何来自凭据存
// 储或调用方的自由文本。
//
//go:embed windows-v1.ps1
var windowsCommandSetV1 string

// buildWindowsInventoryCommand 按 Windows 服务器条目的 shell 类型返回固
// 定采集命令。
func buildWindowsInventoryCommand(shell models.ShellType) string {
	if models.NormalizeShellType(string(shell)) == models.ShellWindowsCmd {
		return encodeWindowsPowerShellCommand(windowsCommandSetV1)
	}
	return windowsCommandSetV1
}

// encodeWindowsPowerShellCommand 把 PowerShell 脚本按 UTF-16LE 小端编码
// 后再做 Base64，作为 cmd 条目的 -EncodedCommand 参数。cmd 的命令行在换
// 行处会截断，这个包装可以避开换行与引号转义。
func encodeWindowsPowerShellCommand(script string) string {
	units := utf16.Encode([]rune(script))
	blob := make([]byte, len(units)*2)
	for index, unit := range units {
		binary.LittleEndian.PutUint16(blob[index*2:], unit)
	}
	return "powershell -NoProfile -NonInteractive -EncodedCommand " +
		base64.StdEncoding.EncodeToString(blob)
}
