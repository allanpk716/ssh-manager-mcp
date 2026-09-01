package cli

// serve_pair.go — `sshmgr serve pair ls|approve|reject`(Plan 42 批1 T8,
// spec §3.3-3):store 直连的配对裁决面,与 broker TUI 的 Pairing 页共享同一张
// pairing_pending 表(跨进程;CAS 仲裁并发)。
//
// SAS 双屏比对(2026-09-01 裁决:恢复 spec rev4:68 冻结原文,撤销 rev4:69
// 的降级勘误):serve 在 enroll 时即持有 X25519 私钥与请求里的 client_pub,
// 当场派生 6 位 SAS 落行;本面输出三件套 `<name> @ <target_url> SAS <6位>`,
// owner 与 client 屏逐位比对一致后才批准。行缺 SAS(版本错配/旧行)→ ⚠
// 警示并建议拒绝——绝不静默回退到抓不住 MITM 的 name/url 两件套对照。
// 机械地址校验(ForeignTarget)在此复算:目标 ≠ 本机地址 → 无
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
		Short: "List the pending pairing queue (rows show the three-piece line incl. SAS — compare digit-by-digit with the client screen)",
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
					"%-16s %s\n  SAS %s\n  source=%s hint=%s flags=%s profile=%s id=%s\n",
					clientops.StripC0C1(p.Name), clientops.StripC0C1(p.TargetURL), servePairSAS(p),
					orDashStr(p.SourceIP), orDashStr(clientops.StripC0C1(strings.TrimSpace(p.ProfileHint))),
					servePairFlags(p, now), profileNameByID(s, p.Profile), hex.EncodeToString(p.ID))
			}
			return nil
		},
	}
}

// servePairSAS renders the row's SAS piece: the real 6-digit code (derived by
// serve at enroll, landed in the row) or — for a row written by a pre-
// 2026-09-01 serve — the no-SAS warning; a two-piece comparison cannot catch
// a MITM, so the row is flagged as un-comparable rather than silently shown.
func servePairSAS(p store.PendingPairing) string {
	if s := strings.TrimSpace(p.SAS); s != "" {
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
