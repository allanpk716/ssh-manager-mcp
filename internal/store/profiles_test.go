package store

import (
	"strings"
	"testing"
	"time"

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

// v0.8.4: SyncServers is the full-sync grant API — unchecked rows removed,
// missing added, all-or-nothing on unknown ids; empty set clears.
func TestSyncServersReplacesGrantSet(t *testing.T) {
	s := newTestStore(t)
	cid := mustCred(t, s, models.CredPassword, "pw")
	a, _ := s.AddServer(&models.Server{Name: "a", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	b, _ := s.AddServer(&models.Server{Name: "b", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	c, _ := s.AddServer(&models.Server{Name: "c", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	pid, _ := s.AddProfile("dev")
	if err := s.GrantServers(pid, []string{a, b}); err != nil {
		t.Fatal(err)
	}

	added, removed, err := s.SyncServers(pid, []string{a, c})
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 || removed != 1 {
		t.Fatalf("added=%d removed=%d, want 1/1", added, removed)
	}
	got, _ := s.ServersForProfile(pid)
	set := map[string]bool{}
	for _, id := range got {
		set[id] = true
	}
	if len(got) != 2 || !set[a] || !set[c] {
		t.Fatalf("grants = %v, want {a c}", got)
	}

	// empty set = deliberate revoke-all
	added, removed, err = s.SyncServers(pid, nil)
	if err != nil {
		t.Fatal(err)
	}
	if added != 0 || removed != 2 {
		t.Fatalf("clear: added=%d removed=%d, want 0/2", added, removed)
	}
	got, _ = s.ServersForProfile(pid)
	if len(got) != 0 {
		t.Fatalf("grants after clear = %v", got)
	}
}

func TestSyncServersUnknownIDFailsFast(t *testing.T) {
	s := newTestStore(t)
	cid := mustCred(t, s, models.CredPassword, "pw")
	a, _ := s.AddServer(&models.Server{Name: "a", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	pid, _ := s.AddProfile("dev")
	if err := s.GrantServers(pid, []string{a}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SyncServers(pid, []string{a, "bogus"}); err == nil {
		t.Fatal("unknown server id must fail the sync")
	}
	got, _ := s.ServersForProfile(pid)
	if len(got) != 1 || got[0] != a {
		t.Fatalf("failed sync must change nothing, got %v", got)
	}
}

func TestDeleteProfile(t *testing.T) {
	s := newTestStore(t)
	cid := mustCred(t, s, models.CredPassword, "pw")
	a, _ := s.AddServer(&models.Server{Name: "a", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	pid, _ := s.AddProfile("dev")
	if err := s.GrantServers(pid, []string{a}); err != nil {
		t.Fatal(err)
	}

	// referenced by a project → refused, project named in the error
	if _, _, err := s.AddProject("agent-x", pid); err != nil {
		t.Fatal(err)
	}
	err := s.DeleteProfile(pid)
	if err == nil || !strings.Contains(err.Error(), "agent-x") {
		t.Fatalf("referenced profile must be refused with the project named, got %v", err)
	}

	// unreferenced profile → deleted, grant rows gone with it
	pid2, _ := s.AddProfile("free")
	if err := s.GrantServers(pid2, []string{a}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProfile(pid2); err != nil {
		t.Fatal(err)
	}
	if ps, _ := s.ListProfiles(); len(ps) != 1 {
		t.Fatalf("profiles after delete = %v", ps)
	}
	if got, _ := s.ServersForProfile(pid2); len(got) != 0 {
		t.Fatalf("grant rows must be deleted with the profile, got %v", got)
	}
}

// v0.8.5: List/Get must fill the timestamps — they were never selected, so
// the TUI rendered Go zero time (0001-01-01) for every profile.
func TestProfileTimestampsFilled(t *testing.T) {
	s := newTestStore(t)
	before := time.Now().Add(-time.Minute)
	pid, err := s.AddProfile("dev")
	if err != nil {
		t.Fatal(err)
	}
	ps, err := s.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 || ps[0].CreatedAt.Before(before) || ps[0].UpdatedAt.Before(before) {
		t.Fatalf("List timestamps must be ~now, got %+v", ps)
	}
	gp, err := s.GetProfile(pid)
	if err != nil || gp == nil {
		t.Fatal(err)
	}
	if gp.CreatedAt.Before(before) {
		t.Fatalf("GetProfile CreatedAt = %v, want ~now", gp.CreatedAt)
	}
}

// v0.8.6: a REAL grant-set change must bump profiles.updated_at (same tx);
// a no-op must not. The TUI's 更新 column reads exactly this column.
// Timestamps are Unix-SECOND granularity — the sleeps step across second
// boundaries so strictly-After assertions are sound.
func TestGrantChangesBumpUpdatedAt(t *testing.T) {
	s := newTestStore(t)
	cid := mustCred(t, s, models.CredPassword, "pw")
	a, _ := s.AddServer(&models.Server{Name: "a", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	pid, _ := s.AddProfile("dev")
	before, _ := s.GetProfile(pid)

	time.Sleep(1100 * time.Millisecond)
	if err := s.GrantServers(pid, []string{a}); err != nil {
		t.Fatal(err)
	}
	after, _ := s.GetProfile(pid)
	if !after.UpdatedAt.After(before.UpdatedAt) {
		t.Fatalf("GrantServers must bump updated_at: %v -> %v", before.UpdatedAt, after.UpdatedAt)
	}

	// no-op sync (same set) keeps the timestamp
	_, _, err := s.SyncServers(pid, []string{a})
	if err != nil {
		t.Fatal(err)
	}
	noop, _ := s.GetProfile(pid)
	if !noop.UpdatedAt.Equal(after.UpdatedAt) {
		t.Fatalf("no-op sync must NOT bump updated_at: %v -> %v", after.UpdatedAt, noop.UpdatedAt)
	}

	time.Sleep(1100 * time.Millisecond)
	// removal bumps it again
	_, _, err = s.SyncServers(pid, nil)
	if err != nil {
		t.Fatal(err)
	}
	cleared, _ := s.GetProfile(pid)
	if !cleared.UpdatedAt.After(noop.UpdatedAt) {
		t.Fatalf("removal must bump updated_at: %v -> %v", noop.UpdatedAt, cleared.UpdatedAt)
	}
}
