package cli

import (
	"bufio"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/sharemanage"
)

func sharesReselectCmd() *cobra.Command {
	var force, yes bool
	c := &cobra.Command{
		Use:   "reselect SHARE SERVER [--force] [--yes]",
		Args:  cobra.ExactArgs(2),
		Short: "Re-select one shared storage address on a Linux or macOS server",
		Long: `让一台仍在可挂载范围内的 Linux 或 macOS 服务器重新选择共享存储地址。

候选顺序与挂载工具一致：物理局域网地址优先，同级按登记顺序，并在目标服务器上探测 TCP 445。
当前地址已是最优可达地址时不做改动。切换时被占用的挂载会保留；加 --force 才强制卸载后重挂。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := requireSharesVault()
			if err != nil {
				return err
			}
			defer s.Close()
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "将重选共享存储 %s 在服务器 %s 上的访问地址。\n", args[0], args[1])
			fmt.Fprintln(out, "物理局域网中仍可达的更优地址会被优先选用。")
			if force {
				fmt.Fprintln(out, "被占用时将强制卸载后重新挂载。")
			}
			if !yes {
				if !sharesStdinIsInteractive() {
					return errors.New("非交互终端不能确认共享存储重选地址；请加 --yes 后重跑")
				}
				fmt.Fprint(out, "输入 y 确认共享存储重选地址：")
				line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				if err != nil && line == "" {
					return fmt.Errorf("读取共享存储重选地址确认失败: %w", err)
				}
				if strings.ToLower(strings.TrimSpace(line)) != "y" {
					fmt.Fprintln(out, "\n已取消，未重选共享存储地址。")
					return nil
				}
			}
			result, err := sharemanage.New(s).Reselect(cmd.Context(), sharemanage.ReselectRequest{
				Share: args[0], Server: args[1], Force: force,
			})
			if err != nil {
				return err
			}
			switch result.Result {
			case "best-address-unchanged":
				fmt.Fprintf(out, "共享存储 %s 在服务器 %s 上的当前地址已是最优可达地址，未做改动。\n",
					result.ShareName, result.ServerName)
			case "busy":
				fmt.Fprintf(out, "服务器 %s 上挂载被占用，共享存储 %s 保留旧地址与旧配置。\n",
					result.ServerName, result.ShareName)
			default:
				fmt.Fprintf(out, "共享存储 %s 已在服务器 %s 上重选地址；网络：%s；版本号：%d\n",
					result.ShareName, result.ServerName, result.NetworkName, result.Version)
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
	c.Flags().BoolVar(&force, "force", false, "被占用时强制卸载后重新挂载")
	c.Flags().BoolVar(&yes, "yes", false, "跳过共享存储重选地址确认；非交互终端必须显式给出")
	return c
}
