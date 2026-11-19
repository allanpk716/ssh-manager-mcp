package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/access"
	"ssh-manager-mcp/internal/clientfeatures"
	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/termdetect"
)

// cacheTokensStdinIsInteractive 判断标准输入是不是交互终端。移除客户端
// 是不可逆操作；脚本场景必须显式给 --yes。
var cacheTokensStdinIsInteractive = func() bool {
	return termdetect.IsTerminal(os.Stdin.Fd())
}

// loadServeCertFingerprint 适配访问管理模块的证书指纹注入协议：模块在产
// 生设备码之前先调用它读取权威端证书公钥指纹，失败时不会留下设备码。
func loadServeCertFingerprint() (string, error) {
	_, _, fingerprint, err := mcpserver.LoadOrCreateServeCert()
	return fingerprint, err
}

// writeCacheTokenAudit records the owner audit row for a cache-tokens
// mutation: Status=ok, or Status=error when opErr is non-nil. The summary
// carries only the command's whitelisted fields (device name, profile name)
// — the one-time code printed to stdout never enters it. ExitCode/DurationMS
// stay at the zero values every owner audit row uses.
func writeCacheTokenAudit(s *store.Store, action string, summary map[string]any, opErr error) error {
	if opErr == nil {
		return s.WriteOwnerAudit(action, summary)
	}
	row := store.OwnerAuditRow(action, summary)
	row.Status = "error"
	return s.WriteAudit(row)
}

func newCacheTokensCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cache-tokens",
		Short: "Manage device authorization codes for offline-cache pulls (owner)",
	}
	cmd.AddCommand(
		cacheTokensAddCmd(), cacheTokensLsCmd(), cacheTokensRevokeCmd(), cacheTokensBindCmd(),
		cacheTokensRemoveClientCmd(),
	)
	return cmd
}

func cacheTokensAddCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "add --name <device> --profile <profile>",
		Short: "Issue a one-time device authorization code (printed once), bound to a profile",
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			name, _ := cmd.Flags().GetString("name")
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			profileName, _ := cmd.Flags().GetString("profile")
			if profileName == "" {
				return fmt.Errorf("--profile is required (the device pulls only that profile's authorized servers — Plan 39)")
			}
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			result, err := access.New(s, access.WithServeCertFingerprint(loadServeCertFingerprint)).
				AddDeviceCode(name, profileName, "https://<serve-host>:7878", access.CLISource())
			if err != nil {
				return err
			}
			printCacheToken(cmd.OutOrStdout(), name, result.Code, result.Fingerprint)
			return nil
		},
	}
	c.Flags().String("name", "", "device name (e.g. laptop, desktop-2); reusable after revoke")
	_ = c.MarkFlagRequired("name")
	c.Flags().String("profile", "", "profile NAME the device is bound to (its /snapshot scope)")
	_ = c.MarkFlagRequired("profile")
	return c
}

func cacheTokensLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List device authorization codes (prefix + status + bound profile + last pull + client version; never the code)",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			tokens, err := access.New(s).ListDeviceCodes()
			if err != nil {
				return err
			}
			for _, ct := range tokens {
				last := "never"
				if !ct.LastPullAt.IsZero() {
					last = ct.LastPullAt.Format("2006-01-02 15:04:05")
				}
				clientVersion := clientfeatures.ClientVersionLabel(ct.ClientVersion)
				profile := ct.ProfileName
				if ct.ProfileID == "" {
					profile = "-"
				} else if profile == "" {
					profile = "?"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%-16s %s prefix=%s… status=%s profile=%s last_pull=%s client_version=%s\n",
					ct.Name, ct.ID, ct.TokenPrefix, ct.Status, profile, last, clientVersion)
			}
			return nil
		},
	}
}

// cacheTokensBindCmd is Plan 39's legacy repair path: an unbound device code
// (pre-Plan-39 migration state — its pulls are refused with 403) is bound to a
// profile in place, keeping name/status/last-pull history. No re-issue, no
// client-side re-enroll needed.
func cacheTokensBindCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "bind [name] [profile]",
		Args:  cobra.ExactArgs(2),
		Short: "Bind an unbound device code to a profile (repairs pre-Plan-39 codes; its pulls 403 until bound)",
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			profileID, err := profileIDByName(s, args[1])
			if err != nil {
				if aerr := writeCacheTokenAudit(s, "cache-token.bind", map[string]any{"name": args[0], "profile": args[1]}, err); aerr != nil {
					return aerr
				}
				return err
			}
			tokens, err := s.ListCacheTokens()
			if err != nil {
				return err
			}
			var expectedProfileID string
			found := false
			for _, token := range tokens {
				if token.Name == args[0] {
					expectedProfileID, found = token.ProfileID, true
					break
				}
			}
			if !found {
				notFound := fmt.Errorf("device code %q not found", args[0])
				if aerr := writeCacheTokenAudit(s, "cache-token.bind", map[string]any{"name": args[0], "profile": args[1]}, notFound); aerr != nil {
					return aerr
				}
				return notFound
			}
			if err := access.New(s).ChangeClientProfile(args[0], expectedProfileID, profileID, access.CLISource()); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "bound cache token %s to profile %s — its next pull serves that profile's authorized servers\n", args[0], args[1])
			return nil
		},
	}
}

func cacheTokensRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke [name]",
		Args:  cobra.ExactArgs(1),
		Short: "Revoke a device authorization code (Lazy — its next pull is rejected)",
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			if err := access.New(s).RevokeDeviceCode(args[0], access.CLISource()); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "revoked cache token %s (status=%s)\n", args[0], models.CacheTokenRevoked)
			// Plan 34 §3: revoking the device code does NOT touch the project
			// token this device also holds (.claude.json is the user's config —
			// the client never rewrites it). A compromised device needs both revoked.
			fmt.Fprintln(cmd.OutOrStdout(), "reminder: also revoke project tokens issued to that device if it may be compromised")
			return nil
		},
	}
}

func cacheTokensRemoveClientCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "remove-client [name]",
		Args:  cobra.ExactArgs(1),
		Short: "移除客户端：解除机器关联、吊销它的设备码以及配对时生成的 MCP 访问令牌",
		RunE: func(cmd *cobra.Command, args []string) error {
			yes, _ := cmd.Flags().GetBool("yes")
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, "移除客户端：解除机器关联、吊销它的设备码以及配对时生成的 MCP 访问令牌")
			if !yes {
				if !cacheTokensStdinIsInteractive() {
					return fmt.Errorf("非交互终端不能确认移除客户端；请加 --yes 后重跑")
				}
				fmt.Fprint(out, "输入 y 确认移除：")
				line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				if err != nil && line == "" {
					return fmt.Errorf("读取移除确认失败: %w", err)
				}
				if strings.ToLower(strings.TrimSpace(line)) != "y" {
					fmt.Fprintln(out, "\n已取消，未移除客户端。")
					return nil
				}
			}
			detail, err := access.New(s).ClientDetail(args[0])
			if err != nil {
				return err
			}
			if detail == nil {
				return fmt.Errorf("client %q not found", args[0])
			}
			machineName := ""
			if detail.Machine != nil {
				machineName = detail.Machine.Name
			}
			if err := access.New(s).RemoveClient(args[0], access.CLISource()); err != nil {
				return err
			}
			if machineName == "" {
				fmt.Fprintf(out, "已移除客户端 %s\n", detail.Name)
				return nil
			}
			fmt.Fprintf(out, "已移除客户端 %s，并解除机器 %s 的关联\n", detail.Name, machineName)
			return nil
		},
	}
	c.Flags().Bool("yes", false, "非交互终端确认移除时必须提供")
	return c
}

// printCacheToken emits the one-time device code + the server's SPKI fingerprint
// + the cache-pull invocation. Shown once. The PRIMARY recommended invocation
// embeds the pin inside the token as "<code>:<pin>" (spec §3.3 形态 A) — that is
// the form cachePullCmd's SplitTokenPin consumes, so producing it here gives
// the embedded-pin path a real producer and keeps the enrollment story
// single-string. A blank fingerprint (only possible if LoadOrCreateServeCert
// failed and the caller chose to print anyway) degrades to the token-only form.
func printCacheToken(out io.Writer, name, code, fingerprint string) {
	fmt.Fprintf(out, "Authorization code for %q (shown once): %s\n", name, code)
	if fingerprint != "" {
		fmt.Fprintf(out, "Server fingerprint (serve cert SPKI): %s\n", fingerprint)
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "On the work machine:")
	if fingerprint != "" {
		fmt.Fprintf(out, "  %s\n", access.DeviceCodePullCommand("https://<serve-host>:7878", code, fingerprint))
		fmt.Fprintf(out, "  # (or) set SSHMGR_SERVE_PIN=%s and pass --token %s\n", fingerprint, code)
	} else {
		fmt.Fprintf(out, "  sshmgr cache pull --url https://<serve-host>:7878 --token %s\n", code)
	}
}
