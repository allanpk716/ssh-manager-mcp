package cli

import (
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/servermanage"
	"ssh-manager-mcp/internal/store"
)

// serversImportCmd: `sshmgr servers import [--file] [--dry-run] [--profile]`.
// 解析、冲突规划与执行都在服务器管理模块里，命令行只负责排版输出与结尾的
// 逐台补全衔接提示。
func serversImportCmd() *cobra.Command {
	var file, profile string
	var dryRun bool
	c := &cobra.Command{
		Use:   "import",
		Short: "Batch-import servers from an OpenSSH client config (~/.ssh/config)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if file == "" {
				home, _ := os.UserHomeDir()
				file = filepath.Join(home, ".ssh", "config")
			}
			file = expandTilde(file)
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			manager := servermanage.New(s)
			plan, err := manager.PlanImport(file, profile)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if plan.MatchWarning {
				fmt.Fprintln(out, "⚠ config 含 Match 块：相关继承值由库按 Host 模式近似求值，可能与真 ssh 不一致")
			}
			selected := make([]string, 0, len(plan.Candidates))
			for _, cand := range plan.Candidates {
				selected = append(selected, cand.Name)
			}
			if dryRun {
				for _, skipped := range plan.VaultSkipped {
					fmt.Fprintf(out, "%-20s %s\n", skipped.Name, renderVaultSkip(skipped.Reason))
				}
				for _, cand := range plan.Candidates {
					fmt.Fprintf(out, "%-20s will-import (%s)\n", cand.Name, candidateNote(cand.Note, false))
				}
				for _, skipped := range plan.ParseSkipped {
					fmt.Fprintf(out, "%-20s skip: %s\n", skipped.Name, skipped.Reason)
				}
				if plan.ProfileID != "" {
					fmt.Fprintf(out, "grant: %d server(s) -> %s (dry-run, not granted)\n", len(plan.Candidates), plan.ProfileName)
				}
				return nil
			}
			result, err := manager.RunImport(file, selected, profile, servermanage.CLISource())
			if err != nil {
				printImportEntries(out, result)
				return err
			}
			printImportEntries(out, result)
			var hints []importHint
			for _, srv := range result.ImportedServers {
				if findings := scanImportServer(srv); len(findings) > 0 {
					hints = append(hints, importHint{name: srv.Name, findings: findings})
				}
			}
			printImportHints(cmd.ErrOrStderr(), hints)
			if plan.ProfileID != "" {
				fmt.Fprintf(out, "granted %d server(s) -> %s\n", result.Imported, plan.ProfileName)
			}
			fmt.Fprintf(out, "统计：导入 %d · 跳过 %d · 失败 %d · 待补 %d\n",
				result.Imported, result.Skipped, result.Failed, result.Pending)
			fmt.Fprintln(out, "提示：原私钥文件仍在盘上（vault 另存一份）。")
			if serversStdinIsTTY() {
				fmt.Fprint(out, "现在逐台补全吗？可运行 `sshmgr servers complete` 继续。")
			} else {
				fmt.Fprintln(out, "逐台补全：sshmgr servers complete")
			}
			return nil
		},
	}
	c.Flags().StringVar(&file, "file", "", "config file (default ~/.ssh/config)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "print what would happen; write nothing")
	c.Flags().StringVar(&profile, "profile", "", "grant every imported server to this profile (must exist)")
	return c
}

// printImportEntries 按配置顺序输出逐台导入状态，沿用既有报告行格式。
func printImportEntries(out io.Writer, result *servermanage.ImportResult) {
	if result == nil {
		return
	}
	for _, entry := range result.Entries {
		switch {
		case entry.Status == "imported":
			fmt.Fprintf(out, "%-20s imported %s\n", entry.Name, candidateNote(entry.Note, true))
		case entry.Status == "failed":
			fmt.Fprintf(out, "%-20s FAILED: %s\n", entry.Name, entry.Error)
		case strings.HasPrefix(entry.Status, "skipped:"):
			reason := strings.TrimPrefix(entry.Status, "skipped:")
			if strings.HasPrefix(reason, "existing-") {
				fmt.Fprintf(out, "%-20s %s\n", entry.Name, renderVaultSkip(reason))
			} else {
				fmt.Fprintf(out, "%-20s skip: %s\n", entry.Name, reason)
			}
		}
	}
}

// candidateNote 把计划里的凭据注释代号换成既有提示文案；imported 为真时是
// 执行结果行，否则是 dry-run 预览行。
func candidateNote(note string, imported bool) string {
	switch note {
	case servermanage.ImportNoteNoCredential:
		return "needs-credential"
	case servermanage.ImportNoteNeedsPassphrase:
		return "needs-passphrase ⚠（连接会失败；servers complete 或 servers edit --key-passphrase 补全）"
	case servermanage.ImportNoteKey:
		return "key"
	default:
		return note
	}
}

// profileByName finds a profile row by name; nil (not an error) when absent —
// the store has no name-keyed lookup, so this scans like profiles.go does.
func profileByName(s *store.Store, name string) (*models.Profile, error) {
	profs, err := s.ListProfiles()
	if err != nil {
		return nil, err
	}
	for _, p := range profs {
		if p.Name == name {
			return p, nil
		}
	}
	return nil, nil
}

// renderVaultSkip maps an import skip reason to its report wording.
func renderVaultSkip(reason string) string {
	switch reason {
	case "existing-name":
		return "skip-existing (name)"
	case "existing-endpoint":
		return "skip-existing (host:port:user)"
	default:
		return "skip-existing (" + reason + ")"
	}
}

// expandTilde expands a bare "~" or "~/"-prefixed path to the user home dir.
// "~user/..." forms are left untouched (mirroring importer.ResolveKeyPaths).
func expandTilde(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// currentUserName is the ssh_config User fallback: the OS account name via
// user.Current, with env fallbacks (USERNAME on POSIX, USER on Windows).
func currentUserName() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if v := os.Getenv("USERNAME"); v != "" {
		return v
	}
	return os.Getenv("USER")
}
