package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/softwareinventory"
	"ssh-manager-mcp/internal/store"
)

// inventoryCollectService 构造采集服务。生产实现使用真实远程执行链路；测
// 试替换这个变量注入回放函数。
var inventoryCollectService = func(s *store.Store) *softwareinventory.Service {
	return &softwareinventory.Service{Store: s}
}

func inventoryCollectCmd() *cobra.Command {
	var device, entry string
	var jsonOutput bool
	c := &cobra.Command{
		Use:   "collect --device 设备 [--entry 服务器条目] [--json]",
		Short: "立即采集一台设备的软件清单",
		Long: `在权威端或单机上对一台 Ubuntu / Debian 设备立即执行固定命令集采集。
命令集随 sshmgr 发布并带版本号，不拼接任何来自 vault 或用户的自由文本，
也不经过命令策略判定。设备有多个服务器条目时可指定采集用条目；未指定
时优先用打开 sudo 采集开关的条目，否则用名字排序第一个。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := requireInventoryVault()
			if err != nil {
				return err
			}
			defer s.Close()
			status, err := s.SoftwareCollectionStatusForDevice(device)
			if err != nil {
				return err
			}
			if status.Collector != "" && status.Collector != "authority" {
				if err := s.RequestSoftwareImmediateCollection(device, store.CLISource()); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "设备 %s 已请求立即采集，等待客户端实例 %s 执行。\n", device, status.Collector)
				return nil
			}
			report, err := inventoryCollectService(s).Collect(cmd.Context(), softwareinventory.CollectRequest{
				DeviceName: device, ServerEntry: entry,
			})
			if err != nil {
				return err
			}
			return writeCollectReport(cmd.OutOrStdout(), report, jsonOutput)
		},
	}
	c.Flags().StringVar(&device, "device", "", "设备名")
	c.Flags().StringVar(&entry, "entry", "", "采集用的服务器条目（名字或标识符，可选）")
	c.Flags().BoolVar(&jsonOutput, "json", false, "以一行 JSON 输出采集报告")
	_ = c.MarkFlagRequired("device")
	return c
}

func inventorySudoCmd() *cobra.Command {
	var enable, disable bool
	c := &cobra.Command{
		Use:   "sudo 服务器条目 --enable|--disable",
		Args:  cobra.ExactArgs(1),
		Short: "设置服务器条目的 sudo 采集开关",
		Long: `sudo 采集开关默认关闭，存在软件清单自己的设置表里，不改变服务器条目。
打开后，用这个条目采集时会复用该条目已有的 sudo 包装（需要先配置 sudo 凭
据），可以看到更多进程名；关闭时命令按登录用户直接执行。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if enable == disable {
				return errors.New("必须且只能给出 --enable 或 --disable 之一")
			}
			s, err := requireInventoryVault()
			if err != nil {
				return err
			}
			defer s.Close()
			if err := s.SetSoftwareCollectSudo(args[0], enable, store.CLISource()); err != nil {
				return err
			}
			state := "已关闭"
			if enable {
				state = "已打开"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "服务器条目 %s 的 sudo 采集开关%s。\n", args[0], state)
			if enable {
				fmt.Fprintln(cmd.OutOrStdout(), "采集前请确认该条目已配置 sudo 凭据。")
			}
			return nil
		},
	}
	c.Flags().BoolVar(&enable, "enable", false, "允许用 sudo 执行固定采集命令")
	c.Flags().BoolVar(&disable, "disable", false, "固定采集命令不经 sudo 包装（默认）")
	return c
}

type collectReportJSON struct {
	DeviceName        string         `json:"device_name"`
	ServerEntryName   string         `json:"server_entry_name"`
	Sudo              bool           `json:"sudo"`
	CommandSetVersion string         `json:"command_set_version"`
	SystemType        string         `json:"system_type"`
	Incomplete        bool           `json:"incomplete"`
	Notes             []string       `json:"notes,omitempty"`
	ItemCounts        map[string]int `json:"item_counts"`
}

func writeCollectReport(w io.Writer, report *softwareinventory.CollectionReport, jsonOutput bool) error {
	if jsonOutput {
		counts := map[string]int{}
		for category, count := range report.ItemCounts {
			counts[category.Label()] = count
		}
		return json.NewEncoder(w).Encode(collectReportJSON{
			DeviceName: report.DeviceName, ServerEntryName: report.ServerEntryName,
			Sudo: report.Sudo, CommandSetVersion: report.CommandSetVersion,
			SystemType: report.SystemType, Incomplete: report.Incomplete,
			Notes: report.Notes, ItemCounts: counts,
		})
	}
	sudoText := "否"
	if report.Sudo {
		sudoText = "是"
	}
	fmt.Fprintf(w, "设备 %s 已采集（条目 %s，sudo %s，命令集 %s，系统 %s）\n",
		report.DeviceName, report.ServerEntryName, sudoText,
		report.CommandSetVersion, report.SystemType)
	for _, category := range []struct {
		label string
	}{
		{"操作系统"}, {"服务程序"}, {"容器镜像"}, {"驱动与固件"}, {"系统包"}, {"未识别进程"},
	} {
		if count := countForLabel(report.ItemCounts, category.label); count > 0 {
			fmt.Fprintf(w, "%s %d 条\n", category.label, count)
		}
	}
	if report.Incomplete {
		fmt.Fprintf(w, "结果不完整：%s\n", strings.Join(report.Notes, "；"))
	}
	return nil
}

func countForLabel(counts map[softwareinventory.Category]int, label string) int {
	for category, count := range counts {
		if category.Label() == label {
			return count
		}
	}
	return 0
}
