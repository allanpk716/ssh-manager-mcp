package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
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
// In read-only mode (offline cache), every mutation method returns ErrReadOnly and
// WriteAudit appends to auditSidecar instead of touching db. Set via SetReadOnly,
// AFTER ImportSnapshot during cache hydration.
type Store struct {
	db           *sql.DB
	masterKey    []byte
	readOnly     bool
	auditSidecar *os.File
}

// ErrReadOnly is returned by every mutation method when the store is in read-only
// (offline-cache) mode. The cache is a pulled snapshot — mutations belong on the server.
var ErrReadOnly = errors.New("store is read-only (offline cache); connect to the server to mutate")

// SetReadOnly puts the store in read-only mode: every mutation returns ErrReadOnly, and
// WriteAudit appends JSONL to auditSidecar (if non-nil) instead of inserting into audit_log.
// Called by cache hydration AFTER ImportSnapshot. auditSidecar may be nil (audit writes then
// return ErrReadOnly too).
func (s *Store) SetReadOnly(auditSidecar *os.File) {
	s.readOnly = true
	s.auditSidecar = auditSidecar
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
	// ALTER TABLE migrations; ADD COLUMN with DEFAULT back-fills existing rows (NULL would
	// break scanServer, which scans these into a Go string — see migrate_null_test.go).
	if err := addColumnIfMissing(db, "servers", "description", "TEXT DEFAULT ''"); err != nil {
		return err
	}
	if err := addColumnIfMissing(db, "servers", "location", "TEXT DEFAULT ''"); err != nil {
		return err
	}
	if err := addColumnIfMissing(db, "servers", "hardware", "TEXT DEFAULT ''"); err != nil {
		return err
	}
	if err := addColumnIfMissing(db, "servers", "services", "TEXT DEFAULT ''"); err != nil {
		return err
	}
	if err := addColumnIfMissing(db, "servers", "role", "TEXT DEFAULT ''"); err != nil {
		return err
	}
	if err := addColumnIfMissing(db, "servers", "caveats", "TEXT DEFAULT ''"); err != nil {
		return err
	}
	// Repair any rows that already carry NULL in these columns. Pre-DEFAULT migrations
	// (the buggy release) left existing rows with SQL NULL here, and rows inserted after
	// a downgrade would too. Idempotent: UPDATE ... WHERE IS NULL is a no-op once clean.
	// Gated on the table existing: migrate() runs BEFORE initSchema on a fresh DB, so the
	// servers table may not yet exist — in that case skip (initSchema creates it WITH the
	// DEFAULT '' baked in, so no back-fill is ever needed there).
	serversExists, err := tableExists(db, "servers")
	if err != nil {
		return err
	}
	if serversExists {
		for _, col := range []string{"description", "location", "hardware", "services", "role", "caveats"} {
			if _, err := db.Exec(fmt.Sprintf("UPDATE servers SET %s = '' WHERE %s IS NULL", col, col)); err != nil {
				return err
			}
		}
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

// tableExists reports whether a table is present in the DB. Used by migrate() to gate
// back-fill UPDATEs that would otherwise error on a fresh DB (where initSchema, which
// runs after migrate(), has not yet created the table).
func tableExists(db *sql.DB, table string) (bool, error) {
	var tmp int
	err := db.QueryRow(`SELECT 1 FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&tmp)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
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
  description TEXT DEFAULT '',
  location TEXT DEFAULT '',
  hardware TEXT DEFAULT '',
  services TEXT DEFAULT '',
  role TEXT DEFAULT '',
  caveats TEXT DEFAULT '',
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
CREATE TABLE IF NOT EXISTS cache_tokens (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  token_hash BLOB NOT NULL,
  token_salt BLOB NOT NULL,
  token_prefix TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'active',
  last_pull_at INTEGER,
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
