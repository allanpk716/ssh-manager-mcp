package softwareinventory

import (
	_ "embed"
	"fmt"

	"ssh-manager-mcp/internal/models"
)

// InventoryMarker 是软件清单固定命令集输出协议的行首标记。连通探测使用
// 同样的「行首固定标记」做法：命令的普通输出不可能伪造这些行。
const InventoryMarker = "SSHMGR-INVENTORY"

// POSIXCommandSetVersion 是所有 POSIX 系统共用的固定命令集版本号。每次
// 采集结果记录所用版本，命令集升级后前后结果仍可比较。
const POSIXCommandSetVersion = "posix-v2"

// posixCommandSetV2 是随二进制发布的 POSIX 固定命令集原文。它只由固定
// 模板组成，不拼接任何来自机密库或用户的自由文本。
//
//go:embed posix-v2.sh
var posixCommandSetV2 string

// BuildInventoryCommand 按服务器条目的 shell 类型生成固定采集命令。命令
// 集随二进制发布并带版本号；当前实现 POSIX 与 Windows，其他系统的命令
// 集未实现前明确报错。
func BuildInventoryCommand(shell models.ShellType) (string, error) {
	switch models.NormalizeShellType(string(shell)) {
	case models.ShellPOSIX:
		return posixCommandSetV2, nil
	case models.ShellPowerShell, models.ShellWindowsCmd:
		return buildWindowsInventoryCommand(shell), nil
	default:
		return "", fmt.Errorf("shell 类型 %q 的软件清单固定命令集尚未实现", shell)
	}
}
