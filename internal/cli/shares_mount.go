package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/sharemanage"
	"ssh-manager-mcp/internal/sharemount"
)

func sharesMountCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mount SHARE SERVER",
		Args:  cobra.ExactArgs(2),
		Short: "Mount one shared storage on a Linux or macOS server",
		Long: `在一台 Linux 或 macOS 服务器上执行幂等挂载，不要求确认。

命令与 agent 挂载工具和 Web UI 挂载按钮使用同一套共享存储挂载服务。
Windows 服务器不需要本命令：网络盘符由每条命令执行前的会话内注入提供。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := requireSharesVault()
			if err != nil {
				return err
			}
			defer s.Close()
			result, err := sharemanage.New(s).Mount(cmd.Context(), sharemanage.MountRequest{
				Share: args[0], Server: args[1], Source: sharemanage.CLISource(),
			})
			if err != nil {
				return err
			}
			printSharesMountResult(cmd.OutOrStdout(), result)
			return nil
		},
	}
}

func printSharesMountResult(out io.Writer, result *sharemanage.MountResult) {
	if result.Action == "no-change" {
		fmt.Fprintf(out, "共享存储 %s 在服务器 %s 上无需改动；版本号：%d\n",
			result.ShareName, result.ServerName, result.Version)
	} else {
		fmt.Fprintf(out, "已挂载共享存储 %s；服务器 %s；结果：%s；版本号：%d\n",
			result.ShareName, result.ServerName, result.Action, result.Version)
	}
	fmt.Fprintf(out, "挂载点：%s\n", result.MountPoint)
	fmt.Fprintf(out, "实际网络：%s（第 %d 个地址）\n", result.Network, result.NetworkSequence)
	for _, notice := range result.Notices {
		fmt.Fprintf(out, "提示：%s\n", notice)
	}
}

func printSharedStorageCheck(out io.Writer, report *sharemanage.CheckReport) {
	fmt.Fprintf(out, "共享存储 %s 现场探测：\n", report.ShareName)
	for _, result := range report.Servers {
		fmt.Fprintf(out, "服务器 %s：%s；系统：%s；观察时间：%s\n",
			result.ServerName,
			sharesCheckStatusLabel(result.Status),
			sharesCheckSystemLabel(result.System),
			result.ObservedAt.Format(time.RFC3339),
		)
		if result.Restricted {
			fmt.Fprintln(out, "  检测方式：受限检测")
		}
		if result.Managed {
			fmt.Fprintf(out, "  远端配置版本号：%d\n", result.Version)
		}
		if result.Notice != "" {
			fmt.Fprintf(out, "  %s\n", result.Notice)
		}
	}
}

func sharesCheckStatusLabel(status string) string {
	switch status {
	case sharemanage.CheckStatusMounted:
		return "已挂载"
	case sharemanage.CheckStatusUnmounted:
		return "未挂载"
	case sharemanage.CheckStatusSkipped:
		return "不检测"
	default:
		return "不可达"
	}
}

func sharesCheckSystemLabel(system string) string {
	switch system {
	case sharemount.CheckSystemLinux:
		return "Linux"
	case sharemount.CheckSystemDarwin:
		return "macOS"
	case sharemount.CheckSystemWindows:
		return "Windows"
	default:
		return "系统未知"
	}
}
