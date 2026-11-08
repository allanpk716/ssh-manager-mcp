package cli

// serve_pair.go — `sshmgr serve pair ls|approve|reject`(Plan 42 批1 T8,
// spec §3.3-3):访问管理模块配对部分的命令行薄面。
//
// 短认证码双屏比对(2026-09-01 裁决:恢复 spec rev4:68 冻结原文,撤销 rev4:69
// 的降级勘误):serve 在 enroll 时即持有 X25519 私钥与请求里的 client_pub,
// 当场派生 6 位短认证码落行;本面按历史输出形状显示设备名、目标地址与短认证码,
// owner 与 client 屏逐位比对一致后才批准。行缺短认证码(版本错配/旧行)→ ⚠
// 警示并建议拒绝——绝不静默回退到抓不住 MITM 的 name/url 两件套对照。
// 机械地址校验(ForeignTarget)在此复算:目标 ≠ 本机地址 → 无
// --allow-foreign-url 拒绝并打 ⚠ 文案。

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/access"
	"ssh-manager-mcp/internal/clientops"
)

func newServePairCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "pair",
		Short: "Adjudicate pending SAS pairing requests (approve/reject; same queue as the broker TUI's Pairing page)",
	}
	c.AddCommand(servePairLsCmd(), servePairApproveCmd(), servePairRejectCmd())
	return c
}

// servePairResolve maps an owner-facing arg (device NAME, or the row's 32B id
// as 64-char hex — both shown by `serve pair ls`) to one actionable row. Name
// matches must be unique — an ambiguity names both ids rather than guessing.
func servePairResolve(rows []access.PairingRequest, arg string) (*access.PairingRequest, error) {
	var byName []*access.PairingRequest
	var byID *access.PairingRequest
	for i := range rows {
		if rows[i].ID == strings.TrimSpace(arg) {
			byID = &rows[i]
			break
		}
	}
	for i := range rows {
		if rows[i].Name == arg {
			byName = append(byName, &rows[i])
		}
	}
	switch {
	case len(byName) == 1:
		return byName[0], nil
	case len(byName) > 1:
		return nil, fmt.Errorf("device name %q is ambiguous (%d pending rows) — approve/reject by row id instead", arg, len(byName))
	case byID != nil:
		return byID, nil
	}
	return nil, fmt.Errorf("no pending pairing row matches %q (see `serve pair ls`; ids are shown in the detail/flags column)", arg)
}

// servePairFlags renders the row's marker column: state+remaining window and
// the two ⚠ marks (未激活码替换 / 目标≠本机).
func servePairFlags(p access.PairingRequest) string {
	parts := []string{p.State}
	parts[0] = fmt.Sprintf("%s剩余%ds", p.State, p.RemainingSeconds)
	if p.ReplaceInactive {
		parts = append(parts, "⚠替换未激活码")
	}
	if p.ForeignTarget {
		parts = append(parts, "⚠目标≠本机")
	}
	return strings.Join(parts, "|")
}

func servePairLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List the pending pairing queue (rows show the three-piece line incl. SAS — compare digit-by-digit with the client screen)",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			rows, err := access.New(s).ListPairingRequests()
			if err != nil {
				return err
			}
			rows = actionablePairingRows(rows)
			if len(rows) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(no pending pairing requests)")
				return nil
			}
			for _, p := range rows {
				fmt.Fprintf(cmd.OutOrStdout(),
					"%-16s %s\n  SAS %s\n  source=%s hint=%s flags=%s profile=%s id=%s\n",
					clientops.StripC0C1(p.Name), clientops.StripC0C1(p.TargetURL), servePairSAS(p),
					orDashStr(p.SourceAddress), orDashStr(clientops.StripC0C1(strings.TrimSpace(p.ProfileHint))),
					servePairFlags(p), orDashStr(p.ProfileName), p.ID)
			}
			return nil
		},
	}
}

