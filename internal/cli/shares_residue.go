package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/sharemanage"
)

func sharesResidueCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "residue [SHARE]",
		Args:  cobra.MaximumNArgs(1),
		Short: "Check Linux and macOS servers for shared-storage leftovers",
		Long: `逐台检测 vault 里的 Linux 与 macOS 服务器，列出不在可挂载范围内、却仍留有凭据文件、挂载配置或挂载的共享存储。

只读检测，不改动任何服务器；清理用 shares unmount。Windows 服务器会被跳过：sshmgr 不在其上留下任何东西。
不带共享名时检查全部共享；带上共享名时只报告该共享。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := requireSharesVault()
			if err != nil {
				return err
			}
			defer s.Close()
			request := sharemanage.ResidueRequest{}
			if len(args) == 1 {
				request.Share = args[0]
			}
			report, err := sharemanage.New(s).Residue(cmd.Context(), request)
			if err != nil {
				return err
			}
			printResidueReport(cmd.OutOrStdout(), request.Share, report)
			return nil
		},
	}
	return c
}

func printResidueReport(out io.Writer, filter string, report *sharemanage.ResidueReport) {
	if filter != "" {
		fmt.Fprintf(out, "共享存储残留检查：只检查 %s。\n", filter)
	} else {
		fmt.Fprintln(out, "共享存储残留检查：逐台检测 Linux 与 macOS 服务器。")
	}
	counts := map[string]int{}
	for _, result := range report.Servers {
		counts[result.Status]++
		switch result.Status {
		case sharemanage.ResidueStatusResidue:
			fmt.Fprintf(out, "服务器 %s（%s）：发现残留\n", result.ServerName, residueSystemLabel(result.System))
		case sharemanage.ResidueStatusMarkerOnly:
			fmt.Fprintf(out, "服务器 %s（%s）：仅有撤销标记（撤销后的预期状态，不需要清理）\n",
				result.ServerName, residueSystemLabel(result.System))
		case sharemanage.ResidueStatusUnchecked:
			fmt.Fprintf(out, "服务器 %s：未检查（%s）\n", result.ServerName, result.Notice)
		case sharemanage.ResidueStatusSkipped:
			fmt.Fprintf(out, "服务器 %s：跳过（%s）\n", result.ServerName, result.Notice)
		default:
			fmt.Fprintf(out, "服务器 %s（%s）：无残留\n", result.ServerName, residueSystemLabel(result.System))
		}
		for _, finding := range result.Findings {
			fmt.Fprintf(out, "  %s（%s）：%s\n", finding.ShareName,
				residueReasonLabel(finding.Reason), residueFindingLabels(finding))
			if finding.CleanupCommand != "" {
				fmt.Fprintf(out, "  清理命令：%s\n", finding.CleanupCommand)
			}
		}
	}
	fmt.Fprintf(out, "汇总：%d 台有残留，%d 台仅有撤销标记，%d 台未检查，%d 台无残留，%d 台跳过。\n",
		counts[sharemanage.ResidueStatusResidue], counts[sharemanage.ResidueStatusMarkerOnly],
		counts[sharemanage.ResidueStatusUnchecked], counts[sharemanage.ResidueStatusNoResidue],
		counts[sharemanage.ResidueStatusSkipped])
}

func residueSystemLabel(system string) string {
	switch system {
	case "linux":
		return "Linux"
	case "darwin":
		return "macOS"
	case "windows":
		return "Windows"
	default:
		return "系统未知"
	}
}

func residueReasonLabel(reason string) string {
	switch reason {
	case sharemanage.ResidueReasonShareUnregistered:
		return "共享登记已删除"
	case sharemanage.ResidueReasonServerOutOfRange:
		return "服务器不在可挂载范围内"
	default:
		return "无需清理"
	}
}

func residueFindingLabels(finding sharemanage.ResidueFinding) string {
	if finding.RevokedMarkerOnly {
		return "仅有撤销标记"
	}
	labels := make([]string, 0, 4)
	if finding.HasConfigEntry {
		labels = append(labels, "配置条目")
	}
	if finding.HasCredentialFile {
		labels = append(labels, "凭据文件")
	}
	if finding.HasMount {
		labels = append(labels, "挂载")
	}
	if finding.HasRevokedMarker {
		labels = append(labels, "撤销标记")
	}
	if len(labels) == 0 {
		return "现场状态未知"
	}
	return strings.Join(labels, "、")
}
