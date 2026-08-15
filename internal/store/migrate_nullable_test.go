package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// legacyServersShape is the pre-Plan-20 (v0.7) shape: servers.credential_id is
// TEXT NOT NULL with an FK on credentials(id). profiles + profile_servers are
// included so the nullable rebuild's FK-off dance is exercised against a REAL
// child table (profile_servers REFERENCES servers ON DELETE CASCADE) — without
// the conn-pinned PRAGMA foreign_keys=OFF, the DROP TABLE would either be
// blocked by that FK or silently cascade-delete the grant rows.
const legacyServersShape = `
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
  description TEXT DEFAULT '',
  location TEXT DEFAULT '',
  hardware TEXT DEFAULT '',
  services TEXT DEFAULT '',
  role TEXT DEFAULT '',
  caveats TEXT DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE profiles (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE profile_servers (
  profile_id TEXT NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
  server_id TEXT NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
  PRIMARY KEY (profile_id, server_id)
);
`

// seedLegacyDB hand-builds a v0.7-shape DB at dbPath: one credential, one
// server bound to it, one profile, one grant. Direct SQL (no Store methods) —
// the current code forbids the shapes we need to seed, and the FK only checks
// row existence, so a fake secret_blob is fine.
func seedLegacyDB(t *testing.T, dbPath string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(legacyServersShape); err != nil {
		t.Fatal(err)
	}
	seed := []string{
		`INSERT INTO credentials (id,type,secret_blob,passphrase_blob,created_at,updated_at) VALUES ('c1','password',x'00',NULL,1,1)`,
		`INSERT INTO servers (id,name,host,port,user,auth_method,credential_id,sudo_credential_id,tags,description,location,hardware,services,role,caveats,created_at,updated_at)
		 VALUES ('s1','gpu','192.0.2.10',22,'deploy','password','c1',NULL,'["prod"]','box','','','','edge cache','',1,1)`,
		`INSERT INTO profiles (id,name,created_at,updated_at) VALUES ('p1','team-a',1,1)`,
		`INSERT INTO profile_servers (profile_id,server_id) VALUES ('p1','s1')`,
	}
	for _, q := range seed {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
}

// openStoreAt opens (and thereby migrates) the store at dbPath, registering cleanup.
func openStoreAt(t *testing.T, dbPath string) *Store {
	t.Helper()
	mk := make([]byte, 32)
	randRead(t, mk)
	s, err := Open(dbPath, mk) // runs migrate()
	if err != nil {
		t.Fatalf("Open(%s): %v", dbPath, err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// credentialIDNotNull reads the on-disk notnull flag directly (pragma
// table-valued function — no dependency on the columnNullable helper under test).
func credentialIDNotNull(t *testing.T, s *Store) int {
	t.Helper()
	var notnull int
	if err := s.db.QueryRow(`SELECT "notnull" FROM pragma_table_info('servers') WHERE name='credential_id'`).Scan(&notnull); err != nil {
		t.Fatalf("pragma_table_info: %v", err)
	}
	return notnull
}

// TestMigrateCredentialIDNullable (Plan 20 C0): a legacy NOT NULL
// servers.credential_id DB is rebuilt nullable on Open, legacy rows and grants
// survive, the FK stays enforced, and credential-less CRUD works afterwards.
func TestMigrateCredentialIDNullable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "store.db")
	seedLegacyDB(t, dbPath)
	st := openStoreAt(t, dbPath)

	// legacy server intact: credential still bound and readable
	srv, err := st.GetServerByName("gpu")
	if err != nil || srv == nil || srv.CredentialID != "c1" {
		t.Fatalf("legacy server lost: %v %v", srv, err)
	}
	// grants survived the table rebuild (profile_servers FKs servers — the
	// pooling/conn-pinning hazard this migration must handle)
	if ids, err := st.ServersForProfile("p1"); err != nil || len(ids) != 1 || ids[0] != "s1" {
		t.Fatalf("grants lost across rebuild: %v %v", ids, err)
	}
	// on-disk schema proof: credential_id is now nullable (notnull == 0)
	if nn := credentialIDNotNull(t, st); nn != 0 {
		t.Fatalf("servers.credential_id still NOT NULL after migrate (notnull=%d)", nn)
	}
	// FK still enforced on new writes: a bogus credential_id must be rejected
	if _, err := st.db.Exec(`INSERT INTO servers (id,name,host,port,user,auth_method,credential_id,tags,created_at,updated_at) VALUES ('bad','bad','h',22,'u','password','nope','[]',1,1)`); err == nil {
		t.Fatal("FK on credential_id lost after rebuild: bogus credential_id insert accepted")
	}

	// credential-less insert now succeeds
	if _, err := st.AddServer(&models.Server{Name: "bare", Host: "h", Port: 22, User: "u"}); err != nil {
		t.Fatalf("credential-less insert failed: %v", err)
	}
	got, _ := st.GetServerByName("bare")
	if got == nil || got.CredentialID != "" || got.AuthMethod != "" {
		t.Fatalf("want empty credential fields, got %+v", got)
	}
	// SQL-layer proof: the credential-less row stores NULL, not '' (the FK on
	// credential_id has no '' row to reference — '' would violate it)
	var nullCred int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM servers WHERE credential_id IS NULL`).Scan(&nullCred); err != nil {
		t.Fatal(err)
	}
	if nullCred != 1 {
		t.Fatalf("credential-less row must store NULL credential_id, found %d NULL row(s)", nullCred)
	}
	// ListServers surfaces both forms
	all, _ := st.ListServers()
	if len(all) != 2 {
		t.Fatalf("want 2 servers, got %d", len(all))
	}
}

// TestMigrateCredentialIDNullableIdempotent: reopening a migrated DB must be a
// no-op (columnNullable reports nullable → no second rebuild, no error).
func TestMigrateCredentialIDNullableIdempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "store.db")
	seedLegacyDB(t, dbPath)
	st := openStoreAt(t, dbPath) // migrates
	if _, err := st.AddServer(&models.Server{Name: "bare", Host: "h", Port: 22, User: "u"}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	st2 := openStoreAt(t, dbPath) // migrate runs again → must be a no-op
	got, err := st2.GetServerByName("bare")
	if err != nil || got == nil || got.CredentialID != "" {
		t.Fatalf("reopen after migration lost credential-less row: %v %v", got, err)
	}
	if ids, err := st2.ServersForProfile("p1"); err != nil || len(ids) != 1 {
		t.Fatalf("grants lost on reopen: %v %v", ids, err)
	}
}

// TestFreshSchemaCredentialIDNullable: a brand-new DB (Open → initSchema) gets
// the nullable column directly — no rebuild, and credential-less CRUD works.
func TestFreshSchemaCredentialIDNullable(t *testing.T) {
	s := newTestStore(t)
	if nn := credentialIDNotNull(t, s); nn != 0 {
		t.Fatalf("fresh servers.credential_id must be nullable, notnull=%d", nn)
	}
	if _, err := s.AddServer(&models.Server{Name: "bare", Host: "h", Port: 22, User: "u"}); err != nil {
		t.Fatalf("credential-less insert on fresh schema: %v", err)
	}
	got, _ := s.GetServerByName("bare")
	if got == nil || got.CredentialID != "" || got.AuthMethod != "" {
		t.Fatalf("want empty credential fields, got %+v", got)
	}
}
