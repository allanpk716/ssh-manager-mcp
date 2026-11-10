package cli

import (
	"errors"

	"github.com/spf13/cobra"
)

// tuiRemovedNotice 是旧终端图形界面命令的迁移提示。终端图形界面已在
// Web UI 完全覆盖后删除，这条提示长期保留，让老用户升级后知道新入口。
const tuiRemovedNotice = "TUI 已删除，人工操作请用 `sshmgr ui`，命令行等价命令见 `sshmgr --help`"

// newTUICmd 构造隐藏的 `sshmgr tui`：关闭参数解析，接受旧命令的任何参数
// （含 `--mode` 的任意取值），一律只打印迁移提示并以退出码 1 结束。它不是
// `sshmgr ui` 的别名——不打开浏览器、不监听端口、不读标准输入。
func newTUICmd() *cobra.Command {
	return &cobra.Command{
		Use:                "tui",
		Short:              "Removed: the terminal interface was replaced by `sshmgr ui`",
		Hidden:             true,
		DisableFlagParsing: true,
		Args:               cobra.ArbitraryArgs,
		SilenceErrors:      true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return errors.New(tuiRemovedNotice)
		},
	}
}
