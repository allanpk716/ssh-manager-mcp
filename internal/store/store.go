package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
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
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
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
`
