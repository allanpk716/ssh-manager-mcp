package softwareinventory

import (
	"encoding/base64"
	"strings"
	"testing"
	"unicode/utf16"

	"ssh-manager-mcp/internal/models"
)

// TestBuildWindowsInventoryCommandPowerShell 验证 PowerShell 条目直接使用
// 固定命令集原文，并钉住开头设置、全部节标记与结尾标记。
func TestBuildWindowsInventoryCommandPowerShell(t *testing.T) {
	command, err := BuildInventoryCommand(models.ShellPowerShell)
	if err != nil {
		t.Fatal(err)
	}
	requiredTexts := []string{
		"[Console]::OutputEncoding=[Text.Encoding]::UTF8",
		"$ProgressPreference='SilentlyContinue'",
		`"$M BEGIN windows-v1"`,
		`"$M END"`,
	}
	for _, section := range []string{
		"os", "installed", "listen-tcp", "listen-udp",
		"processes", "services", "firmware", "gpu-driver",
	} {
		requiredTexts = append(requiredTexts, `Sec `+section)
	}
	for _, required := range requiredTexts {
		if !strings.Contains(command, required) {
			t.Fatalf("PowerShell 固定命令集缺少 %q：\n%s", required, command)
		}
	}
}

// TestBuildWindowsInventoryCommandExcludesWin32_Product 直接断言两种
// Windows 条目的生成结果都不包含会触发安装包修复的 Win32_Product 查询。
func TestBuildWindowsInventoryCommandExcludesWin32Product(t *testing.T) {
	for _, shell := range []models.ShellType{models.ShellPowerShell, models.ShellWindowsCmd} {
		command, err := BuildInventoryCommand(shell)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(command, "Win32_Product") {
			t.Fatalf("shell 类型 %s 的固定命令集不得使用 Win32_Product：\n%s", shell, command)
		}
	}
}

// TestBuildWindowsInventoryCommandWindowsCmdUsesEncodedCommand 验证 cmd 条目
// 使用 PowerShell 编码命令包装；解码后必须得到固定命令集原文，且整个命令
// 低于 cmd 的 8191 字符命令行上限。
func TestBuildWindowsInventoryCommandWindowsCmdUsesEncodedCommand(t *testing.T) {
	command, err := BuildInventoryCommand(models.ShellWindowsCmd)
	if err != nil {
		t.Fatal(err)
	}
	const prefix = "powershell -NoProfile -NonInteractive -EncodedCommand "
	if !strings.HasPrefix(command, prefix) {
		t.Fatalf("cmd 条目必须使用 PowerShell 编码命令包装：\n%s", command)
	}
	if len(command) >= 8191 {
		t.Fatalf("cmd 条目命令长度 %d 必须低于 8191 字符", len(command))
	}
	encoded := strings.TrimPrefix(command, prefix)
	blob, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("编码命令载荷不是 Base64：%v", err)
	}
	if len(blob) == 0 || len(blob)%2 != 0 {
		t.Fatalf("编码命令载荷不是 UTF-16LE 字节：长度 %d", len(blob))
	}
	units := make([]uint16, len(blob)/2)
	for i := range units {
		units[i] = uint16(blob[2*i]) | uint16(blob[2*i+1])<<8
	}
	decoded := string(utf16.Decode(units))
	powerShellCommand, err := BuildInventoryCommand(models.ShellPowerShell)
	if err != nil {
		t.Fatal(err)
	}
	if decoded != powerShellCommand {
		t.Fatalf("cmd 条目解码后的命令集原文不一致：\n%s", decoded)
	}
}
