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
