package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// oldShapeServersProjects is the pre-Plan-8 shape: no servers.description, no projects.status.
// Used to prove migrate() upgrades an existing DB in place.
const oldShapeServersProjects = `
CREATE TABLE credentials (
  id TEXT PRIMARY KEY,
  type TEXT NOT NULL,
  secret_blob BLOB NOT NULL,
  passphrase_blob BLOB,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE servers (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  host TEXT NOT NULL,
  port INTEGER NOT NULL,
  user TEXT NOT NULL,
  auth_method TEXT NOT NULL,
  credential_id TEXT NOT NULL REFERENCES credentials(id),
  sudo_credential_id TEXT,
  tags TEXT,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE projects (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  token_hash BLOB NOT NULL,
  token_salt BLOB NOT NULL,
  token_prefix TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
`

func hasColumn(t *testing.T, db *sql.DB, table, col string) bool {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		if name == col {
			return true
		}
	}
	return false
}

// TestFreshSchemaHasNewColumns: a brand-new DB (via Open → initSchema) has both columns.
func TestFreshSchemaHasNewColumns(t *testing.T) {
	s := newTestStore(t)
	for _, col := range []string{"description", "location", "hardware", "services", "role", "caveats"} {
		if !hasColumn(t, s.db, "servers", col) {
			t.Fatalf("fresh servers table missing %s column", col)
		}
	}
	if !hasColumn(t, s.db, "projects", "status") {
		t.Fatal("fresh projects table missing status column")
	}
}

// TestMigrateAddsColumnsToOldShape: an old-shape DB gets the columns via Open's migrate().
func TestMigrateAddsColumnsToOldShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(oldShapeServersProjects); err != nil {
		t.Fatal(err)
	}
	db.Close()

	mk := make([]byte, 32)
	randRead(t, mk)
	s, err := Open(path, mk) // runs migrate()
	if err != nil {
		t.Fatalf("Open after migrate: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	for _, col := range []string{"description", "location", "hardware", "services", "role", "caveats"} {
		if !hasColumn(t, s.db, "servers", col) {
			t.Fatalf("migrate did not add servers.%s", col)
		}
	}
	if !hasColumn(t, s.db, "projects", "status") {
		t.Fatal("migrate did not add projects.status")
	}
}

// TestMigrateIdempotent: reopening a migrated DB (migrate runs again) must not error.
func TestMigrateIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idem.db")
	mk := make([]byte, 32)
	randRead(t, mk)
	s1, err := Open(path, mk)
	if err != nil {
		t.Fatal(err)
	}
	s1.Close()
	s2, err := Open(path, mk) // migrate runs again → must be a no-op, not "duplicate column"
	if err != nil {
		t.Fatalf("reopen (migrate again) failed: %v", err)
	}
	t.Cleanup(func() { s2.Close() })
}
