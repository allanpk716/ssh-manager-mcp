package tui

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/clientops"
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
	w.closeStore() // T4: the server flow opens the store — release it for TempDir
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
// starts at the role flow, not the first-screen picker. Since T4 the SERVER
// flow is real: on a fresh vault its resume enters the flow at the
// client-name step (the placeholder page is gone; only client/T5 remains).
func TestWizard_ResumeSkipsFirstScreen(t *testing.T) {
	withRoleDirs(t)
	w := newWizardForRole(roles.Launch{Kind: roles.LaunchBroker, Role: roles.RoleServer, ResumeSetup: true})
	if w.step != stepClientName || w.role != roles.RoleServer {
		t.Fatalf("resume wizard must skip picker into the server flow: step=%d role=%q", w.step, w.role)
	}
	if w.form == nil {
		t.Fatal("client-name step must carry a form")
	}
	if v := w.View().Content; !strings.Contains(v, "客户端") {
		t.Fatalf("resume view must show the server flow's first step:\n%s", v)
	}
	w.closeStore()
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

// TestWizard_VaultErrViewShowsSaveErr pins parity with stepRoleDone and the
// form steps: the stepVaultErr screen must render the saveErr banner too. Its
// footer promises 「角色已保存，重开 tui 会继续」 — with a failed role.json
// write that promise is false, and without the banner the user can never see
// why a re-run landed back on the first screen.
func TestWizard_VaultErrViewShowsSaveErr(t *testing.T) {
	withRoleDirs(t)
	w := newWizardForTest()
	w.step, w.form = stepVaultErr, nil
	w.err = errors.New("vault 初始化失败（测试）")
	w.saveErr = errors.New("写入被拒绝")
	v := w.View().Content
	if !strings.Contains(v, "初始化 vault 失败") {
		t.Fatalf("precondition: view must be the vaultErr screen:\n%s", v)
	}
	if !strings.Contains(v, "role.json 写入失败") {
		t.Fatalf("vaultErr view must surface the saveErr banner:\n%s", v)
	}
	if !strings.Contains(v, "写入被拒绝") {
		t.Fatalf("vaultErr view must include the saveErr detail:\n%s", v)
	}
}

// TestWizard_FooterHidesSavedWhenSaveErr (Plan 21 A1) pins every footer site
// that promises 「已保存」: with a failed role.json write (saveErr != nil) the
// promise is false, so the footer must swap to a failure variant that does NOT
// claim saved state — while keeping the site's q/r key prefix. Each site is
// constructed by direct step-field state (same-package pattern as
// TestWizard_VaultErrViewShowsSaveErr); the saveErr==nil build is the control
// case that must still show the original 「已保存」 footer.
func TestWizard_FooterHidesSavedWhenSaveErr(t *testing.T) {
	newBare := func() wizardModel { // fresh model, fields set per-site below
		return newWizardForTest()
	}
	sites := []struct {
		name     string                          // render site
		build    func(w wizardModel) wizardModel // land the model on that step
		failWant string                          // saveErr != nil: failure variant text that MUST appear
		okWant   string                          // saveErr == nil: original footer text that MUST appear
	}{
		{
			name: "stepRoleDone",
			build: func(w wizardModel) wizardModel {
				w.step = stepRoleDone // placeholder page (no form rendered)
				return w
			},
			failWant: "q 退出（role.json 写入失败，进度未保存）",
			okWant:   "q 退出（进度已保存，重开 tui 会继续）",
		},
		{
			name: "stepVaultErr",
			build: func(w wizardModel) wizardModel {
				w.step, w.form = stepVaultErr, nil
				w.err = errors.New("vault 初始化失败（测试）")
				return w
			},
			failWant: "r 重试 / q 退出（角色未保存，重开 tui 从头开始）",
			okWant:   "r 重试 / q 退出（角色已保存，重开 tui 会继续）",
		},
		{
			name: "stepDeviceIssue 失败态",
			build: func(w wizardModel) wizardModel {
				w.step, w.form = stepDeviceIssue, nil
				w.err = errors.New("签发设备码失败（测试）") // err branch carries the r 重试 footer
				return w
			},
			failWant: "r 重试 / q 暂停退出（角色未保存，重开 tui 从头开始）",
			okWant:   "r 重试 / q 暂停退出（角色已保存，重开 tui 会从设备码继续）",
		},
		{
			name: "stepDeviceIssue 等待态",
			build: func(w wizardModel) wizardModel {
				w.step, w.form = stepDeviceIssue, nil
				return w
			},
			failWant: "q 暂停退出（role.json 写入失败，进度未保存）",
			okWant:   "q 暂停退出（进度已保存）",
		},
		{
			name: "stepServeProbe",
			build: func(w wizardModel) wizardModel {
				w.step, w.form = stepServeProbe, nil
				return w
			},
			failWant: "q 暂停退出（role.json 写入失败，进度未保存）",
			okWant:   "q 暂停退出（进度已保存）",
		},
		{
			name: "表单步骤（default 渲染）",
			build: func(w wizardModel) wizardModel {
				w.askFirstServer() // stepServerAsk + its confirm form → the default form branch
				return w
			},
			failWant: "q 暂停退出（role.json 写入失败，进度未保存）",
			okWant:   "q 暂停退出（进度已保存，重开 tui 会继续）",
		},
	}
	for _, s := range sites {
		t.Run(s.name, func(t *testing.T) {
			withRoleDirs(t) // isolate role.json lookup (newWizard → roles.Load)

			// saveErr != nil: no 「已保存」 claim anywhere, failure variant present.
			w := s.build(newBare())
			w.saveErr = errors.New("写入被拒绝")
			v := w.View().Content
			if strings.Contains(v, "已保存") {
				t.Fatalf("saveErr footer must not claim 「已保存」:\n%s", v)
			}
			if !strings.Contains(v, s.failWant) {
				t.Fatalf("saveErr footer must show the failure variant %q:\n%s", s.failWant, v)
			}

			// Control — saveErr == nil: the original 「已保存」 footer is intact.
			w2 := s.build(newBare())
			v2 := w2.View().Content
			if !strings.Contains(v2, s.okWant) {
				t.Fatalf("nil saveErr footer must keep the original %q:\n%s", s.okWant, v2)
			}
		})
	}
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

// TestWizard_ClientFlow pins the client-role wizard (T5): choosing client on
// the first screen writes role.json (client location, setup_complete:false)
// and enters the clientModel-in-wizard-form with the connection form up and
// the source hint visible; a successful first pull leads through the finish
// screen to the client-panel handoff sentinel.
func TestWizard_ClientFlow(t *testing.T) {
	withRoleDirs(t)
	w := newWizardForTest()
	w.chooseRole(roles.RoleClient)
	if w.step != stepClient || w.client == nil {
		t.Fatalf("client role must enter the client wizard: step=%d client=%v", w.step, w.client)
	}
	// role.json lands at the CLIENT location, incomplete (safe-pause invariant).
	p, err := roles.RolePath(roles.RoleClient)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("role.json not written on choose: %v", err)
	}
	if !strings.Contains(string(b), `"role":"client"`) || !strings.Contains(string(b), `"setup_complete":false`) {
		t.Fatalf("role.json must record incomplete client setup: %s", b)
	}
	// The wizard view is the client form with the source hint on top. (The
	// field labels themselves render only after the form initializes; the
	// overlay title + hint pin the screen identity.)
	v := w.View().Content
	for _, want := range []string{"server 机", "编辑连接"} {
		if !strings.Contains(v, want) {
			t.Fatalf("client wizard view missing %q:\n%s", want, v)
		}
	}
	// Successful first pull → finish screen (--cache variant) → done+client.
	m, _ := w.Update(pullSucceededMsg{})
	if v := m.View().Content; !strings.Contains(v, "--cache") {
		t.Fatalf("post-pull screen must be the --cache finish screen:\n%s", v)
	}
	m2, cmd := m.Update(formDoneMsg{})
	if cmd == nil {
		t.Fatal("finish screen dismissal must run the completion cmd")
	}
	final, _ := m2.Update(cmd()) // wizFinishTo → wizardDoneMsg{next:"client"}
	if wm, ok := final.(wizardModel); !ok || !wm.done || wm.next != "client" {
		t.Fatalf("client finish must set done+client handoff, got %+v", final)
	}
	// Completed: role.json now setup_complete:true at the client location.
	b2, err := os.ReadFile(p)
	if err != nil || !strings.Contains(string(b2), `"setup_complete":true`) {
		t.Fatalf("role.json must record completed setup: %s (%v)", b2, err)
	}
}

// TestWizard_ClientResumeWithSavedCred: a resumed client wizard whose
// cache.auth.json already holds a complete cred skips the form (the panel's
// [s]/[c] keys drive the retry) instead of demanding a retyped masked code.
func TestWizard_ClientResumeWithSavedCred(t *testing.T) {
	withRoleDirs(t)
	// Pin the cache dir to an EXISTING temp dir — WriteCacheCred does not
	// MkdirAll (withRoleDirs only pins the cred dir's PARENT via APPDATA).
	t.Setenv("SSHMGR_CACHE_DIR", t.TempDir())
	cred := &clientops.CacheCred{
		URL:   "https://192.0.2.7:7878",
		Token: "code-1",
		Pin:   "sha256:" + strings.Repeat("c", 64),
	}
	if err := clientops.WriteCacheCred(cred); err != nil {
		t.Fatal(err)
	}
	w := newWizardForRole(roles.Launch{Kind: roles.LaunchClient, Role: roles.RoleClient, ResumeSetup: true})
	if w.step != stepClient || w.client == nil {
		t.Fatalf("resume must enter the client wizard: step=%d", w.step)
	}
	if w.client.overlay != nil {
		t.Fatal("resume with a complete cred must NOT reopen the form")
	}
	if w.client.cred == nil || w.client.cred.URL != cred.URL {
		t.Fatalf("resumed model must preload the stored cred: %+v", w.client.cred)
	}
}

// TestLaunchTarget pins the dispatch table: wizard on first run, broker/client
// on completed setups, and wizard again when ANY role's setup is incomplete
// (since T5 the resuming client re-enters the client wizard too).
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
		{roles.Launch{Kind: roles.LaunchClient, Role: roles.RoleClient, ResumeSetup: true}, "wizard"},
	} {
		if got := launchTarget(c.l); got != c.want {
			t.Fatalf("launchTarget(%+v) = %q, want %q", c.l, got, c.want)
		}
	}
}
