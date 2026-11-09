package softwareinventory

import (
	_ "embed"
	"fmt"

	"ssh-manager-mcp/internal/models"
)

// InventoryMarker 是软件清单固定命令集输出协议的行首标记。连通探测使用
// 同样的「行首固定标记」做法：命令的普通输出不可能伪造这些行。
const InventoryMarker = "SSHMGR-INVENTORY"

// LinuxCommandSetVersion 是当前 Linux 固定命令集的版本号。每次采集结果
// 记录所用版本，命令集升级后前后结果仍可比较。
const LinuxCommandSetVersion = "linux-v1"

// linuxV1Sections 是 Linux 命令集按顺序输出的全部节名。解析器用它发现
// 「整节缺失」的情况；某节缺失只影响该节，并把结果标为不完整。
var linuxV1Sections = []string{
	"os", "packages", "listen", "exe", "exe-owner",
	"container-images", "containers", "firmware", "gpu-driver",
}

// linuxCommandSetV1 是随二进制发布的 Linux 固定命令集原文（POSIX sh）。
// 它只由固定模板组成，不拼接任何来自 vault 或用户的自由文本；因此这里
// 用文件嵌入而不是字符串拼接，避免任何调用方在中间塞进别的内容。
//
//go:embed linux-v1.sh
var linuxCommandSetV1 string

// BuildInventoryCommand 按服务器条目的 shell 类型生成固定采集命令。命令
// 集随二进制发布并带版本号；本票只实现 POSIX（Ubuntu / Debian），其他
// shell 类型的命令集属于 Windows 票，未实现前明确报错。
func BuildInventoryCommand(shell models.ShellType) (string, error) {
	switch models.NormalizeShellType(string(shell)) {
	case models.ShellPOSIX:
		return linuxCommandSetV1, nil
	default:
		return "", fmt.Errorf("shell 类型 %q 的软件清单固定命令集尚未实现", shell)
	}
}
