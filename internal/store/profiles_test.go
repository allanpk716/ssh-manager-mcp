package store

import (
	"testing"

	"ssh-manager-mcp/internal/models"
)

func TestProfileGrantAndList(t *testing.T) {
	s := newTestStore(t)
	cid := mustCred(t, s, models.CredPassword, "pw")
	a, _ := s.AddServer(&models.Server{Name: "a", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	b, _ := s.AddServer(&models.Server{Name: "b", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})

	pid, err := s.AddProfile("dev-ab")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.GrantServers(pid, []string{a, b}); err != nil {
		t.Fatal(err)
	}
	servers, err := s.ServersForProfile(pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 2 {
		t.Fatalf("want 2 servers, got %v", servers)
	}
}

func TestProfileGrantIsIdempotentAndAdditive(t *testing.T) {
	s := newTestStore(t)
	cid := mustCred(t, s, models.CredPassword, "pw")
	a, _ := s.AddServer(&models.Server{Name: "a", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	pid, _ := s.AddProfile("p")
	_ = s.GrantServers(pid, []string{a})
	_ = s.GrantServers(pid, []string{a}) // duplicate
	servers, _ := s.ServersForProfile(pid)
	if len(servers) != 1 {
		t.Fatalf("duplicate grant must stay 1, got %d", len(servers))
	}
}

func TestGetProfile(t *testing.T) {
	s := newTestStore(t)
	// Add a profile and retrieve it
	id, err := s.AddProfile("dev")
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.GetProfile(id)
	if err != nil {
		t.Fatal(err)
	}
	if p == nil {
		t.Fatal("wanted non-nil profile, got nil")
	}
	if p.Name != "dev" {
		t.Fatalf("wanted name 'dev', got %q", p.Name)
	}
	// Get nonexistent profile should return (nil, nil)
	p, err = s.GetProfile("nonexistent-id")
	if err != nil {
		t.Fatalf("wanted nil error for nonexistent profile, got %v", err)
	}
	if p != nil {
		t.Fatalf("wanted nil profile for nonexistent id, got %+v", p)
	}
}

func TestListProfiles(t *testing.T) {
	s := newTestStore(t)
	// Add two profiles
	_, err := s.AddProfile("a")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.AddProfile("b")
	if err != nil {
		t.Fatal(err)
	}
	// List them
	profiles, err := s.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 2 {
		t.Fatalf("wanted 2 profiles, got %d", len(profiles))
	}
	// Verify both names are present
	names := make(map[string]bool)
	for _, p := range profiles {
		names[p.Name] = true
	}
	if !names["a"] || !names["b"] {
		t.Fatalf("wanted profiles 'a' and 'b', got names %v", names)
	}
}
