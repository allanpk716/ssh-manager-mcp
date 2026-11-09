package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// AuditRow is one auditable action.
type AuditRow struct {
	// ID 是数据库里的审计行编号；只读旁车文件没有数据库编号，值为 0。
	ID         int64
	TS         time.Time
	ProjectID  string // empty for owner (non-agent) actions
	ServerID   string
	Action     string // "exec"
	Command    string
	Sudo       bool
	Status     string // "ok" / "error" / "timeout"
	ExitCode   int
	DurationMS int64
	// Source 标记动作来自哪类入口：命令行、Web UI 或 agent。旧数据为空。
	Source string
	// SourceIP 是 Web UI 请求的客户端 IP；其他来源为空。
	SourceIP string
	// InjectedEnvironmentNames 记录这次命令实际注入的环境变量名。这里只
	// 允许出现变量名，绝不出现机密项值。
	InjectedEnvironmentNames []string
}

// 审计来源的固定值。来源列面向所有入口，值保持英文以便过滤与导出。
const (
	AuditSourceCLI   = "cli"
	AuditSourceWebUI = "web_ui"
	AuditSourceAgent = "agent"
)

func (s *Store) WriteAudit(r AuditRow) error {
	if s.readOnly {
		if s.auditSidecar == nil {
			return ErrReadOnly
		}
		// Append a JSONL line to the sidecar; never touch s.db. Offline audit is per-machine
		// and is NOT auto-merged back (single-direction, zero-merge — a grilled-in constraint).
		rec := r.JSONMap()
		b, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		b = append(b, '\n')
		_, err = s.auditSidecar.Write(b)
		return err
	}
	return insertAuditRow(s.db, r)
}

// writeAuditTx inserts one audit row on tx — the SAME nine-field mapping as
// WriteAudit's db path, so a mutation and its history commit atomically (Plan 42
// §3.3-8: pairing events ride the approve/finish/reject transaction; a rolled-back
// mint leaves zero audit rows). Pairing Actions use the `pair.` prefix enum
// (pair.enroll/pair.approve/pair.reject/pair.finish/pair.autorevoke/pair.mint/...);
// Command
// carries the sanitized JSON summary {"name":...,"profile":...,"ip":...} — NEVER
// any token/code/pin/sealed value.
func writeAuditTx(tx *sql.Tx, r AuditRow) error {
	return insertAuditRow(tx, r)
}

