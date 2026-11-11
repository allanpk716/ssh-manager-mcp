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
	var unmount, force, yes, newPasswordFromStdin bool
	var newPasswordFromFile string
	c := &cobra.Command{
		Use: "revoke SHARE SERVER [--unmount] [--force] [--new-password-stdin | " +
			"--new-password-from-file 路径] [--yes]",
		Args:  cobra.ExactArgs(2),
		Short: "Revoke one server's shared storage mount permission",
		Long: `把服务器移出共享存储的可挂载范围，并清理 Linux 或 macOS 服务器上的持久配置。

不带 --unmount 时保留现有挂载；带 --unmount 时被占用会列出进程，--force 表示强制卸载。
Windows 服务器不留持久配置，命令只改登记并说明盘符注入的生效边界。
需要安全撤销时可同时更换挂载密码；新密码只能来自标准输入或文件，不进入命令参数。
换密码后，仍在可挂载范围内的 Linux 与 macOS 服务器要等 agent 按新版本号重新挂载，桌面上手动添加的共享连接也需要更换密码。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := requireSharesVault()
			if err != nil {
				return err
			}
			defer s.Close()
			out := cmd.OutOrStdout()
			var newPassword []byte
			if newPasswordFromFile != "" || newPasswordFromStdin {
				password, err := readSharedStorageNewPassword(
					cmd, newPasswordFromFile, newPasswordFromStdin)
				if err != nil {
					return err
				}
				newPassword = password
				defer clearSharedStoragePassword(newPassword)
			}
			fmt.Fprintf(out, "将撤销共享存储 %s 在服务器 %s 上的可挂载范围。\n", args[0], args[1])
			if unmount {
				fmt.Fprintln(out, "Linux 与 macOS 服务器将删除远端持久配置并卸载挂载。")
			} else {
				fmt.Fprintln(out, "Linux 与 macOS 服务器将删除远端持久配置，现有挂载保留。")
			}
			if unmount && force {
				fmt.Fprintln(out, "被占用时将强制卸载。")
			}
			if newPassword != nil {
				fmt.Fprintln(out, "将同时更换共享存储挂载密码。")
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
				NewPassword: newPassword,
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
			if result.PasswordChanged {
				fmt.Fprintln(out, "已更换共享存储挂载密码。")
				if len(result.RemainingMountServers) == 0 {
					fmt.Fprintln(out, "没有仍可挂载的 Linux 与 macOS 服务器。")
				} else {
					fmt.Fprintf(out, "仍可挂载的 Linux 与 macOS 服务器：%s。这些服务器需要 agent 按新版本号调用 mount_share 更新挂载。\n",
						strings.Join(result.RemainingMountServers, "、"))
				}
			}
			for _, process := range result.BusyProcesses {
				fmt.Fprintf(out, "占用进程：%s %s %s\n", process.ProcessID, process.User, process.Command)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&unmount, "unmount", false, "登记撤销后同时卸载远端挂载")
	c.Flags().BoolVar(&force, "force", false, "被占用时强制卸载")
	c.Flags().StringVar(&newPasswordFromFile, "new-password-from-file", "",
		"从文件按字节原样读取新的共享存储挂载密码（末尾换行保留）")
	c.Flags().BoolVar(&newPasswordFromStdin, "new-password-stdin", false,
		"从标准输入读到结束并按字节原样保存新的共享存储挂载密码（末尾换行保留）")
	c.Flags().BoolVar(&yes, "yes", false, "跳过共享存储撤销确认；非交互终端必须显式给出")
	return c
}

func clearSharedStoragePassword(password []byte) {
	for index := range password {
		password[index] = 0
	}
}
