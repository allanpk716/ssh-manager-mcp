package cli

// 票 #188：owner 审阅声明草稿的命令行。草稿由 agent 的 MCP 工具提交；
// 列出、确认、修改后确认与驳回都在这里完成，写审计并记录来源。

import (
	"encoding/json"
	"fmt"
	"github.com/spf13/pflag"
	"io"
	"time"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/softwareinventory"
)

// inventoryDraftsCmd 列出待 owner 审阅的声明草稿。
func inventoryDraftsCmd() *cobra.Command {
	var jsonOutput bool
	c := &cobra.Command{
		Use:   "drafts [--json]",
		Args:  cobra.NoArgs,
		Short: "List declaration drafts waiting for review",
		Long: `列出等待 owner 审阅的声明草稿。草稿由 agent 提交，在确认前不影响任
何查询结果与待办；确认后成为生效声明，驳回后消失。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := requireInventoryVault()
			if err != nil {
				return err
			}
			defer s.Close()
			drafts, err := softwareinventory.New(s).DeclarationDrafts(
				softwareinventory.AllDevicesVisible(),
			)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if jsonOutput {
				for _, draft := range drafts {
					encoder := json.NewEncoder(out)
					if err := encoder.Encode(newInventoryDraftJSON(draft)); err != nil {
						return err
					}
				}
				return nil
			}
			if len(drafts) == 0 {
				fmt.Fprintln(out, "没有待审阅的声明草稿。")
				return nil
			}
			for _, draft := range drafts {
				printInventoryDraft(out, draft)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&jsonOutput, "json", false, "以 JSON Lines 输出；每个草稿一行")
	return c
}

// inventoryDraftJSON 是草稿的 JSON 输出形态。
type inventoryDraftJSON struct {
	ID                 string                            `json:"id"`
	DeviceName         string                            `json:"device_name"`
	Category           softwareinventory.Category        `json:"category"`
	CategoryLabel      string                            `json:"category_label"`
	Name               string                            `json:"name"`
	Version            string                            `json:"version"`
	PackageURL         string                            `json:"purl,omitempty"`
	CPE                string                            `json:"cpe,omitempty"`
	ListeningPorts     []softwareinventory.ListeningPort `json:"listening_ports,omitempty"`
	BelongsTo          string                            `json:"belongs_to,omitempty"`
	Note               string                            `json:"note,omitempty"`
	ClaimedObservedKey string                            `json:"claimed_observed_key,omitempty"`
	Status             string                            `json:"status"`
	CreatedAt          time.Time                         `json:"created_at"`
	UpdatedAt          time.Time                         `json:"updated_at"`
}

func newInventoryDraftJSON(draft softwareinventory.SoftwareDeclarationView) inventoryDraftJSON {
	return inventoryDraftJSON{
		ID: draft.ID, DeviceName: draft.DeviceName, Category: draft.Category,
		CategoryLabel: draft.Category.Label(), Name: draft.Name, Version: draft.Version,
		PackageURL: draft.PackageURL, CPE: draft.CPE, ListeningPorts: draft.ListeningPorts,
		BelongsTo: draft.BelongsTo, Note: draft.Note,
		ClaimedObservedKey: draft.ClaimedObservedKey, Status: draft.Status,
		CreatedAt: draft.CreatedAt, UpdatedAt: draft.UpdatedAt,
	}
}

func printInventoryDraft(w io.Writer, draft softwareinventory.SoftwareDeclarationView) {
	fmt.Fprintf(w, "草稿 %s\n", draft.ID)
	fmt.Fprintf(w, "  设备：%s\n", draft.DeviceName)
	fmt.Fprintf(w, "  %s %s（%s）\n", draft.Category.Label(), draft.Name, draft.Version)
	if draft.PackageURL != "" {
		fmt.Fprintf(w, "  软件包统一地址：%s\n", draft.PackageURL)
	}
	if draft.CPE != "" {
		fmt.Fprintf(w, "  美国国家漏洞库产品命名：%s\n", draft.CPE)
	}
	for _, port := range draft.ListeningPorts {
		fmt.Fprintf(w, "  监听端口：%s:%d/%s（%s）\n",
			port.Address, port.Port, port.Protocol, port.Exposure.Label())
	}
	if draft.BelongsTo != "" {
		fmt.Fprintf(w, "  所属：%s\n", draft.BelongsTo)
	}
	if draft.Note != "" {
		fmt.Fprintf(w, "  备注：%s\n", draft.Note)
	}
	if draft.ClaimedObservedKey != "" {
		fmt.Fprintf(w, "  认领的实测稳定键：%s\n", draft.ClaimedObservedKey)
	}
	fmt.Fprintf(w, "  提交时间：%s\n", draft.CreatedAt.Format(time.RFC3339))
}

// inventoryDraftConfirmCmd 确认一条草稿；带修改参数时为「修改后确认」。
func inventoryDraftConfirmCmd() *cobra.Command {
	var device, category, name, version, packageURL, cpe, belongsTo, note string
	var listens []string
	c := &cobra.Command{
		Use:   "draft-confirm ID [--device 设备] [--category 类别] ...",
		Args:  cobra.ExactArgs(1),
		Short: "Confirm a declaration draft, optionally with edits",
		Long: `确认一条声明草稿。不带修改参数时按草稿原样生效；给出任意修改参数时
先修改再生效（修改后确认）。可用修改参数与 inventory edit 相同。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			edit := softwareinventory.DeclarationEdit{}
			if cmd.Flags().Changed("device") {
				edit.Device = &device
			}
			if cmd.Flags().Changed("category") {
				parsed, err := parseInventoryCategory(category)
				if err != nil {
					return err
				}
				edit.Category = &parsed
			}
			if cmd.Flags().Changed("name") {
				edit.Name = &name
			}
			if cmd.Flags().Changed("version") {
				edit.Version = &version
			}
			if cmd.Flags().Changed("purl") {
				edit.PackageURL = &packageURL
			}
			if cmd.Flags().Changed("cpe") {
				edit.CPE = &cpe
			}
			if cmd.Flags().Changed("listen") {
				ports, err := parseInventoryListens(listens)
				if err != nil {
					return err
				}
				edit.ListeningPorts = &ports
			}
			if cmd.Flags().Changed("belongs-to") {
				edit.BelongsTo = &belongsTo
			}
			if cmd.Flags().Changed("note") {
				edit.Note = &note
			}
			s, err := requireInventoryVault()
			if err != nil {
				return err
			}
			defer s.Close()
			if err := softwareinventory.New(s).ConfirmDeclarationDraft(
				args[0], &edit, softwareinventory.CLISource(),
			); err != nil {
				return err
			}
			mode := "已确认"
			if editFlagsChanged(cmd) {
				mode = "已修改后确认"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "声明草稿 %s %s。\n", args[0], mode)
			return nil
		},
	}
	c.Flags().StringVar(&device, "device", "", "设备")
	c.Flags().StringVar(&category, "category", "", "软件类别")
	c.Flags().StringVar(&name, "name", "", "软件名字")
	c.Flags().StringVar(&version, "version", "", "精确版本；原样保存")
	c.Flags().StringVar(&packageURL, "purl", "", "软件包统一地址")
	c.Flags().StringVar(&cpe, "cpe", "", "美国国家漏洞库产品命名")
	c.Flags().StringArrayVar(&listens, "listen", nil, "监听端口：监听地址,端口,协议,暴露级别；可重复")
	c.Flags().StringVar(&belongsTo, "belongs-to", "", "所属")
	c.Flags().StringVar(&note, "note", "", "备注")
	return c
}

// editFlagsChanged 判断这次确认是否带了修改参数（第一个位置参数之外）。
func editFlagsChanged(cmd *cobra.Command) bool {
	changed := false
	cmd.Flags().Visit(func(*pflag.Flag) { changed = true })
	return changed
}

// inventoryDraftRejectCmd 驳回一条草稿；驳回后草稿消失。
func inventoryDraftRejectCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "draft-reject ID",
		Args:  cobra.ExactArgs(1),
		Short: "Reject a declaration draft",
		Long: `驳回一条声明草稿。草稿会被删除，不会进入软件清单；agent 需要重新提
交修改后的草稿。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := requireInventoryVault()
			if err != nil {
				return err
			}
			defer s.Close()
			if err := softwareinventory.New(s).RejectDeclarationDraft(
				args[0], softwareinventory.CLISource(),
			); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "声明草稿 %s 已驳回。\n", args[0])
			return nil
		},
	}
	return c
}
