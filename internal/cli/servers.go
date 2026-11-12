package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/secrethint"
	"ssh-manager-mcp/internal/servermanage"
	"ssh-manager-mcp/internal/shellprobe"
	"ssh-manager-mcp/internal/store"
)

// changedFlagNames lists, in pflag's lexical order, the flags this invocation
// explicitly passed — FIELD NAMES only. This is what server.edit's "changed"
// summary entry records: names are safe to audit; values (credential ones
// included) never enter the summary.
func changedFlagNames(cmd *cobra.Command) []string {
	var out []string
	cmd.Flags().Visit(func(f *pflag.Flag) {
		out = append(out, f.Name)
	})
	return out
}

// writeOwnerAuditError writes the Status=error variant of an owner audit row
// (store.OwnerAuditRow builds the ok shape; a failed mutation rewrites the
// status before writing). Best-effort by design: on a failure path the
// command's own error is the outcome that matters and is returned unchanged,
// so a failed audit write never masks it.
func writeOwnerAuditError(s *store.Store, action string, summary map[string]any) {
	row := store.OwnerAuditRow(action, summary)
	row.Status = "error"
	_ = s.WriteAudit(row)
}

func newServersCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "servers", Short: "Manage SSH target servers"}
	cmd.AddCommand(serversAddCmd(), serversListCmd(), serversShowCmd(), serversRmCmd(), serversEditCmd(), serversImportCmd(), serversCompleteCmd(), serversPinHostkeyCmd(), serversProbeShellCmd())
	return cmd
}

