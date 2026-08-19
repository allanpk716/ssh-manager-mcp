package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
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
	for i, want := range []struct{ key, value string }{{"1. gpu", "id-a"}, {"2. web", "id-b"}} {
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

// v0.8.3 #3A: a chosen pre-filled with existing grants must arrive CHECKED —
// huh's Accessor() marks matching options selected at construction. Prove it
// behaviorally: submit untouched (Enter) through the routed loop; a working
// preselection writes the same ids back, an empty start would write [].
// The loop stops at formDoneMsg — in production the APP consumes it to close
// the overlay (driving formOverlay as the top model would loop on it).
// (formOverlay adapts *huh.Form — whose Update returns huh.Model — to tea.Model,
// same cast feedForm does.)
func TestNewGrantFormPreselectsExisting(t *testing.T) {
	chosen := []string{"id-a"}
	f := newGrantForm([]*models.Server{{ID: "id-a", Name: "gpu"}, {ID: "id-b", Name: "web"}}, &chosen)
	o := newFormOverlay("授权", f, func() tea.Cmd { return nil })
	_, cmd := o.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	for steps := 0; cmd != nil && steps < 50; steps++ {
		msg := cmd()
		if _, done := msg.(formDoneMsg); done {
			break // the App's close-the-overlay handoff — end of the loop here
		}
		_, next := o.Update(msg)
		cmd = next
	}
	if o.form.State != huh.StateCompleted {
		t.Fatalf("form state = %v, want completed", o.form.State)
	}
	if len(chosen) != 1 || chosen[0] != "id-a" {
		t.Fatalf("preselected grant must survive an untouched submit, got %v", chosen)
	}
}

// v0.8.4 full-sync: unchecking a preselected option and submitting must DROP
// it from the result — driven through the real multiselect keys (Down to the
// option, Space to uncheck, Enter to submit). Down/Space state changes apply
// synchronously on the keypress (their cmd chains are cosmetic); only Enter's
// chain (nextField/nextGroup round trip) must be fed back. Store semantics
// are pinned by TestSyncServersReplacesGrantSet; this pins the FORM side.
func TestGrantFormUncheckRemoves(t *testing.T) {
	chosen := []string{"id-a", "id-b"}
	f := newGrantForm([]*models.Server{{ID: "id-a", Name: "gpu"}, {ID: "id-b", Name: "web"}}, &chosen)
	o := newFormOverlay("授权", f, func() tea.Cmd { return nil })
	_, _ = o.Update(tea.KeyPressMsg{Code: tea.KeyDown})  // option 0 (gpu) → 1 (web)
	_, _ = o.Update(tea.KeyPressMsg{Code: tea.KeySpace}) // uncheck web
	_, cmd := o.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	for steps := 0; cmd != nil && steps < 50; steps++ {
		msg := cmd()
		if _, done := msg.(formDoneMsg); done {
			break // the App's close-the-overlay handoff — end of the loop here
		}
		_, next := o.Update(msg)
		cmd = next
	}
	if len(chosen) != 1 || chosen[0] != "id-a" {
		t.Fatalf("uncheck must remove id-b on submit, got %v", chosen)
	}
}

// v0.8.4: Profiles 页 `d` → Confirm → 删除落库(App 级,经路由门走完回环)。
func TestAppProfilesDeleteKey(t *testing.T) {
	st := newStore(t)
	if _, err := st.AddProfile("team"); err != nil {
		t.Fatal(err)
	}
	a, err := NewBrokerApp(st) // profile exists BEFORE the app fetches pages
	if err != nil {
		t.Fatal(err)
	}
	m, _ := a.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyTab})        // servers → profiles
	m, cmd := m.Update(tea.KeyPressMsg{Code: 'd', Text: "d"}) // delete confirm
	if cmd == nil {
		t.Fatal("'d' must open the confirm overlay")
	}
	m = drain(t, m, cmd)
	var c tea.Cmd
	m, c = m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"}) // Confirm 肯定单键(Enter 落否定项)
	m = drain(t, m, c)                                     // formDoneMsg → after → actionDoneMsg
	ps, err := st.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 0 {
		t.Fatalf("profile must be deleted, got %v", ps)
	}
	if m.(App).overlay != nil {
		t.Fatal("overlay must close after the delete")
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
	// v0.8.3 #2: the ROW description must show the member count too —
	// ListProfiles never fills ServerIDs, so newProfilesPage resolves them
	// (pre-fix this read "0 台服务器" no matter the grants).
	if got := (profileItem{pr: p.items[0]}).Description(); !strings.Contains(got, "2 台服务器") {
		t.Fatalf("row description must show the member count, got %q", got)
	}
	if len(p.items[0].ServerIDs) != 2 {
		t.Fatalf("ServerIDs = %d, want 2 (resolved at page construction)", len(p.items[0].ServerIDs))
	}
}

// compile-time: grantOptions feeds huh options.
var _ = huh.NewOption[string]
