package cli

// serve_pair.go — `ssh-manager serve pair ls|approve|reject`(Plan 42 批1 T8,
// spec §3.3-3):store 直连的配对裁决面,与 broker TUI 的 Pairing 页共享同一张
// pairing_pending 表(跨进程;CAS 仲裁并发)。
//
// SAS 裁决(控制器,覆盖 spec「批准面同屏三件套」原文):SAS 推导需要 serve 进程
// 内存里的 X25519 私钥,本进程只有 store 直连——因此输出行 = 两件套
// `<name> @ <target_url>` + 「对照 client 屏 SAS 后批准」的措辞,绝不伪造
// 第三件。机械地址校验(ForeignTarget)在此复算:目标 ≠ 本机地址 → 无
// --allow-foreign-url 拒绝并打 ⚠ 文案。

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/store"
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
func servePairResolve(rows []store.PendingPairing, arg string) (*store.PendingPairing, error) {
	var byName []*store.PendingPairing
	var byID *store.PendingPairing
	if id, err := hex.DecodeString(strings.TrimSpace(arg)); err == nil && len(id) == 32 {
		for i := range rows {
			if string(rows[i].ID) == string(id) {
				byID = &rows[i]
				break
			}
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
func servePairFlags(p store.PendingPairing, now int64) string {
	parts := []string{p.State}
	deadline := p.EnrollDeadline
	if p.State == "approved" {
		deadline = p.ApprovedDeadline
	}
	rem := deadline - now
	if rem < 0 {
		rem = 0
	}
	parts[0] = fmt.Sprintf("%s剩余%ds", parts[0], rem)
	if p.ReplaceInactive {
		parts = append(parts, "⚠替换未激活码")
	}
	if mcpserver.ForeignTarget(p.TargetURL) {
		parts = append(parts, "⚠目标≠本机")
	}
	return strings.Join(parts, "|")
}

func servePairLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List the pending pairing queue (name/target/source-IP/hint/flags; the SAS shows on the client screen)",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			rows, err := s.ListPendingPairing()
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(no pending pairing requests)")
				return nil
			}
			now := time.Now().Unix()
			for _, p := range rows {
				fmt.Fprintf(cmd.OutOrStdout(),
					"%-16s %s\n  source=%s hint=%s flags=%s profile=%s id=%s\n  SAS 码见 client 屏幕——对照本行名称/地址一致后再批准\n",
					clientops.StripC0C1(p.Name), clientops.StripC0C1(p.TargetURL), orDashStr(p.SourceIP), orDashStr(clientops.StripC0C1(strings.TrimSpace(p.ProfileHint))),
					servePairFlags(p, now), profileNameByID(s, p.Profile), hex.EncodeToString(p.ID))
			}
			return nil
		},
	}
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
			rows, err := s.ListPendingPairing()
			if err != nil {
				return err
			}
			row, err := servePairResolve(rows, args[0])
			if err != nil {
				return err
			}
			// 机械地址校验(spec §3.3-3):foreign 行必须显式 --allow-foreign-url。
			if mcpserver.ForeignTarget(row.TargetURL) && !allowForeign {
				return fmt.Errorf("⚠ 配对声明目标 ≠ 本机地址（%s）——疑似中继/假 discovery/错误网络；确属本机地址请加 --allow-foreign-url 重新执行", clientops.StripC0C1(row.TargetURL))
			}
			profileID, err := resolveProfileID(s, profile)
			if err != nil {
				return err
			}
			ok, err := s.ApprovePairing(row.ID, profileID)
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("approve had no effect — the row expired or was already adjudicated (CAS miss); re-run `serve pair ls`")
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s @ %s (对照 client 屏 SAS 后批准)\n", clientops.StripC0C1(row.Name), clientops.StripC0C1(row.TargetURL))
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
			rows, err := s.ListPendingPairing()
			if err != nil {
				return err
			}
			row, err := servePairResolve(rows, args[0])
			if err != nil {
				return err
			}
			ok, err := s.RejectPairing(row.ID)
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("reject had no effect — the row expired or was already adjudicated (CAS miss); re-run `serve pair ls`")
			}
			fmt.Fprintf(cmd.OutOrStdout(), "rejected %s @ %s (that request can no longer enroll this device)\n", clientops.StripC0C1(row.Name), clientops.StripC0C1(row.TargetURL))
			return nil
		},
	}
}

// orDashStr renders an empty display string as "-" (tui's orDash parity).
func orDashStr(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
