package tui

import (
	"strings"
	"testing"

	"charm.land/huh/v2"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// stSetCred seeds one password credential (servers FK to credentials).
func stSetCred(st *store.Store) (string, error) {
	return st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
}

// srv builds a minimal password-auth server for grant tests.
func srv(name, cid string) *models.Server {
	return &models.Server{Name: name, Host: "h", User: "u", Port: 22,
		AuthMethod: models.AuthPassword, CredentialID: cid}
}

func TestGrantAction(t *testing.T) {
	st := newStore(t)
	pid, _ := st.AddProfile("p1")
	cid, _ := stSetCred(st)
	s1, _ := st.AddServer(srv("a", cid))
	s2, _ := st.AddServer(srv("b", cid))
	if err := st.GrantServers(pid, []string{s1, s2}); err != nil {
		t.Fatal(err)
	}
	ids, _ := st.ServersForProfile(pid)
	if len(ids) != 2 {
		t.Fatalf("granted %d", len(ids))
	}
	// idempotent re-grant (INSERT OR IGNORE)
	_ = st.GrantServers(pid, []string{s1})
	if ids, _ = st.ServersForProfile(pid); len(ids) != 2 {
		t.Fatalf("dup grant leaked: %d", len(ids))
	}
}

// TestGrantOptions: option VALUES must be server ids (GrantServers wants ids;
// names are labels only — same-name servers would otherwise collide).
func TestGrantOptions(t *testing.T) {
	servers := []*models.Server{
		{ID: "id-a", Name: "gpu"},
		{ID: "id-b", Name: "web"},
	}
	opts := grantOptions(servers)
	if len(opts) != 2 {
		t.Fatalf("opts: %d", len(opts))
	}
	for i, want := range []struct{ key, value string }{{"gpu", "id-a"}, {"web", "id-b"}} {
		if opts[i].Key != want.key || opts[i].Value != want.value {
			t.Fatalf("opt[%d] = (%s, %s), want (%s, %s)", i, opts[i].Key, opts[i].Value, want.key, want.value)
		}
	}
}

func TestNewGrantForm(t *testing.T) {
	var chosen []string
	f := newGrantForm([]*models.Server{{ID: "id-a", Name: "gpu"}}, &chosen)
	if f == nil {
		t.Fatal("nil form")
	}
}

// TestProfilesPage_DetailMemberNames: Detail must show server NAMES, not ids.
func TestProfilesPage_DetailMemberNames(t *testing.T) {
	st := newStore(t)
	pid, _ := st.AddProfile("team")
	cid, _ := stSetCred(st)
	s1, _ := st.AddServer(srv("gpu", cid))
	s2, _ := st.AddServer(srv("web", cid))
	if err := st.GrantServers(pid, []string{s1, s2}); err != nil {
		t.Fatal(err)
	}
	profiles, err := st.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	p := newProfilesPage(profiles, st)
	d := p.Detail()
	for _, want := range []string{"team", "2", "gpu", "web"} {
		if !strings.Contains(d, want) {
			t.Fatalf("detail missing %q:\n%s", want, d)
		}
	}
}

// compile-time: grantOptions feeds huh options.
var _ = huh.NewOption[string]
