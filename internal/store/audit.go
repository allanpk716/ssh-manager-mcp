package store

import (
	"database/sql"
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
	var sudo int
	if r.Sudo {
		sudo = 1
	}
	_, err := s.db.Exec(
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
