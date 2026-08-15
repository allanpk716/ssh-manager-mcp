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

// TestWizard_CloseStoreNilSafe pins the Run cleanup path (review C1): st is
// nil on every early exit (first-screen q, T4/T5 placeholder, stepVaultErr),
// so closeStore must be a no-op there — not a nil-deref panic.
func TestWizard_CloseStoreNilSafe(t *testing.T) {
	withRoleDirs(t)
	w := newWizardForTest() // first screen: st guaranteed nil
	if w.st != nil {
		t.Fatal("precondition: fresh wizard must have nil st")
	}
	w.closeStore() // the exact call Run makes after the program exits
	// and the vault-error shape explicitly called out by the review
	wv := newWizardForTest()
	wv.step, wv.form = stepVaultErr, nil
	wv.closeStore()
}

// TestStepFormDone_StandaloneVaultErrNoPanic pins review C2: a fresh
// chooseRole(standalone) whose enterStandalone fails leaves step=stepVaultErr
// and form=nil; stepFormDone must return (w, nil) instead of Init-ing the nil
// form, and Update(formDoneMsg) on that model must not panic either.
func TestStepFormDone_StandaloneVaultErrNoPanic(t *testing.T) {
	vd, _ := withRoleDirs(t)
	seedWizardVault(t, vd)
	// Make the vault EXIST but LOCKED: remove the master key so
	// wizEnsureVault fails without touching anything else.
	if err := os.Remove(filepath.Join(vd, "master.key.plain")); err != nil {
		t.Fatal(err)
	}
	w := newWizardForTest()
	w.askShare = true // simulate q2 answered; next stepFormDone picks the role
	w.ans.share = "self"
	m, _ := w.stepFormDone() // ← used to panic on w.form.Init() with nil form
	wm, ok := m.(wizardModel)
	if !ok {
		t.Fatalf("want wizardModel, got %T", m)
	}
	if wm.step != stepVaultErr {
		t.Fatalf("want stepVaultErr, got %d", wm.step)
	}
	if wm.form != nil {
		t.Fatal("vault-error step must carry a nil form")
	}
	// Update through formDoneMsg on the broken-state model: must be a no-op.
	if _, cmd := wm.Update(formDoneMsg{}); cmd != nil {
		t.Fatalf("formDoneMsg on stepVaultErr must not produce a cmd, got %v", cmd)
	}
}

// TestWizard_ResumeSkipsCompletedEntities pins resume idempotency (review I1):
// quitting mid-flow and re-running must never mint a SECOND profile
// (hostname-2) or project. Full case (profile + project both exist) resumes
// straight at the .mcp.json finish screen and finishing creates nothing.
func TestWizard_ResumeSkipsCompletedEntities(t *testing.T) {
	vd, _ := withRoleDirs(t)
	seedWizardVault(t, vd)
	st := openVault(t)
	pid, err := st.AddProfile("host-x")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AddProject("proj-x", pid); err != nil {
		t.Fatal(err)
	}
	st.Close()

	w := newWizardForRole(roles.Launch{Kind: roles.LaunchBroker, Role: roles.RoleStandalone, ResumeSetup: true})
	if w.step != stepMcpConfig {
		t.Fatalf("resume with profile+project must land on mcpConfig, got step=%d", w.step)
	}
	if w.ov == nil {
		t.Fatal("resume must show the finish overlay")
	}
	// Any key on the finish screen completes setup — and must not create any
	// new entity.
	m, cmd := w.Update(formDoneMsg{})
	if cmd == nil {
		t.Fatal("finish screen key must trigger wizFinish")
	}
	final, _ := m.Update(cmd()) // wizFinish → wizardDoneMsg → done/next
	if wm, ok := final.(wizardModel); !ok || !wm.done || wm.next != "broker" {
		t.Fatalf("finish must set done+broker handoff, got %+v", final)
	}
	// The invariant under test: counts unchanged after the resumed run.
	st2 := openVault(t)
	defer st2.Close()
	profiles, err := st2.ListProfiles()
	if err != nil || len(profiles) != 1 || profiles[0].Name != "host-x" {
		t.Fatalf("resume must not create profiles: %+v %v", profiles, err)
	}
	projects, err := st2.ListProjects()
	if err != nil || len(projects) != 1 || projects[0].Name != "proj-x" {
		t.Fatalf("resume must not create projects: %+v %v", projects, err)
	}
	w.closeStore() // Run's cleanup — also releases the db file for TempDir removal
}

// TestWizard_ResumeReusesExistingProfile pins the half-done case (review I1):
// profile exists but no project → resume skips the server loop AND profile
// creation, reuses the EXISTING profile id, and only runs project+finish.
func TestWizard_ResumeReusesExistingProfile(t *testing.T) {
	vd, _ := withRoleDirs(t)
	seedWizardVault(t, vd)
	st := openVault(t)
	pid, err := st.AddProfile("host-y")
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	w := newWizardForRole(roles.Launch{Kind: roles.LaunchBroker, Role: roles.RoleStandalone, ResumeSetup: true})
	if w.step != stepProject {
		t.Fatalf("resume with profile but no project must land on stepProject, got step=%d", w.step)
	}
	if w.form == nil {
		t.Fatal("project step must have a form")
	}
	if w.data.profileID != pid {
		t.Fatalf("must reuse existing profile id %q, got %q", pid, w.data.profileID)
	}
	// The resumed flow's only mutation: mint the project bound to the
	// EXISTING profile.
	w.data.projName = "proj-y"
	msg := w.submitProject()()
	if _, ok := msg.(tokenIssuedMsg); !ok {
		t.Fatalf("want tokenIssuedMsg, got %#v", msg)
	}
	st2 := openVault(t)
	defer st2.Close()
	profiles, err := st2.ListProfiles()
	if err != nil || len(profiles) != 1 {
		t.Fatalf("resume must not create profiles: %+v %v", profiles, err)
	}
	projects, err := st2.ListProjects()
	if err != nil || len(projects) != 1 {
		t.Fatalf("want exactly one new project: %+v %v", projects, err)
	}
	if projects[0].ProfileID != pid {
		t.Fatalf("new project must bind the existing profile %q, got %q", pid, projects[0].ProfileID)
	}
	w.closeStore() // Run's cleanup — also releases the db file for TempDir removal
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
