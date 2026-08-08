package store

import (
	"database/sql"
	"testing"
	"time"
)

func TestWriteAuditPersistsRow(t *testing.T) {
	s := newTestStore(t)
	err := s.WriteAudit(AuditRow{
		TS: time.Now(), ServerID: "srv1", Action: "exec",
		Command: "ls", Sudo: true, Status: "ok", ExitCode: 0, DurationMS: 12,
	})
	if err != nil {
		t.Fatal(err)
	}
	var action, cmd string
	var sudo int
	err = s.db.QueryRow(`SELECT action, command, sudo FROM audit_log WHERE server_id=?`, "srv1").
		Scan(&action, &cmd, &sudo)
	if err == sql.ErrNoRows {
		t.Fatal("audit row not found")
	}
	if action != "exec" || cmd != "ls" || sudo != 1 {
		t.Fatalf("got action=%q cmd=%q sudo=%d", action, cmd, sudo)
	}
}
