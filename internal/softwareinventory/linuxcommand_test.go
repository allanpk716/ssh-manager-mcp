package softwareinventory

import (
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// TestBuildInventoryCommandIsFixedTemplate 验证固定命令集框架的第一条：
// 命令集带版本号、只由固定模板组成，并按 shell 类型分发。本票只实现
// POSIX 的 Linux 命令集；Windows 命令集属于后续票。
func TestBuildInventoryCommandIsFixedTemplate(t *testing.T) {
	command, err := BuildInventoryCommand(models.ShellPOSIX)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(command, "M=SSHMGR-INVENTORY") ||
		!strings.Contains(command, `echo "$M BEGIN linux-v1"`) {
		t.Fatalf("固定命令集缺少带版本号的开头标记：\n%s", command)
	}
	for _, section := range linuxV1Sections {
		if !strings.Contains(command, `echo "$M SECTION `+section+`"`) {
			t.Fatalf("固定命令集缺少 %s 节标记", section)
		}
	}
	if !strings.Contains(command, `echo "$M END"`) {
		t.Fatal("固定命令集缺少结尾标记")
	}
}

func TestBuildInventoryCommandRejectsUnsupportedShells(t *testing.T) {
	for _, shell := range []models.ShellType{models.ShellType("unknown")} {
		if _, err := BuildInventoryCommand(shell); err == nil {
			t.Fatalf("shell 类型 %s 的固定命令集尚未实现，应返回错误", shell)
		}
	}
}
