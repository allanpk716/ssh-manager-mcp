package tui

import (
	"os"
	"path/filepath"
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
