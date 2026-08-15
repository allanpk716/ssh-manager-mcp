package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"ssh-manager-mcp/internal/paths"

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

// DefaultStorePath returns the on-disk vault location (program-fixed, spec §3.1/§5.1).
// SSHMGR_STORE overrides (test/migrate). Falls back to paths pkg (Win
// C:\ProgramData\ssh-manager\store.db; Unix /var/lib/ssh-manager/store.db).
func DefaultStorePath() (string, error) {
	return paths.StorePath()
}

// Open opens (or creates) the vault at path and ensures the schema. The master key decrypts credentials.
func Open(path string, masterKey []byte) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// Detect first-creation BEFORE sql.Open (modernc creates the file lazily on
	// first query, so we snapshot existence up front). HardenACL must run only
	// when THIS Open creates store.db — not on every open. Re-running HardenACL
	// on an existing store.db from a different account (e.g. serve runs as
	// LocalSystem but the file was created by the interactive user) would
	// rewrite the DACL under the service account's token: currentUserSID()
	// returns LocalSystem, which collides with the SYSTEM ACE (dedup'd by
	// SET_ACCESS), silently dropping the original user's ACE. That broke
	// §5.2's "store.db shares the master.key ACL" contract on NUC10 (F1).
	_, statErr := os.Stat(path)
	isNew := errors.Is(statErr, fs.ErrNotExist)
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
	// HardenACL ONLY on first creation (Plan 16 F1 fix, spec §5.2 xcheck codex
	// P6). On an existing store.db the ACL was set at creation time by whoever
	// first created it (interactive user or service account); re-running it
	// here under a different token would corrupt the DACL (see isNew note above).
	if isNew {
		if err := HardenACL(path); err != nil {
			db.Close()
			return nil, fmt.Errorf("harden ACL on store %q: %w", path, err)
		}
	}
	// HardenACL the SQLite WAL sidecars (-shm / -wal) whenever they exist.
	// Unlike store.db, these are created ON DEMAND by SQLite at first write
	// (not by store.Open), under whatever process first writes — so they miss
	// the creation-time HardenACL above and inherit the creating token's
	// default DACL (e.g. LocalSystem-created -shm ends up SYSTEM+Admins-read,
	// no user ACE). Under WAL mode every opener must write -shm (shared memory
	// index); a second process lacking write access to -shm gets "attempt to
	// write a readonly database" (Plan 16 F2 root cause). These sidecars are
	// owned by SQLite not the user, so re-ACLing them on every Open is safe
	// (unlike store.db, they have no "original creator's intent" to preserve)
	// and is the only way to catch them as they appear. best-effort, non-fatal:
	// a missing sidecar at Open time is normal (no writes yet); an ACL failure
	// is logged but does not block Open (the main store.db ACL is the real gate).
	hardenWALSidecars(path)
	mk := make([]byte, len(masterKey))
	copy(mk, masterKey)
	return &Store{db: db, masterKey: mk}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// hardenWALSidecars applies HardenACL to the SQLite WAL sidecar files (-shm,
// -wal) if they exist next to storePath. Best-effort, non-fatal: these files
// are created on demand by SQLite at first write and may not exist yet at
// Open time; a HardenACL failure is swallowed because the main store.db ACL
// is the real protection gate and a missing/bad sidecar ACL degrades
// gracefully (worst case: a concurrent process gets "readonly database", not
// a security exposure — the sidecars contain no plaintext credentials, only
// the WAL frame index / shared-memory page map). Plan 16 F2.
func hardenWALSidecars(storePath string) {
	for _, suffix := range []string{"-shm", "-wal"} {
		sidecar := storePath + suffix
		if _, err := os.Stat(sidecar); err == nil {
			// File exists — ensure its ACL matches the hardened contract. Ignore
			// errors (best-effort): we may lack WRITE_DAC under some service
			// tokens, and the next Open will retry.
			_ = HardenACL(sidecar)
		}
	}
}

// Checkpoint forces a WAL checkpoint (TRUNCATE) so the main database file
// contains every committed transaction and the -wal sidecar is empty. Used by
// callers that need a self-contained store.db for byte-level copying (the
// migrate-path command copies store.db between locations and must NOT miss
// transactions still buffered in the WAL). Safe to call on a WAL or rollback-
// journal store; idempotent.
func (s *Store) Checkpoint() error {
	_, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
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
	// Plan 20 C0: servers.credential_id becomes nullable (credential-less
	// servers — e.g. ssh-config imports of hosts without IdentityFile).
	// SQLite can't relax NOT NULL in place, so this is a guarded table rebuild.
	// serversExists was computed above: on a fresh DB migrate() runs BEFORE
	// initSchema, so the servers table may not exist yet — initSchema creates
	// it already nullable, nothing to rebuild.
	if serversExists {
		nullable, err := columnNullable(db, "servers", "credential_id")
		if err != nil {
			return err
		}
		if !nullable {
			if err := rebuildServersNullable(db); err != nil {
				return err
			}
		}
	}
	return nil
}

// columnNullable reports whether table.column exists and lacks the NOT NULL flag.
func columnNullable(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return notnull == 0, nil
		}
	}
	return false, rows.Err()
}

// rebuildServersNullable recreates servers with a nullable credential_id inside
// one transaction (SQLite ALTER-rename dance: create new → copy → drop old →
// rename). The new table is the CURRENT schemaSQL servers definition verbatim
// with the single difference that credential_id loses NOT NULL.
//
// FK enforcement is disabled for the dance: with foreign_keys ON, DROP TABLE
// servers performs an implicit DELETE FROM servers that would cascade-delete
// profile_servers grant rows (ON DELETE CASCADE). PRAGMA foreign_keys is
// PER-CONNECTION and database/sql pools connections, so the OFF/ON pair and
// the transaction MUST run pinned to ONE connection — otherwise the PRAGMA
// lands on one pooled conn while the tx runs on another and the dance dies on
// (or is cascade-damaged by) FK enforcement. The DSN pragma
// (store.go Open: _pragma=foreign_keys(1)) sets the flag for every NEW
// connection, so only this pinned conn needs the OFF/ON restore.
func rebuildServersNullable(db *sql.DB) error {
	ctx := context.Background()
	conn, err := db.Conn(ctx) // pin ONE pooled conn for the whole dance
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	// Restore enforcement on the pinned conn no matter how the dance ends
	// (deferred runs after tx.Rollback/commit handling, before conn.Close).
	defer func() { _, _ = conn.ExecContext(ctx, `PRAGMA foreign_keys=ON`) }()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after Commit
	stmts := []string{
		`CREATE TABLE servers_new (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  host TEXT NOT NULL,
  port INTEGER NOT NULL,
  user TEXT NOT NULL,
  auth_method TEXT NOT NULL,
  credential_id TEXT REFERENCES credentials(id),
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
)`,
		`INSERT INTO servers_new (id,name,host,port,user,auth_method,credential_id,sudo_credential_id,tags,description,location,hardware,services,role,caveats,created_at,updated_at)
SELECT id,name,host,port,user,auth_method,credential_id,sudo_credential_id,tags,description,location,hardware,services,role,caveats,created_at,updated_at FROM servers`,
		`DROP TABLE servers`,
		`ALTER TABLE servers_new RENAME TO servers`,
	}
	for _, s := range stmts {
		if _, err := tx.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("rebuild servers: %w", err)
		}
	}
	return tx.Commit()
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
  credential_id TEXT REFERENCES credentials(id),
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
