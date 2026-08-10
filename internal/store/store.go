package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// readRand is a seam for tests (not overridden in production).
var readRand = func(b []byte) (int, error) { return rand.Read(b) }

// newID returns a short random base64url id.
func newID() string {
	b := make([]byte, 8)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func now() int64 { return time.Now().Unix() }

// Store is the encrypted credential vault. masterKey lives in memory while open.
type Store struct {
	db        *sql.DB
	masterKey []byte
}

// DefaultStorePath returns the on-disk vault location.
func DefaultStorePath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "ssh-manager", "store.db"), nil
}

// Open opens (or creates) the vault at path and ensures the schema. The master key decrypts credentials.
func Open(path string, masterKey []byte) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// Enable foreign_keys (OFF by default in SQLite — otherwise ON DELETE CASCADE
	// and FK constraints are dead code) and a 5s busy_timeout to wait on locks.
	// WAL mode improves concurrency; MaxOpenConns(1) serializes access so the
	// single SQLite writer never hits "database is locked" under parallel requests.
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite single-writer; serializes access and avoids "database is locked"
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := initSchema(db); err != nil {
		db.Close()
		return nil, err
	}
	mk := make([]byte, len(masterKey))
	copy(mk, masterKey)
	return &Store{db: db, masterKey: mk}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func initSchema(db *sql.DB) error {
	_, err := db.Exec(schemaSQL)
	return err
}

// migrate evolves the schema from prior pre-release shapes. host_keys was keyed by
// host only (PRIMARY KEY host); it is now keyed by host:port so same-host-different-
// port servers (host sshd:22 + container:2222) don't collide/clobber. Legacy host-only
// rows are port-ambiguous and are NOT secrets (regenerated via TOFU on next connect),
// so we drop and let CREATE rebuild. Idempotent: no-op on fresh and already-migrated DBs.
func migrate(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(host_keys)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	hasHostCol := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == "host" {
			hasHostCol = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if hasHostCol {
		if _, err := db.Exec(`DROP TABLE host_keys`); err != nil {
			return err
		}
	}
	// Plan 8: owner-notes (servers.description) + project lifecycle (projects.status).
	// CREATE TABLE IF NOT EXISTS does not evolve existing tables, so these ship as guarded
	// ALTER TABLE migrations; ADD COLUMN with NOT NULL DEFAULT back-fills existing rows.
	if err := addColumnIfMissing(db, "servers", "description", "TEXT"); err != nil {
		return err
	}
	if err := addColumnIfMissing(db, "projects", "status", "TEXT NOT NULL DEFAULT 'active'"); err != nil {
		return err
	}
	return nil
}

// addColumnIfMissing adds a column to a table iff the table exists and lacks the column
// (idempotent — safe to run every Open). On a fresh DB migrate() runs BEFORE initSchema,
// so the table may not exist yet; in that case skip (initSchema will create it WITH the
// column). Only an existing table missing the column gets ALTERed.
func addColumnIfMissing(db *sql.DB, table, column, decl string) error {
	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return err
	}
	defer rows.Close()
	sawAny := false
	for rows.Next() {
		sawAny = true
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return nil // already present
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !sawAny {
		return nil // table absent (pre-initSchema on fresh DB) — initSchema creates it with the column
	}
	_, err = db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, decl))
	return err
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS credentials (
  id TEXT PRIMARY KEY,
  type TEXT NOT NULL,
  secret_blob BLOB NOT NULL,
  passphrase_blob BLOB,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS servers (
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
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS profiles (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS profile_servers (
  profile_id TEXT NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
  server_id TEXT NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
  PRIMARY KEY (profile_id, server_id)
);
CREATE TABLE IF NOT EXISTS projects (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  token_hash BLOB NOT NULL,
  token_salt BLOB NOT NULL,
  token_prefix TEXT NOT NULL,
  profile_id TEXT NOT NULL REFERENCES profiles(id),
  status TEXT NOT NULL DEFAULT 'active',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS audit_log (
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
);
CREATE TABLE IF NOT EXISTS host_keys (
  host_port TEXT PRIMARY KEY,
  key_blob BLOB NOT NULL,
  created_at INTEGER NOT NULL
);
`
