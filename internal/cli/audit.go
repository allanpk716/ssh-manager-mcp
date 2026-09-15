package cli

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/store"
)

// newAuditCmd: `sshmgr audit` — the owner's read path over the vault's
// audit_log (backlog #16). Owner-only BY CONSTRUCTION: it goes through
// openUnlockedStore() (the master-key gate) and is never registered as an MCP
// tool — audit rows carry other agents' full command text and may contain
// secrets (spec §4).
func newAuditCmd() *cobra.Command {
	var (
		since    string
		servers  []string
		projects []string
		owner    bool
		actions  []string
		statuses []string
		limit    int
		asJSON   bool
	)
	c := &cobra.Command{
		Use:   "audit",
		Short: "Read the vault audit log (owner-only)",
		Long: "Read audit_log rows from the vault, newest first.\n" +
			"Filters: --since (30m | 1.5h | 7d | 2w | RFC3339 | local datetime | date),\n" +
			"--server/--project (name or id; unknown values simply match nothing),\n" +
			"--owner (rows with no project = owner actions), --action/--status, --limit.\n" +
			"--limit 0 = unlimited (audit_log has no auto-cleanup — mind big vaults).",
		RunE: func(cmd *cobra.Command, args []string) error {
			if owner && len(projects) > 0 {
				return fmt.Errorf("--owner and --project are mutually exclusive (--owner selects rows with NO project)")
			}
			if limit < 0 {
				return fmt.Errorf("--limit must be >= 0")
			}
			var sinceUnix int64
			if since != "" {
				v, err := parseAuditSince(since, time.Now())
				if err != nil {
					return err
				}
				sinceUnix = v
			}
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()

			probe := limit
			if probe > 0 {
				probe++ // fetch one extra row to detect truncation (spec §1)
			}
			rows, err := s.QueryAudit(store.AuditFilter{
				Since:      sinceUnix,
				ServerIDs:  resolveServerIDs(s, servers),
				ProjectIDs: resolveProjectIDs(s, projects),
				OwnerOnly:  owner,
				Actions:    actions,
				Statuses:   statuses,
				Limit:      probe,
			})
			if err != nil {
				return err
			}
			truncated := limit > 0 && len(rows) > limit
			if truncated {
				rows = rows[:limit]
			}

			out, errs := cmd.OutOrStdout(), cmd.ErrOrStderr()
			if asJSON {
				for _, r := range rows {
					b, err := json.Marshal(r.JSONMap())
					if err != nil {
						return err
					}
					fmt.Fprintln(out, string(b))
				}
				return nil
			}
			if limit == 0 {
				fmt.Fprintln(errs, "unlimited query: audit_log has no auto-cleanup — output may be large")
			}
			if len(rows) == 0 {
				fmt.Fprintln(out, "no matching audit rows")
				return nil
			}
			projNames, srvNames := auditNameMaps(s)
			for _, r := range rows {
				fmt.Fprintf(out, "%s  %s  %s  %s  %s  exit=%d  %dms  %s%s\n",
					r.TS.Local().Format("2006-01-02 15:04:05-07:00"),
					escapeAuditText(displayAuditEntity(r.ServerID, srvNames)),
					escapeAuditText(displayAuditProject(r.ProjectID, projNames)),
					escapeAuditText(r.Action),
					escapeAuditText(r.Status),
					r.ExitCode, r.DurationMS,
					sudoMark(r.Sudo),
					escapeAuditText(r.Command),
				)
			}
			if truncated {
				fmt.Fprintf(errs, "showing first %d rows (more exist) — use --limit 0 for full output\n", limit)
			}
			return nil
		},
	}
	c.Flags().StringVar(&since, "since", "", "lower bound: 30m/1.5h/7d/2w, RFC3339, local datetime, or plain date")
	c.Flags().StringSliceVar(&servers, "server", nil, "filter by server name or id (repeatable/comma)")
	c.Flags().StringSliceVar(&projects, "project", nil, "filter by project name or id (repeatable/comma)")
	c.Flags().BoolVar(&owner, "owner", false, "only owner (non-agent) rows — rows with no project")
	c.Flags().StringSliceVar(&actions, "action", nil, "filter by action (repeatable/comma; unknown values match nothing)")
	c.Flags().StringSliceVar(&statuses, "status", nil, "filter by status (repeatable/comma; unknown values match nothing)")
	c.Flags().IntVar(&limit, "limit", 100, "max rows (default 100; 0 = unlimited — audit_log has no auto-cleanup, mind big vaults)")
	c.Flags().BoolVar(&asJSON, "json", false, "JSONL output (same nine fields as the offline sidecar)")
	return c
}

