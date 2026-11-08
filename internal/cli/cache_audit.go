package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/ops"
)

// cacheAuditCmd 提供 `sshmgr cache audit`：读取一个客户端实例目录中的本机审计
// 日志文件（JSON 按行追加），最新记录在前。客户端角色不打开权威 vault，因
// 此不经过 openUnlockedStore。
func cacheAuditCmd() *cobra.Command {
	var (
		instance string
		limit    int
		asJSON   bool
	)
	c := &cobra.Command{
		Use:   "audit",
		Short: "Read one cache instance's local execution audit log (newest first)",
		Long: "Read cache-audit.log next to one cache instance, newest first.\n" +
			"Rows are the JSON lines written while serving the local read-only cache;\n" +
			"damaged lines are skipped. --instance picks a named instance (default slot\n" +
			"when omitted). --limit N keeps the newest N rows (0 = unlimited).",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkInstanceFlag(instance); err != nil {
				return err
			}
			if limit < 0 {
				return fmt.Errorf("--limit must be >= 0")
			}
			rows, err := ops.ReadClientAudit(instance, limit)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				for _, row := range rows {
					blob, err := json.Marshal(row.JSONMap())
					if err != nil {
						return err
					}
					fmt.Fprintln(out, string(blob))
				}
				return nil
			}
			if len(rows) == 0 {
				fmt.Fprintln(out, "no matching audit rows")
				return nil
			}
			for _, row := range rows {
				project, server := row.ProjectID, row.ServerID
				if project == "" {
					project = "-"
				}
				if server == "" {
					server = "-"
				}
				fmt.Fprintf(out, "%s  project=%s server=%s  %s  %s  来源=%s 来源IP=%s exit=%d  %dms  %s%s\n",
					row.TS.Local().Format("2006-01-02 15:04:05-07:00"),
					escapeAuditText(project), escapeAuditText(server),
					escapeAuditText(row.Action), escapeAuditText(row.Status),
					escapeAuditText(row.Source), escapeAuditText(row.SourceIP),
					row.ExitCode, row.DurationMS,
					sudoMark(row.Sudo),
					escapeAuditText(row.Command),
				)
			}
			return nil
		},
	}
	c.Flags().StringVar(&instance, "instance", "", "read this named cache instance's audit log (default slot when omitted)")
	c.Flags().IntVar(&limit, "limit", 100, "max rows (default 100; 0 = unlimited)")
	c.Flags().BoolVar(&asJSON, "json", false, "JSONL output (same fields as the audit sidecar file)")
	return c
}
