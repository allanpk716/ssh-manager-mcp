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
	TS         time.Time
	ProjectID  string // empty for owner (non-agent) actions
	ServerID   string
	Action     string // "exec"
	Command    string
	Sudo       bool
	Status     string // "ok" / "error" / "timeout"
	ExitCode   int
	DurationMS int64
}

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
// (pair.approve/pair.reject/pair.finish/pair.autorevoke/pair.mint/...); Command
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
	_, err := db.Exec(
		`INSERT INTO audit_log (ts, project_id, server_id, action, command, sudo, status, exit_code, duration_ms)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		r.TS.Unix(), nullableString(r.ProjectID), nullableString(r.ServerID),
		r.Action, r.Command, sudo, nullableString(r.Status), r.ExitCode, r.DurationMS,
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
		`SELECT ts, project_id, server_id, action, command, sudo, status, exit_code, duration_ms
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
	Limit      int // rows to return; 0 = unlimited; negative = error
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
	q := "SELECT ts, project_id, server_id, action, command, sudo, status, exit_code, duration_ms FROM audit_log"
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
		var projectID, serverID, action, command, status sql.NullString
		var sudo int
		var exitCode, durationMS sql.NullInt64
		if err := rows.Scan(&ts, &projectID, &serverID, &action, &command, &sudo, &status, &exitCode, &durationMS); err != nil {
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
		out = append(out, r)
	}
	return out, rows.Err()
}

// JSONMap is the single nine-field map construction shared by the offline
// sidecar (WriteAudit read-only branch) and the owner-facing `audit --json`
// output — one construction, byte-identical encoding in both consumers
// (json.Marshal key order for maps is lexicographic). Zero values are written
// as-is: no null, no omitempty (spec §3).
func (r AuditRow) JSONMap() map[string]any {
	return map[string]any{
		"ts":          r.TS.Unix(),
		"project_id":  r.ProjectID,
		"server_id":   r.ServerID,
		"action":      r.Action,
		"command":     r.Command,
		"sudo":        r.Sudo,
		"status":      r.Status,
		"exit_code":   r.ExitCode,
		"duration_ms": r.DurationMS,
	}
}
