package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
)

// withRoleDirs isolates both role-file locations via env (SSHMGR_STORE pins the
// vault dir; XDG_CONFIG_HOME/APPDATA pins the user dir), mirroring the roles
// package's withDirs helper (Plan 19 T2). The master-key path and cache dir are
// also pinned so a real vault / cache on the dev machine
// (C:\ProgramData\ssh-manager, %AppData%\ssh-manager) can never leak into the
// wizard under test.
func withRoleDirs(t *testing.T) (vaultDir, userDir string) {
	t.Helper()
	vaultDir = t.TempDir()
	userDir = t.TempDir()
	t.Setenv("SSHMGR_STORE", filepath.Join(vaultDir, "store.db"))
	t.Setenv("SSHMGR_FILEKEY_PATH", filepath.Join(vaultDir, "master.key.plain"))
	t.Setenv("SSHMGR_MASTERKEY_HEX", "")
	t.Setenv("SSHMGR_CACHE_DIR", "")
	t.Setenv("SSHMGR_SERVE_CERT", "")
	t.Setenv("APPDATA", userDir) // os.UserConfigDir on Windows
	t.Setenv("XDG_CONFIG_HOME", userDir)
	return vaultDir, userDir
}

// seedWizardVault creates an unlocked vault at the pinned SSHMGR_STORE (mirror
// of roles_test's seedVault). Needed because ResolveMode hard-fails a
// standalone/server role.json whose vault is missing (spec §1.2 anomaly).
func seedWizardVault(t *testing.T, vaultDir string) {
	t.Helper()
	os.Remove(filepath.Join(vaultDir, "store.db"))
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(vaultDir, "store.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if err := os.WriteFile(filepath.Join(vaultDir, "master.key.plain"), mk, 0o600); err != nil {
		t.Fatal(err)
	}
}

// newWizardForTest builds a first-run wizard with empty isolation env active.
func newWizardForTest() wizardModel {
	return newWizard(roles.Launch{Kind: roles.LaunchWizard})
}

// TestWizard_FirstScreenSavesRole pins the anti-dead-state invariant (spec
// §2.1): choosing a role writes role.json IMMEDIATELY with
// setup_complete:false, so a later ResolveMode flags ResumeSetup and the next
// `tui` re-enters the wizard instead of dead-ending.
func TestWizard_FirstScreenSavesRole(t *testing.T) {
	vd, _ := withRoleDirs(t)
	seedWizardVault(t, vd) // server role.json without a vault is an anomaly, not a resume
	w := newWizardForTest()
	w.chooseRole(roles.RoleServer)
	b, err := os.ReadFile(filepath.Join(vd, "role.json"))
	if err != nil {
		t.Fatalf("role.json not written on choose: %v", err)
	}
	if want := `"role":"server"`; !strings.Contains(string(b), want) {
		t.Fatalf("role.json not written on choose: %s", b)
	}
	if want := `"setup_complete":false`; !strings.Contains(string(b), want) {
		t.Fatalf("role.json must record incomplete setup: %s", b)
	}
	// resume: setup_complete false
	l, err := roles.ResolveMode("")
	if err != nil || !l.ResumeSetup || l.Role != roles.RoleServer {
		t.Fatalf("must resume: %+v %v", l, err)
	}
}

// TestWizard_ResumeSkipsFirstScreen: a resumed launch (role already on disk)
// starts at the role flow, not the first-screen picker.
func TestWizard_ResumeSkipsFirstScreen(t *testing.T) {
	withRoleDirs(t)
	w := newWizardForRole(roles.Launch{Kind: roles.LaunchBroker, Role: roles.RoleServer, ResumeSetup: true})
	if w.step != stepRoleDone || w.role != roles.RoleServer {
		t.Fatalf("resume wizard must skip picker: step=%d role=%q", w.step, w.role)
	}
	if !strings.Contains(w.View().Content, "server") {
		t.Fatalf("placeholder view must name the role:\n%s", w.View().Content)
	}
}

// TestLaunchTarget pins the dispatch table: wizard on first run, broker/client
// on completed setups, and wizard again when a standalone/server setup is
// incomplete. Client resume stays on the client panel in THIS task (Task 5
// gives the client wizard its form).
func TestLaunchTarget(t *testing.T) {
	for _, c := range []struct {
		l    roles.Launch
		want string
	}{
		{roles.Launch{Kind: roles.LaunchWizard}, "wizard"},
		{roles.Launch{Kind: roles.LaunchBroker, Role: roles.RoleStandalone}, "broker"},
		{roles.Launch{Kind: roles.LaunchBroker, Role: roles.RoleServer}, "broker"},
		{roles.Launch{Kind: roles.LaunchClient, Role: roles.RoleClient}, "client"},
		{roles.Launch{Kind: roles.LaunchBroker, Role: roles.RoleStandalone, ResumeSetup: true}, "wizard"},
		{roles.Launch{Kind: roles.LaunchBroker, Role: roles.RoleServer, ResumeSetup: true}, "wizard"},
		{roles.Launch{Kind: roles.LaunchClient, Role: roles.RoleClient, ResumeSetup: true}, "client"},
	} {
		if got := launchTarget(c.l); got != c.want {
			t.Fatalf("launchTarget(%+v) = %q, want %q", c.l, got, c.want)
		}
	}
}
