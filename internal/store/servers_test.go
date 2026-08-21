package store

import (
	"database/sql"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

func mustCred(t *testing.T, s *Store, typ models.CredentialType, secret string) string {
	t.Helper()
	id, err := s.SetCredential(&models.Credential{Type: typ, Secret: []byte(secret)})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAddGetServer(t *testing.T) {
	s := newTestStore(t)
	cid := mustCred(t, s, models.CredPassword, "pw")
	id, err := s.AddServer(&models.Server{
		Name: "gpu-3090", Host: "10.0.0.5", Port: 22, User: "ubuntu",
		AuthMethod: models.AuthPassword, CredentialID: cid, Tags: []string{"gpu"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetServer(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "gpu-3090" || got.Host != "10.0.0.5" || got.User != "ubuntu" {
		t.Fatalf("server = %+v", got)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "gpu" {
		t.Fatalf("tags = %v", got.Tags)
	}
	if got.CreatedAt.IsZero() {
		t.Fatal("CreatedAt should be populated")
	}
}

func TestGetServerByName(t *testing.T) {
	s := newTestStore(t)
	cid := mustCred(t, s, models.CredPassword, "pw")
	if _, err := s.AddServer(&models.Server{Name: "web", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetServerByName("web")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "web" {
		t.Fatal("wrong server")
	}
	if _, err := s.GetServerByName("nope"); err != nil {
		t.Fatalf("missing by name should be nil,nil; got %v", err)
	}
}

func TestDeleteServer(t *testing.T) {
	s := newTestStore(t)
	cid := mustCred(t, s, models.CredPassword, "pw")
	id, _ := s.AddServer(&models.Server{Name: "x", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	if err := s.DeleteServer(id); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetServer(id)
	if got != nil {
		t.Fatal("server should be gone")
	}
}

func TestListServers(t *testing.T) {
	s := newTestStore(t)
	cid := mustCred(t, s, models.CredPassword, "pw")
	if _, err := s.AddServer(&models.Server{Name: "web", Host: "h1", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddServer(&models.Server{Name: "db", Host: "h2", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid}); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListServers()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("len = %d, want 2", len(list))
	}
	names := map[string]bool{}
	for _, srv := range list {
		names[srv.Name] = true
	}
	if !names["web"] || !names["db"] {
		t.Fatalf("expected web and db in %v", names)
	}
}

func TestServerDescriptionRoundTrip(t *testing.T) {
	s := newTestStore(t)
	cid := mustCred(t, s, models.CredPassword, "pw")
	const desc = "GPU 训练机, 8x A100 80GB"
	id, err := s.AddServer(&models.Server{
		Name: "gpu", Host: "h", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid, Description: desc,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetServer(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != desc {
		t.Fatalf("GetServer description = %q, want %q", got.Description, desc)
	}
	byName, err := s.GetServerByName("gpu")
	if err != nil {
		t.Fatal(err)
	}
	if byName.Description != desc {
		t.Fatalf("GetServerByName description = %q, want %q", byName.Description, desc)
	}
	list, _ := s.ListServers()
	if list[0].Description != desc {
		t.Fatalf("ListServers description = %q, want %q", list[0].Description, desc)
	}
}

func TestServerStructuredFieldsRoundTrip(t *testing.T) {
	s := newTestStore(t)
	cid := mustCred(t, s, models.CredPassword, "pw")
	const (
		location = "dc2 rack14"
		hardware = "8x A100 80GB, 1TB RAM"
		services = "postgres primary, prometheus"
		role     = "prod pg primary"
		caveats  = "do not reboot 02-03:00\nfailover is manual"
	)
	id, err := s.AddServer(&models.Server{
		Name: "db1", Host: "10.0.0.5", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid,
		Location: location, Hardware: hardware, Services: services, Role: role, Caveats: caveats,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetServer(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Location != location || got.Hardware != hardware || got.Services != services ||
		got.Role != role || got.Caveats != caveats {
		t.Fatalf("structured fields lost:\nlocation=%q hardware=%q services=%q role=%q caveats=%q",
			got.Location, got.Hardware, got.Services, got.Role, got.Caveats)
	}
	byName, _ := s.GetServerByName("db1")
	if byName.Role != role || byName.Caveats != caveats {
		t.Fatalf("GetServerByName lost fields: %+v", byName)
	}
	list, _ := s.ListServers()
	if list[0].Services != services {
		t.Fatalf("ListServers lost services: %q", list[0].Services)
	}

	// UpdateServer persists edits to the structured fields.
	loaded, _ := s.GetServer(id)
	loaded.Role = "prod pg replica"
	loaded.Caveats = "drained"
	if err := s.UpdateServer(loaded); err != nil {
		t.Fatalf("UpdateServer: %v", err)
	}
	updated, _ := s.GetServer(id)
	if updated.Role != "prod pg replica" || updated.Caveats != "drained" {
		t.Fatalf("UpdateServer did not persist structured fields: %+v", updated)
	}

	// Empty fields stay empty (nullable columns, scan as "").
	empty, _ := s.AddServer(&models.Server{
		Name: "bare", Host: "h", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid,
	})
	gotEmpty, _ := s.GetServer(empty)
	if gotEmpty.Location != "" || gotEmpty.Hardware != "" || gotEmpty.Services != "" ||
		gotEmpty.Role != "" || gotEmpty.Caveats != "" {
		t.Fatalf("unset fields should be empty: %+v", gotEmpty)
	}
}

// TestAddServerDuplicateName: a second AddServer under a live name must fail
// with the LOCALIZED duplicate-name error ("already exists"), not raw SQLite
// UNIQUE-constraint text.
func TestAddServerDuplicateName(t *testing.T) {
	s := newTestStore(t)
	cid := mustCred(t, s, models.CredPassword, "pw")
	if _, err := s.AddServer(&models.Server{Name: "gpu", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid}); err != nil {
		t.Fatal(err)
	}
	_, err := s.AddServer(&models.Server{Name: "gpu", Host: "h2", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("want localized duplicate-name error, got %v", err)
	}
}

func TestUpdateServer(t *testing.T) {
	s := newTestStore(t)
	cid := mustCred(t, s, models.CredPassword, "pw")
	id, err := s.AddServer(&models.Server{
		Name: "gpu", Host: "h", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid, Tags: []string{"a"}, Description: "old",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Edit mutable fields (incl. rename + description), keep id + cred.
	loaded, _ := s.GetServer(id)
	loaded.Name = "gpu2"
	loaded.Host = "newhost"
	loaded.Port = 2222
	loaded.User = "ops"
	loaded.Description = "8x A100"
	loaded.Tags = []string{"a", "b"}
	if err := s.UpdateServer(loaded); err != nil {
		t.Fatalf("UpdateServer: %v", err)
	}

	got, _ := s.GetServerByName("gpu2")
	if got == nil {
		t.Fatal("renamed server not found by new name")
	}
	if got.ID != id {
		t.Fatalf("id changed: got %s want %s", got.ID, id)
	}
	if got.Host != "newhost" || got.Port != 2222 || got.User != "ops" || got.Description != "8x A100" {
		t.Fatalf("fields not updated: %+v", got)
	}
	if len(got.Tags) != 2 {
		t.Fatalf("tags = %v", got.Tags)
	}
	if got.CredentialID != cid {
		t.Fatalf("credential_id changed without re-credential: %s", got.CredentialID)
	}
	if old, _ := s.GetServerByName("gpu"); old != nil {
		t.Fatal("old name should be free after rename")
	}

	// Re-credential: new cred → credential_id repointed, auth_method flips, id preserved.
	newCid := mustCred(t, s, models.CredPrivateKey, "KEY")
	loaded2, _ := s.GetServer(id)
	loaded2.CredentialID = newCid
	loaded2.AuthMethod = models.AuthPrivateKey
	if err := s.UpdateServer(loaded2); err != nil {
		t.Fatalf("UpdateServer re-credential: %v", err)
	}
	got2, _ := s.GetServer(id)
	if got2.CredentialID != newCid {
		t.Fatalf("credential_id = %s, want %s", got2.CredentialID, newCid)
	}
	if got2.AuthMethod != models.AuthPrivateKey {
		t.Fatalf("auth_method = %v, want private_key", got2.AuthMethod)
	}

	// UpdateServer on a missing id reports not-found.
	if err := s.UpdateServer(&models.Server{ID: "nonexistent", Name: "x", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid}); err == nil {
		t.Fatal("UpdateServer missing id should error")
	}
}

func TestServerFieldSizeCap(t *testing.T) {
	s := newTestStore(t)
	cid := mustCred(t, s, models.CredPassword, "pw")

	// Exactly 4096 bytes is allowed.
	atLimit := strings.Repeat("x", 4096)
	id, err := s.AddServer(&models.Server{
		Name: "ok", Host: "h", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid, Caveats: atLimit,
	})
	if err != nil {
		t.Fatalf("at-limit field should be accepted: %v", err)
	}

	// 4097 bytes is rejected with a field-named error.
	overLimit := strings.Repeat("x", 4097)
	_, err = s.AddServer(&models.Server{
		Name: "big", Host: "h", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid, Caveats: overLimit,
	})
	if err == nil {
		t.Fatal("over-limit caveats should be rejected")
	}
	if !strings.Contains(err.Error(), "caveats") {
		t.Fatalf("error should name the field, got: %v", err)
	}

	// UpdateServer also enforces the cap.
	loaded, _ := s.GetServer(id)
	loaded.Hardware = strings.Repeat("h", 4097)
	if err := s.UpdateServer(loaded); err == nil {
		t.Fatal("UpdateServer over-limit hardware should be rejected")
	}

	// Per-tag cap.
	_, err = s.AddServer(&models.Server{
		Name: "taggy", Host: "h", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid, Tags: []string{strings.Repeat("t", 4097)},
	})
	if err == nil || !strings.Contains(err.Error(), "tag") {
		t.Fatalf("over-limit tag should be rejected with a tag-named error, got: %v", err)
	}
}

// openTestStore opens a fresh store in t.TempDir() (store-package test helper
// for Plan 31; mirrors the mcpserver newStore pattern).
func openTestStore(t *testing.T) *Store {
	t.Helper()
	mk, _ := GenerateMasterKey()
	st, err := Open(t.TempDir()+"/t.db", mk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestExposeHostRoundTripRow: AddServer persists ExposeHost and every read
// path (by id / by name / list) returns it. Default (zero value) is false.
func TestExposeHostRoundTripRow(t *testing.T) {
	st := openTestStore(t)
	id, err := st.AddServer(&models.Server{
		Name: "bexposed", Host: "h1", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, ExposeHost: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddServer(&models.Server{
		Name: "amasked", Host: "h2", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, // ExposeHost zero value = false
	}); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetServer(id)
	if err != nil || got == nil {
		t.Fatalf("GetServer: %v %v", got, err)
	}
	if !got.ExposeHost {
		t.Fatal("GetServer: ExposeHost = false, want true")
	}
	byName, _ := st.GetServerByName("amasked")
	if byName == nil || byName.ExposeHost {
		t.Fatal("GetServerByName: want ExposeHost=false (default)")
	}
	all, err := st.ListServers()
	if err != nil || len(all) != 2 {
		t.Fatalf("ListServers: %d %v", len(all), err)
	}
	// ListServers ORDER BY name → amasked < bexposed
	if all[0].ExposeHost || !all[1].ExposeHost {
		t.Fatalf("ListServers ExposeHost = [%v %v], want [false true]", all[0].ExposeHost, all[1].ExposeHost)
	}
	// Full-row update must preserve the bit (updateServerTx writes the whole row).
	got.Name = "exposed2"
	if err := st.UpdateServer(got); err != nil {
		t.Fatal(err)
	}
	again, _ := st.GetServer(id)
	if again == nil || !again.ExposeHost {
		t.Fatal("UpdateServer dropped ExposeHost")
	}
}

// TestMigrateAddsExposeHostToLegacyDB locks the migrate() ordering claim
// (addColumnIfMissing BEFORE the rebuildServersNullable check — spec §2) and
// the rebuild's two column lists carrying expose_host. A pre-Plan-8-shaped DB
// (no metadata columns, credential_id NOT NULL) goes through BOTH the
// addColumn block and the rebuild inside one Open.
func TestMigrateAddsExposeHostToLegacyDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := dir + "/legacy.db"
	// Craft the legacy shape directly (pre-initSchema), like the DB a
	// pre-Plan-8 binary would have left on disk.
	ldb, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ldb.Exec(`CREATE TABLE servers(
		id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, host TEXT NOT NULL,
		port INTEGER NOT NULL, user TEXT NOT NULL, auth_method TEXT NOT NULL,
		credential_id TEXT NOT NULL, sudo_credential_id TEXT, tags TEXT,
		created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := ldb.Exec(`INSERT INTO servers(id,name,host,port,user,auth_method,credential_id,tags,created_at,updated_at)
		VALUES('srv-legacy','old-nuc','10.0.0.9',22,'ops','password','cred-x','[]',1,1)`); err != nil {
		t.Fatal(err)
	}
	if err := ldb.Close(); err != nil {
		t.Fatal(err)
	}

	mk, _ := GenerateMasterKey()
	st, err := Open(dbPath, mk) // runs migrate() then initSchema
	if err != nil {
		t.Fatalf("Open on legacy DB: %v", err)
	}
	defer st.Close()

	got, err := st.GetServerByName("old-nuc")
	if err != nil || got == nil {
		t.Fatalf("GetServerByName after migration: %v %v", got, err)
	}
	if got.ExposeHost {
		t.Fatal("legacy row must migrate to ExposeHost=false (v0.9 breaking default)")
	}
	// The row itself must survive the rebuild dance.
	if got.ID != "srv-legacy" || got.Host != "10.0.0.9" || got.User != "ops" {
		t.Fatalf("legacy row data lost: %+v", got)
	}
}
