package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// TestMigrateOldRowReadable pins the CRITICAL migration regression: a row that existed
// BEFORE the structured-metadata columns were added must remain readable after Open()
// migrates the DB. The original bug shipped addColumnIfMissing with plain "TEXT" (no
// DEFAULT), so ALTER TABLE ADD COLUMN left pre-existing rows with SQL NULL; scanServer
// (servers.go) scans those columns into a Go string, and database/sql errors with
// "converting NULL to string is unsupported" — making every upgraded vault unreadable.
//
// The fix ships DEFAULT ” on both the schema and ALTER paths, plus a back-fill UPDATE
// for DBs already bitten by the buggy release. This test pins BOTH paths:
//   - "added-by-migrate": old-shape DB lacks the columns; Open adds them via ADD COLUMN
//     ... DEFAULT ” and the pre-existing row reads back with "" metadata.
//   - "back-fill-null": already-buggy DB has the columns but SQL NULL in them; Open's
//     back-fill UPDATE repairs them in place and the row reads back with "" metadata.
func TestMigrateOldRowReadable(t *testing.T) {
	t.Run("added-by-migrate", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "old.db")
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(oldShapeServersProjects); err != nil { // pre-Plan-8 shape: no metadata cols
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO credentials (id,type,secret_blob,passphrase_blob,created_at,updated_at) VALUES ('c1','password',x'00',NULL,1,1)`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO servers (id,name,host,port,user,auth_method,credential_id,sudo_credential_id,tags,created_at,updated_at) VALUES ('s1','srv','h',22,'u','password','c1',NULL,'[]',1,1)`); err != nil {
			t.Fatal(err)
		}
		db.Close()

		mk := make([]byte, 32)
		randRead(t, mk)
		s, err := Open(path, mk) // migrates: ADD COLUMN ... DEFAULT ''
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer s.Close()

		got, err := s.GetServer("s1")
		if err != nil {
			t.Fatalf("GetServer on migrated row ERRORED (NULL-scan bug): %v", err)
		}
		assertEmptyMetadata(t, got)
	})

	t.Run("back-fill-null", func(t *testing.T) {
		// Simulate an already-broken DB: it ran the buggy release's migration (ADD COLUMN
		// TEXT, no DEFAULT), so the columns exist but the pre-existing row carries SQL
		// NULL in all six. The back-fill UPDATE in migrate() must repair those rows
		// in place — otherwise the upgrade path for operators who hit the buggy release
		// stays broken even after they install the fix.
		path := filepath.Join(t.TempDir(), "buggy.db")
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		// Post-buggy-migration shape directly: columns present, no DEFAULT, NULL values.
		if _, err := db.Exec(`
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
  description TEXT,
  location TEXT,
  hardware TEXT,
  services TEXT,
  role TEXT,
  caveats TEXT,
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
`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO credentials (id,type,secret_blob,passphrase_blob,created_at,updated_at) VALUES ('c1','password',x'00',NULL,1,1)`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO servers (id,name,host,port,user,auth_method,credential_id,sudo_credential_id,tags,description,location,hardware,services,role,caveats,created_at,updated_at) VALUES ('s1','srv','h',22,'u','password','c1',NULL,'[]',NULL,NULL,NULL,NULL,NULL,NULL,1,1)`); err != nil {
			t.Fatal(err)
		}
		db.Close()

		mk := make([]byte, 32)
		randRead(t, mk)
		s, err := Open(path, mk) // migrates: back-fill UPDATE repairs NULL → ''
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer s.Close()

		// SQL-layer proof that NULLs are gone — guards against a future refactor that
		// makes GetServer tolerate NULL while leaving the on-disk data corrupted.
		var nullCount int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM servers WHERE description IS NULL OR location IS NULL OR hardware IS NULL OR services IS NULL OR role IS NULL OR caveats IS NULL`).Scan(&nullCount); err != nil {
			t.Fatal(err)
		}
		if nullCount != 0 {
			t.Fatalf("back-fill left %d row(s) with NULL metadata columns", nullCount)
		}

		got, err := s.GetServer("s1")
		if err != nil {
			t.Fatalf("GetServer on back-filled row ERRORED: %v", err)
		}
		assertEmptyMetadata(t, got)
	})
}

// assertEmptyMetadata fails the test unless all six structured-metadata fields read back
// as the empty string — the value the DEFAULT ” / back-fill guarantees in place of SQL NULL.
func assertEmptyMetadata(t *testing.T, got *models.Server) {
	t.Helper()
	for _, f := range []struct{ name, val string }{
		{"description", got.Description},
		{"location", got.Location},
		{"hardware", got.Hardware},
		{"services", got.Services},
		{"role", got.Role},
		{"caveats", got.Caveats},
	} {
		if f.val != "" {
			t.Errorf("metadata field %s = %q, want %q (must be empty string, not NULL)", f.name, f.val, "")
		}
	}
}