// insertAuditRow is the single shared INSERT (identical columns/bindings to the
// pre-refactor WriteAudit db path — behavior zero change) over the dbtx surface
// both *sql.DB (legacy tx-less path) and *sql.Tx satisfy.
func insertAuditRow(db dbtx, r AuditRow) error {
	var sudo int
	if r.Sudo {
		sudo = 1
	}
	names := any(nil)
	if len(r.InjectedEnvironmentNames) > 0 {
		encoded, err := json.Marshal(r.InjectedEnvironmentNames)
		if err != nil {
			return err
		}
		names = string(encoded)
	}
	_, err := db.Exec(
		`INSERT INTO audit_log (ts, project_id, server_id, action, command, sudo, status, exit_code, duration_ms, source, source_ip, injected_environment_names)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.TS.Unix(), nullableString(r.ProjectID), nullableString(r.ServerID),
		r.Action, r.Command, sudo, nullableString(r.Status), r.ExitCode, r.DurationMS,
		nullableString(r.Source), nullableString(r.SourceIP), names,
	)
	return err
}

// AuditRows returns the most recent audit rows (newest first), up to limit.
// Used by callers (and tests) that need to verify what was recorded.
func (s *Store) AuditRows(limit int) ([]AuditRow, error) {
	if limit <= 0 {
		limit = 1
	}
	rows, err := s.db.Query(
		`SELECT id, ts, project_id, server_id, action, command, sudo, status, exit_code, duration_ms, source, source_ip, injected_environment_names
		 FROM audit_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAuditRows(rows)
}

// AuditFilter narrows QueryAudit. Zero value = all rows, newest first.
type AuditFilter struct {
	Since      int64 // unix seconds; 0 = no lower bound
	ServerIDs  []string
	ProjectIDs []string
	OwnerOnly  bool // only rows with an empty project_id (owner actions)
	Actions    []string
	Statuses   []string
	Source     string // 空 表示不过滤来源
	SourceIP   string // 空 表示不过滤来源地址
	BeforeID   int64  // 0 表示不限制编号；正数只返回编号小于它的行
	Limit      int    // rows to return; 0 = unlimited; negative = error
}

// QueryAudit returns audit rows newest-first (ORDER BY id DESC — the
// AUTOINCREMENT pk IS the insertion order). Values go through placeholders
// exclusively; only the placeholder count is ever concatenated into SQL.
// Filter semantics are zero-existence-check by design: a value that matches
// nothing yields an empty result, never an error, so history rows of deleted
// entities stay reachable by their old ids (spec §1).
func (s *Store) QueryAudit(f AuditFilter) ([]AuditRow, error) {
	if f.Limit < 0 {
		// SQLite treats LIMIT -1 as UNLIMITED — never let a negative value
		// silently become a full scan (spec §2 second gate).
		return nil, fmt.Errorf("limit must be >= 0")
	}
	var conds []string
	var args []any
	if f.Since != 0 {
		conds = append(conds, "ts >= ?")
		args = append(args, f.Since)
	}
	in := func(col string, vals []string) {
		if len(vals) == 0 {
			return
		}
		ph := strings.Repeat("?,", len(vals))
		conds = append(conds, col+" IN ("+ph[:len(ph)-1]+")")
		for _, v := range vals {
			args = append(args, v)
		}
	}
	in("server_id", f.ServerIDs)
	in("project_id", f.ProjectIDs)
	in("action", f.Actions)
	in("status", f.Statuses)
	if f.OwnerOnly {
		conds = append(conds, "(project_id IS NULL OR project_id = '')")
	}
	if f.Source != "" {
		conds = append(conds, "source = ?")
		args = append(args, f.Source)
	}
	if f.SourceIP != "" {
		conds = append(conds, "source_ip = ?")
		args = append(args, f.SourceIP)
	}
	if f.BeforeID > 0 {
		conds = append(conds, "id < ?")
		args = append(args, f.BeforeID)
	}
	q := "SELECT id, ts, project_id, server_id, action, command, sudo, status, exit_code, duration_ms, source, source_ip, injected_environment_names FROM audit_log"
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY id DESC"
	if f.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, f.Limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAuditRows(rows)
}

// scanAuditRows is the shared row→AuditRow scan (extracted from AuditRows so
// both read paths stay byte-identical in interpretation).
func scanAuditRows(rows *sql.Rows) ([]AuditRow, error) {
	var out []AuditRow
	for rows.Next() {
		var r AuditRow
		var ts int64
		var projectID, serverID, action, command, status, source, sourceIP, injectedNames sql.NullString
		var sudo int
		var exitCode, durationMS sql.NullInt64
		if err := rows.Scan(&r.ID, &ts, &projectID, &serverID, &action, &command, &sudo, &status, &exitCode, &durationMS, &source, &sourceIP, &injectedNames); err != nil {
			return nil, err
		}
		r.TS = time.Unix(ts, 0)
		r.ProjectID = projectID.String
		r.ServerID = serverID.String
		r.Action = action.String
		r.Command = command.String
		r.Sudo = sudo == 1
		r.Status = status.String
		r.ExitCode = int(exitCode.Int64)
		r.DurationMS = durationMS.Int64
		r.Source = source.String
		r.SourceIP = sourceIP.String
		if injectedNames.Valid && injectedNames.String != "" {
			if err := json.Unmarshal([]byte(injectedNames.String), &r.InjectedEnvironmentNames); err != nil {
				return nil, fmt.Errorf("decode injected environment names: %w", err)
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// JSONMap is the shared map construction used by the offline sidecar
// (WriteAudit read-only branch) and the owner-facing `audit --json` output —
// one construction, byte-identical encoding in both consumers (json.Marshal
// key order for maps is lexicographic). Base fields are written as-is: no
// null, no omitempty (spec §3). The optional injection-name list appears only
// when a command actually injected environment variables.
func (r AuditRow) JSONMap() map[string]any {
	rec := map[string]any{
		"id":          r.ID,
		"ts":          r.TS.Unix(),
		"project_id":  r.ProjectID,
		"server_id":   r.ServerID,
		"action":      r.Action,
		"command":     r.Command,
		"sudo":        r.Sudo,
		"status":      r.Status,
		"exit_code":   r.ExitCode,
		"duration_ms": r.DurationMS,
		"source":      r.Source,
		"source_ip":   r.SourceIP,
	}
	if len(r.InjectedEnvironmentNames) > 0 {
		rec["injected_environment_names"] = r.InjectedEnvironmentNames
	}
	return rec
}