// servePairSAS renders the row's short authentication code: the real 6-digit
// code (derived by serve at enroll, landed in the row) or — for a row written
// by a pre-2026-09-01 serve — the missing-code warning; a two-piece
// comparison cannot catch
// a MITM, so the row is flagged as un-comparable rather than silently shown.
func servePairSAS(p access.PairingRequest) string {
	if s := strings.TrimSpace(p.ShortCode); s != "" {
		return s
	}
	return "⚠ 行缺 SAS——serve 版本过旧或行损坏,无法比对,建议 reject 后重新配对"
}

func servePairApproveCmd() *cobra.Command {
	var profile string
	var allowForeign bool
	c := &cobra.Command{
		Use:   "approve <name|idHex> --profile <profile>",
		Args:  cobra.ExactArgs(1),
		Short: "Approve a pending pairing (CAS; flips it to approved for the client's 120s finish window)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(profile) == "" {
				return fmt.Errorf("--profile is required (the paired device pulls only that profile's authorized servers)")
			}
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			manager := access.New(s)
			rows, err := manager.ListPairingRequests()
			if err != nil {
				return err
			}
			rows = actionablePairingRows(rows)
			row, err := servePairResolve(rows, args[0])
			if err != nil {
				return err
			}
			override := ""
			if allowForeign {
				override = access.PairingOverrideText
			}
			// 命令行 owner 先用 `serve pair ls` 查看短认证码并逐位比对，再
			// 显式执行 approve；这次显式执行就是比对确认，与 Web UI 的勾选
			// 框等价，因此这里传入「已比对」。
			// 目标地址异常的错误在这里翻回命令行原有提示；目标地址来自客
			// 户端请求，输出前必须剥掉 C0/C1 控制字符。
			if err := manager.ApprovePairing(row.ID, profile, true, override, access.CLISource()); err != nil {
				var foreignTarget *access.PairingForeignTargetError
				if errors.As(err, &foreignTarget) {
					return fmt.Errorf(
						"⚠ 配对声明目标 ≠ 本机地址（%s）——疑似中继/假 discovery/错误网络；确属本机地址请加 --allow-foreign-url 重新执行",
						clientops.StripC0C1(foreignTarget.TargetURL),
					)
				}
				return err
			}
			// 三件套行(spec rev4:83):approve 输出与 client 屏同一行,便于留痕比对。
			fmt.Fprintf(cmd.OutOrStdout(), "%s @ %s SAS %s\n", clientops.StripC0C1(row.Name), clientops.StripC0C1(row.TargetURL), servePairSAS(*row))
			fmt.Fprintf(cmd.OutOrStdout(), "profile=%s — the client has 120s to finish (it polls; the credentials land on its disk automatically)\n", profile)
			return nil
		},
	}
	c.Flags().StringVar(&profile, "profile", "", "profile NAME the paired device is bound to (its /snapshot scope)")
	_ = c.MarkFlagRequired("profile")
	c.Flags().BoolVar(&allowForeign, "allow-foreign-url", false, "explicitly approve a target whose host is NOT this machine (overrides the mechanical address check)")
	return c
}

func servePairRejectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reject <name|idHex>",
		Args:  cobra.ExactArgs(1),
		Short: "Reject a pending pairing (terminal — that request can never enroll the device)",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			manager := access.New(s)
			rows, err := manager.ListPairingRequests()
			if err != nil {
				return err
			}
			rows = actionablePairingRows(rows)
			row, err := servePairResolve(rows, args[0])
			if err != nil {
				return err
			}
			if err := manager.RejectPairing(row.ID, access.CLISource()); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "rejected %s @ %s (that request can no longer enroll this device)\n", clientops.StripC0C1(row.Name), clientops.StripC0C1(row.TargetURL))
			return nil
		},
	}
}

// actionablePairingRows 让命令行继续只显示待批准与已批准请求；已完成请求
// 只由 Web UI 在现有保留窗口内作为完成反馈显示。
func actionablePairingRows(rows []access.PairingRequest) []access.PairingRequest {
	out := make([]access.PairingRequest, 0, len(rows))
	for _, row := range rows {
		if row.State == "pending" || row.State == "approved" {
			out = append(out, row)
		}
	}
	return out
}

// orDashStr renders an empty display string as "-" (tui's orDash parity).
func orDashStr(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
