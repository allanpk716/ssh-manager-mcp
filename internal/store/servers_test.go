package store

import (
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
