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

// serverMetadataColumns is the set of structured-metadata columns Plan 8 added to
// servers. Shared by every migration-shape test so the column list lives in one place.
var serverMetadataColumns = []string{"description", "location", "hardware", "services", "role", "caveats"}

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
	for _, col := range serverMetadataColumns {
		if !hasColumn(t, s.db, "servers", col) {
			t.Fatalf("fresh servers table missing %s column", col)
		}
	}
	if !hasColumn(t, s.db, "projects", "status") {
		t.Fatal("fresh projects table missing status column")
	}
}

// TestMigrateAddsColumnsToOldShape: an old-shape DB gets the columns via Open's migrate().
//
// DEFERRED (finding #4 of the final review): the review also asked this test to seed an
// old-shape server row, run Open (migrate), then GetServer and assert the 5 new metadata
// columns read back as "". That assertion does NOT hold today: addColumnIfMissing adds
// the columns as plain "TEXT" (no DEFAULT), so pre-migration rows get SQL NULL, and
// scanServer scans them into a Go string (not sql.NullString) — which errors with
// "converting NULL to string is unsupported". So a pre-Plan-8 DB upgraded in place would
// leave every existing server row UNREADABLE via GetServer/GetServerByName/ListServers.
// Fixing it is a production change (addColumn DEFAULT ” or NullString scan) outside this
// review-fix brief's scope ("Do NOT touch production behavior beyond #2 and #6"). The
// deferral is tracked in the final-review-fix-report; the seed-row + GetServer assertion
// will be added alongside the migration fix.
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
	for _, col := range serverMetadataColumns {
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