func serversAddCmd() *cobra.Command {
	var (
		name, host, user, password, keyPath, keyPass, sudoPassword, description string
		location, hardware, services, role, caveats                             string
		port                                                                    int
		tags                                                                    []string
		exposeHost                                                              bool
		shell                                                                   string
		device                                                                  string
	)
	c := &cobra.Command{
		Use:   "add",
		Short: "Add a server (credential optional)",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			if host == "" && device != "" {
				memberships, err := s.ListNetworkMembershipsByRegistration(device)
				if err != nil {
					return err
				}
				if len(memberships) == 0 {
					return errors.New("指定已有设备但没有网络成员时，必须提供兜底主机")
				}
				host, _ = servermanage.FallbackHostFromMemberships(memberships)
			}
			if host == "" && device == "" {
				return errors.New("必须提供 --host，或提供 --device 并确保该设备已有网络成员")
			}
			// Audit summary (owner whitelist: name/host/port/user only —
			// credential values are excluded by construction). Built once from
			// the flags so the success and the failure rows share one shape.
			summary := map[string]any{"name": name, "host": host, "port": port, "user": user}
			// Credential is OPTIONAL (Plan 20 C0): --password/--key are mutually
			// exclusive, but a server may carry NEITHER (credential-less — e.g.
			// an ssh_config host without IdentityFile). exec on such a server
			// returns "no_credential" until one is attached via `servers edit`.
			if password != "" && keyPath != "" {
				writeOwnerAuditError(s, "server.add", summary)
				return fmt.Errorf("--password and --key are mutually exclusive; provide one or neither")
			}
			// 命令策略(ticket #14):shell 类型是三选一的连接信息字段,
			// 非法值在入口拒绝(取值表与策略判定内核一致)。
			if _, err := models.ParseShellType(shell); err != nil {
				writeOwnerAuditError(s, "server.add", summary)
				return err
			}
			srv := &models.Server{
				Name: name, Host: host, Port: port, User: user, Tags: tags,
				Description: strings.TrimSpace(description),
				Location:    strings.TrimSpace(location),
				Hardware:    strings.TrimSpace(hardware),
				Services:    strings.TrimSpace(services),
				Role:        strings.TrimSpace(role),
				Caveats:     strings.TrimSpace(caveats),
				ExposeHost:  exposeHost,
				ShellType:   models.ShellType(shell),
			}
			// Suspected-secret hint (Plan 28 T2): advisory, non-blocking scan
			// of the free-text metadata this add is about to persist, in its
			// final (trimmed / JSON-raw tags) form. A hit never fails the add
			// and the warning never echoes field content (no-echo by
			// construction). Credential fields are deliberately NOT scanned.
			printSecretHints(cmd, secrethint.ScanServer(
				tagsRawForScan(srv.Tags),
				srv.Description, srv.Location, srv.Hardware, srv.Services, srv.Role, srv.Caveats,
			))
			// Credentials are minted (or reused, when .ID is set) INSIDE the
			// same transaction as the server insert — a mid-way failure leaves
			// zero orphan credential rows (Plan 20 B1/G6).
			var cred, sudoCred *models.Credential
			switch {
			case password != "":
				cred = &models.Credential{Type: models.CredPassword, Secret: []byte(password)}
			case keyPath != "":
				keyBytes, err := readKeyFile(keyPath)
				if err != nil {
					writeOwnerAuditError(s, "server.add", summary)
					return err
				}
				cred = &models.Credential{Type: models.CredPrivateKey, Secret: keyBytes, Passphrase: []byte(keyPass)}
			}
			if sudoPassword != "" {
				sudoCred = &models.Credential{Type: models.CredPassword, Secret: []byte(sudoPassword)}
			}
			added, err := s.AddServerWithCredentialsToDevice(srv, cred, sudoCred, device)
			if err != nil {
				writeOwnerAuditError(s, "server.add", summary)
				return err
			}
			// Success row (projects.go precedent: a mutation's audit write
			// failure fails the command — the row is mandatory, not advisory).
			if err := s.WriteOwnerAudit("server.add", summary); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "added server %s id=%s device=%s\n", name, added.ServerID, added.Device.Name)
			printServerAnchorCopyResult(cmd.OutOrStdout(), added.AnchorCopy)
			return nil
		},
	}
	c.Flags().StringVar(&name, "name", "", "server name (unique)")
	c.Flags().StringVar(&host, "host", "", "hostname or IP; may be omitted when --device names a device with network memberships")
	c.Flags().IntVar(&port, "port", 22, "port")
	c.Flags().StringVar(&user, "user", "", "ssh user")
	c.Flags().StringVar(&password, "password", "", "password auth (mutually exclusive with --key; omit both for a credential-less server)")
	c.Flags().StringVar(&keyPath, "key", "", "path to private key file (mutually exclusive with --password; omit both for a credential-less server)")
	c.Flags().StringVar(&keyPass, "key-passphrase", "", "passphrase for encrypted private key")
	c.Flags().StringVar(&sudoPassword, "sudo-password", "", "sudo password (enables sudo -S)")
	c.Flags().StringSliceVar(&tags, "tags", nil, "tags")
	c.Flags().StringVar(&description, "description", "", "owner notes (shown to the agent); prefer structured fields below")
	c.Flags().StringVar(&location, "location", "", "where deployed (datacenter/region/rack/tenant) — shown to the agent")
	c.Flags().StringVar(&hardware, "hardware", "", "hardware config (CPU/RAM/disk/GPU) — shown to the agent")
	c.Flags().StringVar(&services, "services", "", "what is deployed/running here — shown to the agent")
	c.Flags().StringVar(&role, "role", "", "this server's purpose (e.g. 'prod pg primary') — shown to the agent")
	c.Flags().StringVar(&caveats, "special-handling", "", "operational gotchas / special handling rules — the agent reads this BEFORE acting")
	c.Flags().BoolVar(&exposeHost, "expose-host", false, "return the plaintext host in list_servers (default: masked as \"hidden\")")
	c.Flags().StringVar(&shell, "shell", "posix", "shell type for command-policy judging (posix, powershell, cmd)")
	c.Flags().StringVar(&device, "device", "", "关联的已有拓扑设备；不给时自动生成同名设备，兜底主机可从它的物理局域网地址生成")
	_ = c.MarkFlagRequired("name")
	_ = c.MarkFlagRequired("user")
	return c
}

// serversProbeShellCmd:`sshmgr servers probe-shell [<name>...]`——探测服
// 务器实际的 shell 类型并与标注比对(ticket #17,命令策略的诊断面;探
// 测只向服务器发两条固定的 echo 命令,无副作用)。与标注不一致时提示
// owner 用 servers edit 修正——探测从不改标注;无法判定时如实说明并给
// 出判定依据。不给名字则探测全部服务器。退出码:连接或执行失败为非
// 零;不一致与无法判定只是提示,不是失败。
func serversProbeShellCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "probe-shell [name...]",
		Short: "Probe each server's real shell type and compare with the annotation (read-only: a mismatch is hinted at, never fixed)",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer st.Close()
			// 先把全部目标解析出来再探测:名字拼错立刻报错,不在长网
			// 络操作之后才暴露(与 ssh 命令的查找语义一致)。
			var targets []*models.Server
			if len(args) == 0 {
				if targets, err = st.ListServers(); err != nil {
					return err
				}
			} else {
				for _, name := range args {
					srv, gerr := st.GetServerByName(name)
					if gerr != nil {
						return gerr
					}
					if srv == nil {
						return fmt.Errorf("server %q not found", name)
					}
					targets = append(targets, srv)
				}
			}
			out := cmd.OutOrStdout()
			stderr := cmd.ErrOrStderr()
			failed := false
			for _, srv := range targets {
				if err := probeAndReportShell(out, stderr, st, srv); err != nil {
					failed = true
					fmt.Fprintf(out, "%s: probe failed: %v\n", srv.Name, err)
				}
			}
			if failed {
				return errors.New("servers probe-shell: one or more servers could not be probed")
			}
			return nil
		},
	}
}

