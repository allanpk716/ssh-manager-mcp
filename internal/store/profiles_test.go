package store

import (
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// TestAddProfileDuplicateName: a second AddProfile under a live name must fail
// with the LOCALIZED duplicate-name error ("already exists"), not raw SQLite
// UNIQUE-constraint text.
func TestAddProfileDuplicateName(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.AddProfile("dev"); err != nil {
		t.Fatal(err)
	}
	_, err := s.AddProfile("dev")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("want localized duplicate-name error, got %v", err)
	}
}

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

// TestGrantServersUnknownIDFailsFast pins the in-transaction precheck: an
// unknown server id aborts the grant BEFORE any INSERT runs. The error names
// the offending id, a mid-batch bad id grants nothing (not even the valid ids
// listed before it), and the profile itself survives — the aborted grant must
// never leave a half-granted profile that a caller retry would orphan. The FK
// constraint remains as the fail-closed backstop behind the precheck.
func TestGrantServersUnknownIDFailsFast(t *testing.T) {
	s := newTestStore(t)
	cid := mustCred(t, s, models.CredPassword, "pw")
	real, err := s.AddServer(&models.Server{Name: "a", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	if err != nil {
		t.Fatal(err)
	}
	pid, err := s.AddProfile("p")
	if err != nil {
		t.Fatal(err)
	}
	err = s.GrantServers(pid, []string{real, "no-such-server"})
	if err == nil || !strings.Contains(err.Error(), "no-such-server") {
		t.Fatalf("want fail-fast unknown server error, got %v", err)
	}
	// Nothing changed — not even the valid id listed before the bad one.
	ids, err := s.ServersForProfile(pid)
	if err != nil || len(ids) != 0 {
		t.Fatalf("aborted grant must grant nothing: %v %v", ids, err)
	}
	// and the profile still exists
	p, err := s.GetProfile(pid)
	if err != nil || p == nil || p.Name != "p" {
		t.Fatalf("profile must survive the aborted grant: %+v %v", p, err)
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
