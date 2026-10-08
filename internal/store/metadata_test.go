package store

// Plan 51 T1/T2/T3 (spec §9): the metadata-edit store primitives — the
// broker-side audited CAS (UpdateForwardedMetadata), the client-side local
// mirror narrow gap (ApplyForwardedMetadata), and the revision column's
// ride-through on the snapshot chain.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"ssh-manager-mcp/internal/models"
)

// metaSeedServer inserts one server and returns its id (the metadata twin of
// the hostkeys tests' seeding).
func metaSeedServer(t *testing.T, s *Store, name string) string {
	t.Helper()
	id, err := s.AddServer(&models.Server{
		Name: name, Host: "192.0.2.10", Port: 22, User: "u",
		AuthMethod: models.AuthPassword,
		Hardware:   "old-hw",
		Caveats:    "old-caveat",
		Role:       "keep-me",
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func metaPtr(v string) *string { return &v }

func TestUpdateForwardedMetadata_CASSuccess(t *testing.T) {
	s := newTestStore(t)
	id := metaSeedServer(t, s, "gpu")

	newRev, err := s.UpdateForwardedMetadata(id, map[string]*string{
		"hardware": metaPtr("H200 x1"),
		"caveats":  metaPtr(""), // explicit empty = CLEAR
	}, 0, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if newRev != 1 {
		t.Fatalf("newRev = %d, want 1", newRev)
	}
	got, err := s.GetServer(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Hardware != "H200 x1" || got.Caveats != "" {
		t.Fatalf("applied fields wrong: hardware=%q caveats=%q", got.Hardware, got.Caveats)
	}
	if got.Role != "keep-me" {
		t.Fatalf("absent field must keep its value: role=%q", got.Role)
	}
	if got.Revision != 1 {
		t.Fatalf("row revision = %d, want 1", got.Revision)
	}
	rows, err := s.AuditRows(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Action != "meta-edit" || rows[0].ServerID != id || rows[0].Status != "ok" {
		t.Fatalf("audit row wrong: %+v", rows)
	}
	cmd := rows[0].Command
	for _, want := range []string{
		"server=gpu", "fields=caveats,hardware", "device=laptop",
		"via=meta-edit", "rev=0->1", `old.hardware="old-hw"`, `old.caveats="old-caveat"`,
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("audit command missing %q:\n%s", want, cmd)
		}
	}
}

func TestUpdateForwardedMetadata_Stale(t *testing.T) {
	s := newTestStore(t)
	id := metaSeedServer(t, s, "gpu")

	_, err := s.UpdateForwardedMetadata(id, map[string]*string{"role": metaPtr("x")}, 7, "laptop")
	var stale *ErrStaleRevision
	if !errors.As(err, &stale) || stale.Current != 0 {
		t.Fatalf("want ErrStaleRevision{Current:0}, got %v", err)
	}
	got, _ := s.GetServer(id)
	if got.Role != "keep-me" || got.Revision != 0 {
		t.Fatalf("stale write must change nothing: role=%q revision=%d", got.Role, got.Revision)
	}
	if rows, _ := s.AuditRows(1); len(rows) != 0 {
		t.Fatalf("stale write must leave no audit row, got %+v", rows)
	}
}

func TestUpdateForwardedMetadata_ServerGoneAndValidation(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.UpdateForwardedMetadata("nope", map[string]*string{"role": metaPtr("x")}, 0, "laptop"); !errors.Is(err, ErrServerGone) {
		t.Fatalf("absent row: want ErrServerGone, got %v", err)
	}
	id := metaSeedServer(t, s, "gpu")
	cases := []struct {
		name  string
		edits map[string]*string
		want  string
	}{
		{"unknown field", map[string]*string{"host": metaPtr("1.2.3.4")}, `unknown metadata field "host"`},
		{"null value", map[string]*string{"role": nil}, `is null`},
		{"oversize", map[string]*string{"role": metaPtr(strings.Repeat("x", 4097))}, "exceeds 4096-byte limit"},
	}
	for _, tc := range cases {
		_, err := s.UpdateForwardedMetadata(id, tc.edits, 0, "laptop")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: want error containing %q, got %v", tc.name, tc.want, err)
		}
	}
	if _, err := s.UpdateForwardedMetadata(id, map[string]*string{}, 0, "laptop"); err == nil {
		t.Fatal("empty edit must error")
	}
	got, _ := s.GetServer(id)
	if got.Revision != 0 {
		t.Fatalf("rejected writes must not bump revision, got %d", got.Revision)
	}
}

func TestUpdateForwardedMetadata_AuditFailureRollsBackEdit(t *testing.T) {
	s := newTestStore(t)
	id := metaSeedServer(t, s, "gpu")
	// Force the in-tx audit write to fail: no audit_log table, no audit row.
	if _, err := s.db.Exec(`DROP TABLE audit_log`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateForwardedMetadata(id, map[string]*string{"role": metaPtr("x")}, 0, "laptop"); err == nil {
		t.Fatal("audit failure must surface as an error")
	}
	// The edit itself must have rolled back with it (the whole point of the
	// single transaction).
	var hw string
	if err := s.db.QueryRow(`SELECT hardware FROM servers WHERE id=?`, id).Scan(&hw); err != nil {
		t.Fatal(err)
	}
	if hw != "old-hw" {
		t.Fatalf("edit must roll back with the audit failure, hardware=%q", hw)
	}
	var rev int64
	if err := s.db.QueryRow(`SELECT revision FROM servers WHERE id=?`, id).Scan(&rev); err != nil {
		t.Fatal(err)
	}
	if rev != 0 {
		t.Fatalf("revision must roll back too, got %d", rev)
	}
}

func TestOwnerWritePathsBumpRevision(t *testing.T) {
	s := newTestStore(t)
	id := metaSeedServer(t, s, "gpu")

	srv, _ := s.GetServer(id)
	srv.Hardware = "owner-1"
	if err := s.UpdateServer(srv); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetServer(id); got.Revision != 1 {
		t.Fatalf("after UpdateServer: revision=%d, want 1", got.Revision)
	}
	srv2, _ := s.GetServer(id)
	srv2.Hardware = "owner-2"
	if err := s.UpdateServerWithCredentials(srv2, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetServer(id); got.Revision != 2 {
		t.Fatalf("after UpdateServerWithCredentials: revision=%d, want 2", got.Revision)
	}
	// The CAS sees the owner writes: a device edit holding revision 0 is stale.
	_, err := s.UpdateForwardedMetadata(id, map[string]*string{"role": metaPtr("x")}, 0, "laptop")
	var stale *ErrStaleRevision
	if !errors.As(err, &stale) || stale.Current != 2 {
		t.Fatalf("owner+device contend on one token: want stale{Current:2}, got %v", err)
	}
}

func TestMigrateAddsRevisionColumn(t *testing.T) {
	// An old-shape servers table WITHOUT revision migrates on Open, and its
	// existing row back-fills to 0 ("never written").
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "old.db")
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE servers (
		id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, host TEXT NOT NULL, port INTEGER NOT NULL,
		user TEXT NOT NULL, auth_method TEXT NOT NULL, credential_id TEXT, sudo_credential_id TEXT,
		tags TEXT, description TEXT DEFAULT '', location TEXT DEFAULT '', hardware TEXT DEFAULT '',
		services TEXT DEFAULT '', role TEXT DEFAULT '', caveats TEXT DEFAULT '',
		expose_host INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO servers (id,name,host,port,user,auth_method,tags,created_at,updated_at)
		VALUES ('legacy','legacy','h',22,'u','password','',1,1)`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	mk := make([]byte, 32)
	randRead(t, mk)
	s, err := Open(dbPath, mk)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.GetServerByName("legacy")
	if err != nil || got == nil {
		t.Fatalf("migrated read: %v %v", got, err)
	}
	if got.Revision != 0 {
		t.Fatalf("legacy row revision = %d, want 0 (back-fill)", got.Revision)
	}
}

func TestTruncateForAudit(t *testing.T) {
	// Control characters become spaces (no forged audit lines).
	if got := truncateForAudit("a\x00b\x1bc\x7fd"); got != "a b c d" {
		t.Fatalf("control sanitize: %q", got)
	}
	// Short values pass through verbatim.
	if got := truncateForAudit("plain"); got != "plain" {
		t.Fatalf("short passthrough: %q", got)
	}
	// A multi-byte value is cut at ≤200 BYTES on a rune boundary and marked.
	long := strings.Repeat("板", 200) // 600 bytes of valid CJK
	got := truncateForAudit(long)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated value must carry the ellipsis marker: %q…?", got[:20])
	}
	body := strings.TrimSuffix(got, "…")
	if len(body) > 200 || !utf8.ValidString(body) {
		t.Fatalf("truncation must be ≤200 bytes and rune-aligned: len=%d valid=%v", len(body), utf8.ValidString(body))
	}
}

func TestApplyForwardedMetadata_NarrowGap(t *testing.T) {
	s := newTestStore(t)
	id := metaSeedServer(t, s, "gpu")
	// The client-side posture: read-only cache store.
	s.SetReadOnly(nil)

	if err := s.ApplyForwardedMetadata(id, map[string]*string{"hardware": metaPtr("H200")}, 3, 12345); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetServer(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Hardware != "H200" || got.Revision != 3 {
		t.Fatalf("mirror: hardware=%q revision=%d, want H200/3", got.Hardware, got.Revision)
	}
	if got.Role != "keep-me" || got.Caveats != "old-caveat" {
		t.Fatalf("mirror must touch ONLY the carried fields: %+v", got)
	}
	// updated_at comes from the BROKER response, not local now().
	if got.UpdatedAt.Unix() != 12345 {
		t.Fatalf("mirror updated_at = %d, want the broker's 12345", got.UpdatedAt.Unix())
	}
	// The narrow gap is NARROW: the generic mutation still refuses.
	if err := s.UpdateServer(got); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("UpdateServer on read-only store must stay ErrReadOnly, got %v", err)
	}
}

func TestApplyForwardedMetadata_MonotonicGuard(t *testing.T) {
	s := newTestStore(t)
	id := metaSeedServer(t, s, "gpu")
	// Local generation is already NEWER (revision 5) than the response (3) —
	// a hot rebuild swapped in someone else's write. The mirror is a no-op,
	// never a downgrade.
	if _, err := s.db.Exec(`UPDATE servers SET revision=5 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	s.SetReadOnly(nil)
	if err := s.ApplyForwardedMetadata(id, map[string]*string{"hardware": metaPtr("stale-value")}, 3, 1); err != nil {
		t.Fatalf("newer-local mirror must be idempotent success, got %v", err)
	}
	got, _ := s.GetServer(id)
	if got.Hardware != "old-hw" || got.Revision != 5 {
		t.Fatalf("no-op mirror changed the row: hardware=%q revision=%d", got.Hardware, got.Revision)
	}
}

func TestApplyForwardedMetadata_RowAbsent(t *testing.T) {
	s := newTestStore(t)
	s.SetReadOnly(nil)
	err := s.ApplyForwardedMetadata("missing", map[string]*string{"role": metaPtr("x")}, 1, 1)
	if err == nil || !strings.Contains(err.Error(), "not in local cache") {
		t.Fatalf("absent row must name cache pull, got %v", err)
	}
}

func TestSnapshotRevisionRoundTrip(t *testing.T) {
	s := newTestStore(t)
	id := metaSeedServer(t, s, "gpu")
	if _, err := s.UpdateForwardedMetadata(id, map[string]*string{"role": metaPtr("edge")}, 0, "laptop"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateForwardedMetadata(id, map[string]*string{"hardware": metaPtr("H200")}, 1, "laptop"); err != nil {
		t.Fatal(err)
	}
	snap, err := s.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Servers) != 1 || snap.Servers[0].Revision != 2 {
		t.Fatalf("ExportSnapshot revision = %+v, want 2", snap.Servers)
	}

	// Fresh store import keeps the broker's revision (a pull that zeroed it
	// would make every later edit a permanent 409).
	mk2 := make([]byte, 32)
	randRead(t, mk2)
	s2, err := Open(filepath.Join(t.TempDir(), "import.db"), mk2)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if err := s2.ImportSnapshot(snap); err != nil {
		t.Fatal(err)
	}
	got, _ := s2.GetServerByName("gpu")
	if got.Revision != 2 {
		t.Fatalf("ImportSnapshot revision = %d, want 2 (verbatim)", got.Revision)
	}
	// And the imported cache's CAS agrees with a broker at revision 2.
	if _, err := s2.UpdateForwardedMetadata(id, map[string]*string{"role": metaPtr("y")}, 2, "laptop2"); err != nil {
		t.Fatalf("post-import CAS at rev 2 must succeed, got %v", err)
	}
}

func TestImportSnapshotLegacyNoRevision(t *testing.T) {
	// A pre-Plan-51 snapshot (no revision key) imports as 0 — lossless.
	legacy := `{"version":1,"credentials":[],"servers":[{"id":"srv1","name":"old","host":"h","port":22,"user":"u","auth_method":"password","credential_id":"","sudo_credential_id":"","tags":"","description":"","location":"","hardware":"","services":"","role":"r","caveats":"","expose_host":false,"created_at":1,"updated_at":1}],"profiles":[],"grants":[],"projects":[],"host_keys":[],"audit":[]}`
	var snap Snapshot
	if err := json.Unmarshal([]byte(legacy), &snap); err != nil {
		t.Fatal(err)
	}
	s := newTestStore(t)
	if err := s.ImportSnapshot(&snap); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetServerByName("old")
	if got == nil || got.Revision != 0 {
		t.Fatalf("legacy snapshot revision: %+v", got)
	}
}