// probeAndReportShell 探测一台服务器并打一行结论,三种形态:与标注一
// 致 / 不一致(带修正指引)/ 无法判定(带判定依据)。探测本身出错时由
// 调用方打失败行。
func probeAndReportShell(out, stderr io.Writer, st *store.Store, srv *models.Server) error {
	detected, evidence, connectionNotes, err := shellprobe.ProbeServer(context.Background(), st, srv)
	if err != nil {
		return err
	}
	for _, note := range connectionNotes {
		fmt.Fprintf(stderr, "连接说明：%s\n", note)
	}
	annotated := models.NormalizeShellType(string(srv.ShellType))
	switch {
	case detected == "":
		fmt.Fprintf(out, "%s: could not determine the real shell type (evidence: %s); the annotation says %s — check this server by hand\n", srv.Name, evidence, annotated)
	case detected != annotated:
		fmt.Fprintf(out, "%s: real shell=%s, annotation=%s — MISMATCH: fix the annotation with `sshmgr servers edit %s --shell %s` (this probe never changes the annotation; evidence: %s)\n",
			srv.Name, detected, annotated, srv.Name, detected, evidence)
	default:
		fmt.Fprintf(out, "%s: real shell=%s — matches the annotation (evidence: %s)\n", srv.Name, detected, evidence)
	}
	return nil
}

func serversListCmd() *cobra.Command {
	var attentionOnly bool
	c := &cobra.Command{
		Use:   "ls",
		Short: "List servers",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			items, err := servermanage.New(s).List(attentionOnly)
			if err != nil {
				return err
			}
			for _, item := range servermanage.SortNeedsAttentionFirst(items) {
				srv := item.Server
				sudo := "-"
				if srv.SudoCredentialID != "" {
					sudo = "sudo"
				}
				role := srv.Role
				if role == "" {
					role = "-"
				}
				attention := "-"
				if labels := servermanage.ReasonLabels(item.AttentionReasons); len(labels) > 0 {
					attention = strings.Join(labels, "、")
				}
				// 命令策略(ticket #14):shell 类型是连接信息的一员,owner 在
				// ls 里能看到标注(存储层已归一,空值不会到这里)。
				fmt.Fprintf(cmd.OutOrStdout(), "%-16s %-20s %s@%s:%d [shell=%s] [%s] (%s) [%s] · %s\n",
					srv.Name, srv.ID, srv.User, srv.Host, srv.Port,
					string(models.NormalizeShellType(string(srv.ShellType))), sudo, role, attention, truncate(srv.Caveats))
			}
			return nil
		},
	}
	c.Flags().BoolVar(&attentionOnly, "attention", false, "only list servers that need attention")
	return c
}

// truncate clips a free-text field for the ls line (rune-safe; "" → "-").
func truncate(s string) string {
	if s == "" {
		return "-"
	}
	if r := []rune(s); len(r) > 40 {
		return string(r[:37]) + "..."
	}
	return s
}

