package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/softwareinventory"
)

func inventoryHistoryCmd() *cobra.Command {
	var jsonOutput bool
	c := &cobra.Command{
		Use:   "history DEVICE [--json]",
		Args:  cobra.ExactArgs(1),
		Short: "Show one device's software inventory change history",
		Long: `查看一台设备的软件清单变化历史，按时间倒序排列。变化种类包括新增、
移除、版本变化与暴露级别变化；每次成功采集与上一次结果比对后写入，超过
一年的记录随采集结果清理。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := requireInventoryVault()
			if err != nil {
				return err
			}
			defer s.Close()
			entries, err := softwareinventory.New(s).ChangeHistory(
				args[0], softwareinventory.AllDevicesVisible(),
			)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(entries) == 0 {
				fmt.Fprintln(out, "没有软件清单变化历史。")
				return nil
			}
			for _, entry := range entries {
				if jsonOutput {
					if err := writeInventoryHistoryJSON(out, entry); err != nil {
						return err
					}
					continue
				}
				printInventoryHistoryEntry(out, entry)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&jsonOutput, "json", false, "以 JSON Lines 输出；每条变化一行")
	return c
}

// inventoryHistoryJSONRow 是变化历史的 JSON Lines 形态。旧值与新值按数据
// 库原样输出，暴露级别保持代号。
type inventoryHistoryJSONRow struct {
	DeviceName string                    `json:"device_name"`
	OccurredAt time.Time                 `json:"occurred_at"`
	Kind       models.SoftwareChangeKind `json:"kind"`
	KindLabel  string                    `json:"kind_label"`
	Category   models.SoftwareCategory   `json:"category"`
	Name       string                    `json:"name"`
	StableKey  string                    `json:"stable_key"`
	OldValue   string                    `json:"old_value,omitempty"`
	NewValue   string                    `json:"new_value,omitempty"`
}

func writeInventoryHistoryJSON(w io.Writer, entry softwareinventory.ChangeEntry) error {
	return json.NewEncoder(w).Encode(inventoryHistoryJSONRow{
		DeviceName: entry.DeviceName, OccurredAt: entry.OccurredAt,
		Kind: entry.Kind, KindLabel: entry.Kind.Label(),
		Category: entry.Category, Name: entry.Name, StableKey: entry.StableKey,
		OldValue: entry.OldValue, NewValue: entry.NewValue,
	})
}

func printInventoryHistoryEntry(w io.Writer, entry softwareinventory.ChangeEntry) {
	oldValue, newValue := entry.OldValue, entry.NewValue
	if entry.Kind == models.SoftwareChangeExposureChanged {
		oldValue = models.SoftwareExposure(oldValue).Label()
		newValue = models.SoftwareExposure(newValue).Label()
	}
	fmt.Fprintf(w, "%s %s %s %s", entry.OccurredAt.Format("2006-01-02 15:04:05"),
		entry.Kind.Label(), entry.Category.Label(), entry.Name)
	switch {
	case oldValue != "" && newValue != "":
		fmt.Fprintf(w, "：%s -> %s", oldValue, newValue)
	case newValue != "":
		fmt.Fprintf(w, "：%s", newValue)
	case oldValue != "":
		fmt.Fprintf(w, "：%s", oldValue)
	}
	fmt.Fprintln(w)
}

func inventoryStatusCmd() *cobra.Command {
	var jsonOutput bool
	c := &cobra.Command{
		Use:   "status [DEVICE] [--json]",
		Args:  cobra.MaximumNArgs(1),
		Short: "Show software inventory collection status",
		Long: `查看软件清单采集状态：采集执行者、上次成功时间、上次失败时间与原因、
上次结果是否不完整、固定命令集版本，以及是否清单过期。清单过期不存储，
每次读取时按「当前时间减上次成功时间超过 7 天」计算；从未成功采集的受
管设备也算过期，不受管设备不算。不带设备名时列出全部设备。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := requireInventoryVault()
			if err != nil {
				return err
			}
			defer s.Close()
			deviceName := ""
			if len(args) > 0 {
				deviceName = args[0]
			}
			statuses, err := softwareinventory.New(s).CollectionStatuses(
				softwareinventory.AllDevicesVisible(), deviceName,
			)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, status := range statuses {
				if jsonOutput {
					if err := json.NewEncoder(out).Encode(status); err != nil {
						return err
					}
					continue
				}
				printInventoryCollectionStatus(out, status)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&jsonOutput, "json", false, "以 JSON Lines 输出；每台设备一行")
	return c
}

func printInventoryCollectionStatus(w io.Writer, status softwareinventory.CollectionStatusView) {
	managedText := "不受管"
	if status.Managed {
		managedText = "受管"
	}
	fmt.Fprintf(w, "%s（%s）\n", status.DeviceName, managedText)
	{
		fmt.Fprintf(w, "  采集执行者：%s\n", status.CollectorLabel)
	}
	if status.ServerEntry != "" {
		fmt.Fprintf(w, "  采集用条目：%s\n", status.ServerEntry)
	}
	if status.LastSuccessAt != nil {
		fmt.Fprintf(w, "  上次成功：%s\n", status.LastSuccessAt.Format("2006-01-02 15:04:05"))
	} else {
		fmt.Fprintln(w, "  从未采集")
	}
	if status.LastFailureAt != nil {
		fmt.Fprintf(w, "  上次失败：%s：%s\n",
			status.LastFailureAt.Format("2006-01-02 15:04:05"), status.LastFailureReason)
	} else {
		fmt.Fprintln(w, "  上次失败：无")
	}
	if status.Incomplete {
		fmt.Fprintln(w, "  上次结果不完整")
	} else {
		fmt.Fprintln(w, "  上次结果完整")
	}
	if status.CommandSetVersion != "" {
		fmt.Fprintf(w, "  命令集版本 %s\n", status.CommandSetVersion)
	}
	switch {
	case status.Stale:
		fmt.Fprintln(w, "  清单过期：是（超过 7 天没有成功采集）")
	case !status.Managed && status.NeverCollected:
		fmt.Fprintln(w, "  清单过期：否（不受管设备不算过期）")
	default:
		fmt.Fprintln(w, "  清单过期：否（未过期）")
	}
}
