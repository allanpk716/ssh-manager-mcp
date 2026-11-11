package cli

import (
	"bufio"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/sharemanage"
)

func sharesRevokeCmd() *cobra.Command {
	var unmount, force, yes bool
	c := &cobra.Command{
		Use:   "revoke SHARE SERVER [--unmount] [--force] [--yes]",
		Args:  cobra.ExactArgs(2),
		Short: "Revoke one server's shared storage mount permission",
		Long: `把服务器移出共享存储的可挂载范围，并清理 Linux 或 macOS 服务器上的持久配置。

不带 --unmount 时保留现有挂载；带 --unmount 时被占用会列出进程，--force 表示强制卸载。
Windows 服务器不留持久配置，命令只改登记并说明盘符注入的生效边界。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := requireSharesVault()
			if err != nil {
				return err
			}
			defer s.Close()
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "将撤销共享存储 %s 在服务器 %s 上的可挂载范围。\n", args[0], args[1])
			if unmount {
				fmt.Fprintln(out, "Linux 与 macOS 服务器将删除远端持久配置并卸载挂载。")
			} else {
				fmt.Fprintln(out, "Linux 与 macOS 服务器将删除远端持久配置，现有挂载保留。")
			}
			if unmount && force {
				fmt.Fprintln(out, "被占用时将强制卸载。")
			}
			fmt.Fprintln(out, "Windows 服务器只改登记，不发起连接。")
			if !yes {
				if !sharesStdinIsInteractive() {
					return errors.New("非交互终端不能确认共享存储撤销；请加 --yes 后重跑")
				}
				fmt.Fprint(out, "输入 y 确认共享存储撤销：")
				line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				if err != nil && line == "" {
					return fmt.Errorf("读取共享存储撤销确认失败: %w", err)
				}
				if strings.ToLower(strings.TrimSpace(line)) != "y" {
					fmt.Fprintln(out, "\n已取消，未撤销共享存储。")
					return nil
				}
			}
			result, err := sharemanage.New(s).Revoke(cmd.Context(), sharemanage.RevokeRequest{
				Share: args[0], Server: args[1], Unmount: unmount, Force: force,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "已撤销共享存储 %s 在服务器 %s 上的可挂载范围；版本号：%d\n",
				result.ShareName, result.ServerName, result.Version)
			if result.Result == "success" {
				if result.System == "Windows" {
					fmt.Fprintln(out, "未连接 Windows 服务器；各客户端同步快照后停止注入。")
				} else if result.Unmount {
					fmt.Fprintln(out, "远端持久配置已删除，挂载已卸载。")
				} else {
					fmt.Fprintln(out, "远端持久配置已删除，现有挂载保留。")
				}
			} else {
				fmt.Fprintf(out, "远端持久配置已删除，挂载被占用并保留；结果：%s\n", result.Result)
			}
			for _, notice := range result.Notices {
				fmt.Fprintf(out, "提示：%s\n", notice)
			}
			for _, process := range result.BusyProcesses {
				fmt.Fprintf(out, "占用进程：%s %s %s\n", process.ProcessID, process.User, process.Command)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&unmount, "unmount", false, "登记撤销后同时卸载远端挂载")
	c.Flags().BoolVar(&force, "force", false, "被占用时强制卸载")
	c.Flags().BoolVar(&yes, "yes", false, "跳过共享存储撤销确认；非交互终端必须显式给出")
	return c
}