func serversRmCmd() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:     "rm [name-or-id]",
		Short:   "Remove a server after reviewing the impact preview",
		Example: "  sshmgr servers rm web-01\n  sshmgr servers rm --yes web-01   # non-interactive/script use\n  sshmgr servers rm -- -AbC123xyz   # an older ID that starts with \"-\": put -- before it",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			manager := servermanage.New(s)
			preview, err := manager.DeletePreview(args[0])
			if err != nil {
				return err
			}
			if preview == nil {
				// 与旧语义一致：目标不存在是安静的幂等空操作。
				return nil
			}
			writeServerDeletePreview(cmd.OutOrStdout(), preview)
			if !yes {
				if !serversStdinIsTTY() {
					return errors.New("servers rm 需要交互确认；非交互终端请加 --yes")
				}
				fmt.Fprintf(cmd.OutOrStdout(), "确认删除 %s？输入 y 删除，其他输入取消：", preview.Server.Name)
				line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				if err != nil && line == "" {
					return errors.New("未读取到确认输入，已取消删除")
				}
				if strings.TrimSpace(line) != "y" {
					fmt.Fprintln(cmd.OutOrStdout(), "已取消删除")
					return nil
				}
			}
			fresh, err := manager.Delete(args[0], preview.Digest, servermanage.CLISource())
			if err != nil {
				if errors.Is(err, servermanage.ErrDeletePreviewChanged) {
					fmt.Fprintln(cmd.OutOrStdout(), "删除预览已变化，未删除任何数据。新的预览：")
					writeServerDeletePreview(cmd.OutOrStdout(), fresh)
				}
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted server %s\n", preview.Server.Name)
			return nil
		},
	}
	c.Flags().BoolVar(&yes, "yes", false, "skip the interactive confirmation (required on non-interactive stdin)")
	return c
}

// rmSummary is server.rm's whitelist summary: exactly name/host/port (the
// whitelist for rm has no user field, unlike server.add).
func rmSummary(srv *models.Server) map[string]any {
	return map[string]any{"name": srv.Name, "host": srv.Host, "port": srv.Port}
}

// serversEditCmd edits a server in place: only flags the operator passed are
// applied (cobra Changed()), so the server id + profile bindings are preserved.
// The write path lives in the server management module, shared with the Web UI;
// audit rows carry the flag names this invocation passed (never values).
func serversEditCmd() *cobra.Command {
	var (
		newName, host, user, password, keyPath, keyPass, sudoPassword, description string
		location, hardware, services, role, caveats                                string
		port                                                                       int
		tags                                                                       []string
		clearCred                                                                  bool
		exposeHost                                                                 bool
		shell                                                                      string
	)
	c := &cobra.Command{
		Use:   "edit [name]",
		Args:  cobra.ExactArgs(1),
		Short: "Edit a server's fields and/or re-credential (id + profile bindings preserved)",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			fl := cmd.Flags()
			req := servermanage.EditRequest{Identifier: args[0]}
			if fl.Changed("name") {
				req.Name = &newName
			}
			if fl.Changed("host") {
				req.Host = &host
			}
			if fl.Changed("port") {
				req.Port = &port
			}
			if fl.Changed("user") {
				req.User = &user
			}
			if fl.Changed("description") {
				req.Description = &description
			}
			if fl.Changed("tags") {
				req.Tags = &tags
			}
			if fl.Changed("location") {
				req.Location = &location
			}
			if fl.Changed("hardware") {
				req.Hardware = &hardware
			}
			if fl.Changed("services") {
				req.Services = &services
			}
			if fl.Changed("role") {
				req.Role = &role
			}
			if fl.Changed("special-handling") {
				req.Caveats = &caveats
			}
			if fl.Changed("expose-host") {
				req.ExposeHost = &exposeHost
			}
			if fl.Changed("shell") {
				req.Shell = &shell
			}
			if fl.Changed("password") {
				req.Password = &password
			}
			if fl.Changed("key") {
				req.KeyPath = &keyPath
			}
			if fl.Changed("key-passphrase") {
				req.KeyPassphrase = &keyPass
			}
			if fl.Changed("sudo-password") {
				req.SudoPassword = &sudoPassword
			}
			req.ClearCredential = clearCred
			manager := servermanage.New(s)
			// 只补私钥口令会同时影响共用这把私钥的其他服务器：写入前列出名
			// 单，让 owner 在提交前看到影响范围。
			if fl.Changed("key-passphrase") && !fl.Changed("key") && !fl.Changed("password") && !clearCred {
				if peers, err := manager.SharedCredentialServers(args[0]); err == nil && len(peers) > 1 {
					fmt.Fprintf(cmd.OutOrStdout(), "补私钥口令会影响共用这把私钥的全部服务器：%s\n", strings.Join(peers, ","))
				}
			}
			result, err := manager.Edit(req, servermanage.CLISource())
			if err != nil {
				var conflict *servermanage.RevisionConflict
				if errors.As(err, &conflict) {
					return fmt.Errorf("%w", err)
				}
				return err
			}
			if len(result.ChangedFields) == 1 && result.ChangedFields[0] == "clear-credential" {
				origName := args[0]
				if before, _ := s.GetServer(result.Server.ID); before != nil {
					// clear 路径不应用其他字段；提示用条目当前名字。
					origName = before.Name
				}
				fmt.Fprintf(cmd.OutOrStdout(), "cleared credentials for %s\n", origName)
				return nil
			}
			// 成功保存后的疑似机密提示：只扫描本次显式提交的元数据字段，
			// 值来自保存后的服务器条目。
			printSecretHints(cmd, scanEditMetadata(cmd, result.Server))
			fmt.Fprintf(cmd.OutOrStdout(), "updated server %s\n", result.Server.Name)
			if len(result.AffectedServers) > 1 {
				fmt.Fprintf(cmd.OutOrStdout(), "受影响服务器：%s\n", strings.Join(result.AffectedServers, ","))
			}
			return nil
		},
	}
	c.Flags().StringVar(&newName, "name", "", "rename the server")
	c.Flags().StringVar(&host, "host", "", "hostname or IP")
	c.Flags().IntVar(&port, "port", 22, "port")
	c.Flags().StringVar(&user, "user", "", "ssh user")
	c.Flags().StringVar(&description, "description", "", "owner notes (shown to the agent); prefer structured fields below")
	c.Flags().StringVar(&location, "location", "", "where deployed (datacenter/region/rack/tenant) — shown to the agent")
	c.Flags().StringVar(&hardware, "hardware", "", "hardware config (CPU/RAM/disk/GPU) — shown to the agent")
	c.Flags().StringVar(&services, "services", "", "what is deployed/running here — shown to the agent")
	c.Flags().StringVar(&role, "role", "", "this server's purpose — shown to the agent")
	c.Flags().StringVar(&caveats, "special-handling", "", "operational gotchas / special handling rules — pass \"\" to clear")
	c.Flags().StringSliceVar(&tags, "tags", nil, "tags (replaces existing)")
	c.Flags().StringVar(&password, "password", "", "switch to / replace password auth")
	c.Flags().StringVar(&keyPath, "key", "", "switch to / replace key auth (path to private key)")
	c.Flags().StringVar(&keyPass, "key-passphrase", "", "passphrase for the private key (with --key, or alone to supplement the stored key)")
	c.Flags().StringVar(&sudoPassword, "sudo-password", "", "set / replace sudo password")
	c.Flags().BoolVar(&clearCred, "clear-credential", false, "remove the server's credentials (back to credential-less)")
	c.Flags().BoolVar(&exposeHost, "expose-host", false, "return the plaintext host in list_servers (default: masked as \"hidden\")")
	c.Flags().StringVar(&shell, "shell", "", "shell type for command-policy judging (posix, powershell, cmd); omit to keep current")
	return c
}

