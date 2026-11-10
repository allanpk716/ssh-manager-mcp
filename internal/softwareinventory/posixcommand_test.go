package softwareinventory

import (
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// TestBuildPOSIXInventoryCommandIsFixedTemplate 覆盖所有 POSIX 服务器条目
// 共用的固定命令集：版本号是 posix-v2，并按协议输出新增的系统识别、
// netstat、lsof、macOS 应用与 Homebrew、群晖套件节。
func TestBuildPOSIXInventoryCommandIsFixedTemplate(t *testing.T) {
	command, err := BuildInventoryCommand(models.ShellPOSIX)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(command, `echo "$M BEGIN posix-v2"`) {
		t.Fatalf("固定命令集版本应为 posix-v2，命令开头：\n%s", firstLines(command, 3))
	}
	for _, section := range []string{
		"os", "listen-netstat", "listen-lsof", "apps", "brew", "synology-packages",
	} {
		if !strings.Contains(command, `echo "$M SECTION `+section+`"`) {
			t.Fatalf("固定命令集缺少 %s 节", section)
		}
	}
	if strings.Contains(command, "Win32_Product") {
		t.Fatal("固定命令集不应包含有副作用的 Win32_Product 查询")
	}
}

// TestBuildInventoryCommandRejectsUnsupportedShells 保持命令生成的显式边
// 界：没有固定命令集的 shell 不能静默退回 POSIX 命令。
func TestBuildInventoryCommandRejectsUnsupportedShells(t *testing.T) {
	if _, err := BuildInventoryCommand(models.ShellType("unknown")); err == nil {
		t.Fatal("未知 shell 类型应报错")
	}
}

func firstLines(value string, count int) string {
	lines := strings.Split(value, "\n")
	if len(lines) < count {
		count = len(lines)
	}
	return strings.Join(lines[:count], "\n")
}