// parseAuditSince (spec §1): a single relative duration (integer or decimal
// number + exactly one of m/h/d/w), else — in order — RFC3339 with offset, a
// local offset-less datetime, or a plain local date.
func parseAuditSince(s string, now time.Time) (int64, error) {
	if d, ok := parseRelativeDuration(s); ok {
		return now.Add(-d).Unix(), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t.Unix(), nil
		}
	}
	return 0, fmt.Errorf("invalid --since %q: use a relative duration (one number + one unit, e.g. 30m, 1.5h, 7d, 2w) or an absolute time (RFC3339 like 2026-08-20T09:00:00+08:00, local datetime 2026-08-20T09:00:00, or date 2026-08-20)", s)
}

func parseRelativeDuration(s string) (time.Duration, bool) {
	units := map[byte]time.Duration{'m': time.Minute, 'h': time.Hour, 'd': 24 * time.Hour, 'w': 7 * 24 * time.Hour}
	if len(s) < 2 {
		return 0, false
	}
	d, ok := units[s[len(s)-1]]
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(s[:len(s)-1], 64)
	if err != nil || f <= 0 {
		return 0, false
	}
	return time.Duration(f * float64(d)), true
}

// resolveServerIDs / resolveProjectIDs: name-first, else the raw value as-is.
// Zero existence checking (spec §1): filters only narrow the result set — a
// value matching nothing yields an empty result, never an error, so history
// rows of deleted entities stay reachable by their old ids. Name uniqueness is
// schema-enforced (servers.name / projects.name are NOT NULL UNIQUE), so a
// name hit is always at most one entity.
func resolveServerIDs(s *store.Store, vals []string) []string {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if srv, _ := s.GetServerByName(v); srv != nil {
			v = srv.ID
		}
		out = append(out, v)
	}
	return out
}

func resolveProjectIDs(s *store.Store, vals []string) []string {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if p, _ := s.GetProjectByName(v); p != nil {
			v = p.ID
		}
		out = append(out, v)
	}
	return out
}

func auditNameMaps(s *store.Store) (proj, srv map[string]string) {
	proj, srv = map[string]string{}, map[string]string{}
	if ps, err := s.ListProjects(); err == nil {
		for _, p := range ps {
			proj[p.ID] = p.Name
		}
	}
	if ss, err := s.ListServers(); err == nil {
		for _, x := range ss {
			srv[x.ID] = x.Name
		}
	}
	return proj, srv
}

// displayAuditEntity: empty id → "(none)" (no server context, e.g. project-level
// owner actions); unresolvable id → first 8 chars + "…(deleted)"; else the name.
// Everything is escaped at the print site (spec §3).
func displayAuditEntity(id string, names map[string]string) string {
	if id == "" {
		return "(none)"
	}
	if n, ok := names[id]; ok && n != "" {
		return n
	}
	if len(id) > 8 {
		return id[:8] + "…(deleted)"
	}
	return id + "…(deleted)"
}

// displayAuditProject: empty project_id → "(owner)"; else the entity rules.
func displayAuditProject(id string, names map[string]string) string {
	if id == "" {
		return "(owner)"
	}
	return displayAuditEntity(id, names)
}

// escapeAuditText (spec §3): applied to EVERY dynamic text field of the human
// line. Closed set (backslash itself escapes first → reversible), line
// boundaries preserved, terminal control-sequence injection and invisible-
// character spoofing closed; other non-ASCII (CJK included) stays verbatim.
// Invalid UTF-8 bytes surface as U+FFFD via range-over-string, matching what
// json.Marshal does on the --json side.
func escapeAuditText(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r >= 0x80 && r <= 0x9f, r == 0x2028 || r == 0x2029,
			r == 0x200e || r == 0x200f, r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069,
			r == 0x00ad, r >= 0x200b && r <= 0x200d, r == 0x2060, r == 0xfeff:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func sudoMark(b bool) string {
	if b {
		return "[sudo]  "
	}
	return ""
}