// readKeyFile reads a private key from disk.
func readKeyFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// stripTag returns tags minus every occurrence of tag; an all-matches list
// yields a valid empty slice.
func stripTag(tags []string, tag string) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if t != tag {
			out = append(out, t)
		}
	}
	return out
}

// printSecretHints writes one advisory warning line per finding to the
// command's stderr. Non-blocking by contract: callers' success path, return
// values, and exit codes are untouched.
func printSecretHints(cmd *cobra.Command, findings []secrethint.Finding) {
	for _, f := range findings {
		fmt.Fprintln(cmd.ErrOrStderr(), secrethint.FormatWarning(f))
	}
}

// scanEditMetadata returns suspected-secret findings for the metadata flags
// this edit invocation actually passed (cobra Changed()), each scanned in the
// final persisted form held by srv at call time. Flag names here are the CLI
// spellings; Finding field names are the persisted/model names.
func scanEditMetadata(cmd *cobra.Command, srv *models.Server) []secrethint.Finding {
	fl := cmd.Flags()
	var findings []secrethint.Finding
	if fl.Changed("tags") {
		findings = append(findings, secrethint.ScanValue("tags", tagsRawForScan(srv.Tags))...)
	}
	if fl.Changed("description") {
		findings = append(findings, secrethint.ScanValue("description", srv.Description)...)
	}
	if fl.Changed("location") {
		findings = append(findings, secrethint.ScanValue("location", srv.Location)...)
	}
	if fl.Changed("hardware") {
		findings = append(findings, secrethint.ScanValue("hardware", srv.Hardware)...)
	}
	if fl.Changed("services") {
		findings = append(findings, secrethint.ScanValue("services", srv.Services)...)
	}
	if fl.Changed("role") {
		findings = append(findings, secrethint.ScanValue("role", srv.Role)...)
	}
	if fl.Changed("special-handling") {
		findings = append(findings, secrethint.ScanValue("caveats", srv.Caveats)...)
	}
	return findings
}
