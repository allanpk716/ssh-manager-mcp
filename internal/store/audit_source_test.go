package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestAuditSourceColumnsMigrateAndPersist(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "old-store.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE audit_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ts INTEGER NOT NULL,
		project_id TEXT,
		server_id TEXT,
		action TEXT NOT NULL,
		command TEXT,
		sudo INTEGER NOT NULL DEFAULT 0,
		status TEXT,
		exit_code INTEGER,
		duration_ms INTEGER
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO audit_log(ts,action,status) VALUES(1,'exec','ok')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dbPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.WriteAudit(AuditRow{
		TS:       time.Unix(2, 0),
		Action:   "web.login.success",
		Status:   "ok",
		Source:   AuditSourceWebUI,
		SourceIP: "192.0.2.10",
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.QueryAudit(AuditFilter{Limit: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("审计行数量 = %d，应为 2", len(rows))
	}
	got := rows[0]
	if got.Source != AuditSourceWebUI || got.SourceIP != "192.0.2.10" {
		t.Fatalf("新审计行来源 = %q/%q，应为 Web UI 与来源 IP", got.Source, got.SourceIP)
	}
	old := rows[1]
	if old.Source != "" || old.SourceIP != "" {
		t.Fatalf("旧审计行来源应保持为空，实际 %q/%q", old.Source, old.SourceIP)
	}
}
