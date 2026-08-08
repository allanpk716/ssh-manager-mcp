package store

import "time"

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
