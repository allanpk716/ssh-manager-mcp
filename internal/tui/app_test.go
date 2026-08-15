package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// newTestApp opens a temp store seeded with one server and builds the broker App.
func newTestApp(t *testing.T) App {
	t.Helper()
	dir := t.TempDir()
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "t.db"), mk)
	if err != nil {
		t.Fatalf("open temp store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	// Servers carry a FK to credentials — seed one first.
	credID, err := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("p")})
	if err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	if _, err := st.AddServer(&models.Server{
		Name: "gpu", Host: "192.0.2.10", User: "u", Port: 22,
		AuthMethod: models.AuthPassword, CredentialID: credID,
	}); err != nil {
		t.Fatalf("seed server: %v", err)
	}
	a, err := NewBrokerApp(st)
	if err != nil {
		t.Fatalf("NewBrokerApp: %v", err)
	}
	return a
}

func TestApp_TabCyclesPages(t *testing.T) {
	a := newTestApp(t)
	if a.page != pageServers {
		t.Fatalf("start page = %v", a.page)
	}
	m2, _ := a.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if m2.(App).page != pageProfiles {
		t.Fatalf("tab: %v", m2.(App).page)
	}
	m3, _ := m2.(App).Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	if m3.(App).page != pageServers {
		t.Fatalf("shift-tab wrap: %v", m3.(App).page)
	}
}

func TestApp_QuitOnQ(t *testing.T) {
	a := newTestApp(t)
	_, cmd := a.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if cmd == nil {
		t.Fatal("q must produce a quit cmd")
	}
}

// TestServersPageDispatch (Plan 20 T1, paying the Plan 18 T6 dispatch-table
// debt): the key→page→action mapping on the servers page, driven through the
// real Update path. a/e/d/i must open their form overlay (right title + the
// overlay's Init cmd comes back); g must be a NO-OP here — it is the profiles
// page's grant key, and per-page dispatch is what keeps the overlapping
// letters (a/e/d on servers AND projects) from swallowing each other. `i`
// (ssh-config import) landed in Task 10.
func TestServersPageDispatch(t *testing.T) {
	cases := []struct {
		key         string
		wantOverlay string // "" = the key must be a no-op on this page
	}{
		{"a", "新增服务器"},
		{"e", "编辑服务器"},
		{"d", "删除服务器"},
		{"i", "导入 ssh config"},
		{"g", ""},
	}
	for _, c := range cases {
		a := newTestApp(t) // fresh app per key: one seeded server at cursor 0
		m, cmd := a.Update(tea.KeyPressMsg{Code: rune(c.key[0]), Text: c.key})
		got := m.(App)
		if c.wantOverlay == "" {
			if got.overlay != nil || cmd != nil {
				t.Fatalf("key %q on servers page must be a no-op: overlay=%v cmd=%v", c.key, got.overlay, cmd)
			}
			continue
		}
		if got.overlay == nil {
			t.Fatalf("key %q must open the %q overlay, got none", c.key, c.wantOverlay)
		}
		if title := got.overlay.Title(); title != c.wantOverlay {
			t.Fatalf("key %q opened %q, want %q", c.key, title, c.wantOverlay)
		}
		if cmd == nil {
			t.Fatalf("key %q must return the opened overlay's Init cmd", c.key)
		}
	}
}

// TestServersPageFilterKey (Plan 20 T10): `!` toggles the ⚠ filter through the
// real Update path — no overlay, pure list state. The seeded gpu has no Role,
// so it is ⚠: unfiltered shows the prefixed row, filtered shows the same
// single row (the multi-row filter behavior is pinned page-level in
// TestServersPageWarnSortFilter).
func TestServersPageFilterKey(t *testing.T) {
	a := newTestApp(t)
	m, cmd := a.Update(tea.KeyPressMsg{Code: '!', Text: "!"})
	got := m.(App)
	if got.overlay != nil || cmd != nil {
		t.Fatalf("! must be a pure list toggle: overlay=%v cmd=%v", got.overlay, cmd)
	}
	sp, _ := got.pages[pageServers].(*serversPage)
	if !sp.warnOnly {
		t.Fatal("! must set warnOnly")
	}
	if rows := sp.Rows(); len(rows) != 1 || rows[0] != "⚠ gpu" {
		t.Fatalf("filtered rows: %v", rows)
	}
	// footer advertises leaving the filter once it is on
	if f := got.footer(); !strings.Contains(f, "[!]显示全部") {
		t.Fatalf("footer in filter mode: %q", f)
	}
	m2, _ := m.(App).Update(tea.KeyPressMsg{Code: '!', Text: "!"})
	if sp2, _ := m2.(App).pages[pageServers].(*serversPage); sp2.warnOnly {
		t.Fatal("second ! must clear the filter")
	}
}

// TestServersWarnViewSurvivesRefetch (Plan 20 T10): the ⚠ filter and the
// warn-first order must survive a data refresh (refetchPages is what every
// actionDoneMsg / tokenIssuedMsg path funnels through).
func TestServersWarnViewSurvivesRefetch(t *testing.T) {
	a := newTestApp(t)
	// seed a second, COMPLETE server so the filter has something to hide
	// (servers carry an FK to credentials — mint the row first)
	cid, err := a.st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("p")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.st.AddServer(&models.Server{
		Name: "done", Host: "192.0.2.99", User: "u", Port: 22,
		CredentialID: cid, Role: "app", AuthMethod: models.AuthPassword,
	}); err != nil {
		t.Fatal(err)
	}
	m, _ := a.Update(tea.KeyPressMsg{Code: '!', Text: "!"})
	got := m.(App)
	got.refetchPages()
	sp, _ := got.pages[pageServers].(*serversPage)
	if !sp.warnOnly {
		t.Fatal("refetchPages dropped the warnOnly filter")
	}
	rows := sp.Rows()
	if len(rows) != 1 || rows[0] != "⚠ gpu" {
		t.Fatalf("filter+sort after refetch: %v", rows)
	}
}

// TestBrokerApp_RoleLoadFailsClosed (fix round F3): a corrupt role.json must
// surface as an error from NewBrokerApp, not silently default the App to
// standalone — matching roles' fail-closed design (a broken state guides
// `clear`; a silent downgrade would mislabel a machine and misroute the [u]
// upgrade affordance).
func TestBrokerApp_RoleLoadFailsClosed(t *testing.T) {
	vd, _ := withRoleDirs(t) // pins SSHMGR_STORE → role.json lives in vd
	if err := os.WriteFile(filepath.Join(vd, "role.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "t.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := NewBrokerApp(st); err == nil {
		t.Fatal("a corrupt role.json must fail NewBrokerApp (fail closed), got nil error")
	}
}
