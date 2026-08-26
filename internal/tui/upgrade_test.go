package tui

import (
	"io"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
)

// Plan 19 T6: the non-destructive standalone→server upgrade ([u] in the broker
// console). Three invariants under test:
//  1. [u] opens the serve segment only on a standalone App;
//  2. the full walkthrough persists role.json {server, setup_complete:true}
//     WITHOUT touching existing vault entities (servers/profiles/projects);
//     the only vault write is the client device code it mints;
//  3. a server-role App never advertises or dispatches [u].

// TestUpgrade_KeyOpensSegment (brief T6 test 1): standalone App + [u] → the
// segment is live with an overlay; a server-role App ignores [u].
func TestUpgrade_KeyOpensSegment(t *testing.T) {
	withServeCertDirs(t)
	a := newTestApp(t)
	a.role = roles.RoleStandalone
	m, _ := a.Update(tea.KeyPressMsg{Code: 'u', Text: "u"})
	am, ok := m.(App)
	if !ok || am.upg == nil || am.overlay == nil {
		t.Fatalf("[u] must open the upgrade segment (overlay non-nil), got upg=%v overlay=%v", am.upg, am.overlay)
	}
	if am.upg.step != upgAddr {
		t.Fatalf("segment must start at the addr form, got step=%d", am.upg.step)
	}
	// server role: [u] is inert
	b := newTestApp(t)
	b.role = roles.RoleServer
	mb, _ := b.Update(tea.KeyPressMsg{Code: 'u', Text: "u"})
	if bb := mb.(App); bb.upg != nil || bb.overlay != nil {
		t.Fatal("[u] must be inert on a server-role App")
	}
}

// TestUpgrade_FooterRoleGated (brief T6 test 3): [u]升级为 server appears in
// the footer only while the machine is standalone.
func TestUpgrade_FooterRoleGated(t *testing.T) {
	withServeCertDirs(t)
	a := newTestApp(t)
	a.role = roles.RoleStandalone
	if !strings.Contains(a.footer(), "[u]升级为 server") {
		t.Fatalf("standalone footer must advertise [u]: %s", a.footer())
	}
	b := newTestApp(t)
	b.role = roles.RoleServer
	if strings.Contains(b.footer(), "[u]") {
		t.Fatalf("server footer must NOT advertise [u]: %s", b.footer())
	}
}

// TestUpgrade_FullFlowPersistsRoleKeepsVault (brief T6 test 2): the complete
// walkthrough with a fake installer (SetServeInstaller seam) ends with
// role.json {server, setup_complete:true}, the App's role field refreshed
// (footer drops [u]), the 0.0.0.0:7878 binding honored, and the vault's
// servers/profiles/projects counts unchanged — the upgrade's only vault write
// is the client device code.
func TestUpgrade_FullFlowPersistsRoleKeepsVault(t *testing.T) {
	withServeCertDirs(t)
	orig := serveInstall
	defer func() { serveInstall = orig }()
	var bound string
	serveInstall = func(addr, tlsCert, tlsKey string, out io.Writer) error {
		bound = addr
		return nil
	}
	if err := roles.Save(roles.State{Role: roles.RoleStandalone, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}

	a := newTestApp(t)
	if a.role != roles.RoleStandalone {
		t.Fatalf("App must pick up standalone from role.json, got %q", a.role)
	}
	// Plan 39: the upgrade segment binds its device code to a profile — seed
	// exactly one so the auto-bind path runs (counted in profBefore below).
	if _, err := a.st.AddProfile("upg-bind"); err != nil {
		t.Fatal(err)
	}
	srvBefore, profBefore, projBefore := vaultCounts(t, a.st)

	m, _ := a.Update(tea.KeyPressMsg{Code: 'u', Text: "u"})
	m.(App).upg.serveAddr = "https://192.168.100.235:7878" // in-package: answer the addr form directly
	m2, _ := m.Update(formDoneMsg{})                       // addr form done → admin notice
	if ov := m2.(App).overlay; ov == nil || !strings.Contains(viewString(ov), "管理员") {
		t.Fatalf("addr form must be followed by the admin notice, got %v", ov)
	}
	m3, cmd := m2.Update(formDoneMsg{})                                     // admin ack → install cmd
	m4, _ := m3.Update(cmd())                                               // serveInstalledMsg{nil} → probe scheduled
	m5, _ := m4.Update(serveProbeMsg{ok: true, detail: "401 Unauthorized"}) // probe → result screen
	if ov := m5.(App).overlay; ov == nil || !strings.Contains(viewString(ov), "已安装") {
		t.Fatalf("clean install must show the ok banner, got %v", ov)
	}
	m6, _ := m5.Update(formDoneMsg{}) // result dismissed → client-name form
	m6.(App).upg.clientName = "laptop"
	m7, cmd2 := m6.Update(formDoneMsg{}) // name done → issue cmd
	m8, _ := m7.Update(cmd2())           // deviceCodeIssuedMsg → one-time screen
	if ov := m8.(App).overlay; ov == nil || !strings.Contains(viewString(ov), "设备码") {
		t.Fatalf("device code must open the one-time screen, got %v", ov)
	}
	m9, _ := m8.Update(formDoneMsg{}) // code dismissed → access card
	if ov := m9.(App).overlay; ov == nil || !strings.Contains(viewString(ov), "https://192.168.100.235:7878") {
		t.Fatalf("access card must carry the chosen addr, got %v", ov)
	}
	m10, _ := m9.Update(formDoneMsg{}) // card dismissed → upgrade completes

	fin := m10.(App)
	if fin.role != roles.RoleServer {
		t.Fatalf("App role must be server after upgrade, got %q", fin.role)
	}
	if fin.status != "已升级为 server" {
		t.Fatalf("status banner must read 已升级为 server, got %q", fin.status)
	}
	if strings.Contains(fin.footer(), "[u]") {
		t.Fatalf("footer must drop [u] after upgrade: %s", fin.footer())
	}

	st, err := roles.Load()
	if err != nil || st == nil || st.Role != roles.RoleServer || !st.SetupComplete {
		t.Fatalf("role.json must be {server, setup_complete:true}, got %+v %v", st, err)
	}
	if bound != "0.0.0.0:7878" {
		t.Fatalf("upgrade install must bind 0.0.0.0:7878, hook saw %q", bound)
	}
	srvAfter, profAfter, projAfter := vaultCounts(t, fin.st)
	if srvAfter != srvBefore || profAfter != profBefore || projAfter != projBefore {
		t.Fatalf("upgrade must not touch vault entities: servers %d→%d profiles %d→%d projects %d→%d",
			srvBefore, srvAfter, profBefore, profAfter, projBefore, projAfter)
	}
	toks, err := fin.st.ListCacheTokens()
	if err != nil || len(toks) != 1 || toks[0].Name != "laptop" {
		t.Fatalf("the only vault write must be the client device code, got %+v %v", toks, err)
	}
}

// TestUpgrade_InstallFailureKeepsStandalone: a failed install completes the
// walkthrough (manual command shown, device code already minted) but does NOT
// persist the role — the machine stays standalone so [u] remains retry-able.
func TestUpgrade_InstallFailureKeepsStandalone(t *testing.T) {
	withServeCertDirs(t)
	orig := serveInstall
	defer func() { serveInstall = orig }()
	serveInstall = func(addr, tlsCert, tlsKey string, out io.Writer) error { return ioErr("access denied") }
	if err := roles.Save(roles.State{Role: roles.RoleStandalone, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}

	a := newTestApp(t)
	// Plan 39: the device-code mint binds to a profile — seed exactly one
	// (auto-bind path; no extra form step in this flow).
	if _, err := a.st.AddProfile("upg-bind"); err != nil {
		t.Fatal(err)
	}
	a.startUpgrade()
	a.upg.serveAddr = "https://192.168.100.235:7878"
	m, _ := a.Update(formDoneMsg{})    // addr → admin notice
	m2, cmd := m.Update(formDoneMsg{}) // admin ack → install
	m3, _ := m2.Update(cmd())          // install failed → probe (non-blocking)
	m4, _ := m3.Update(serveProbeMsg{ok: false, detail: "refused"})
	if v := viewString(m4.(App).overlay); !strings.Contains(v, "serve install --addr 0.0.0.0:7878") {
		t.Fatalf("install failure must show the manual command:\n%s", v)
	}
	m5, _ := m4.Update(formDoneMsg{}) // result → client-name form
	m5.(App).upg.clientName = "laptop"
	m6, cmd2 := m5.Update(formDoneMsg{})
	m7, _ := m6.Update(cmd2())
	m8, _ := m7.Update(formDoneMsg{})  // code → access card
	fin, _ := m8.Update(formDoneMsg{}) // card dismissed → walkthrough ends (role NOT flipped)

	f := fin.(App)
	if f.role != roles.RoleStandalone || f.upg != nil {
		t.Fatalf("failed install must keep the machine standalone and close the segment, got role=%q upg=%v", f.role, f.upg)
	}
	if !strings.Contains(f.footer(), "[u]") {
		t.Fatal("[u] must remain available for retry after a failed install")
	}
	st, _ := roles.Load()
	if st == nil || st.Role != roles.RoleStandalone || !st.SetupComplete {
		t.Fatalf("role.json must stay standalone on failed install, got %+v", st)
	}
}

// TestUpgrade_ErrMsgAbortsSegment: a failed segment action (e.g. device-code
// mint) aborts cleanly back to the standalone console with the error visible.
func TestUpgrade_ErrMsgAbortsSegment(t *testing.T) {
	withServeCertDirs(t)
	a := newTestApp(t)
	a.role = roles.RoleStandalone
	a.startUpgrade()
	m, _ := a.Update(errMsg{err: ioErr("boom")})
	am := m.(App)
	if am.upg != nil || am.overlay != nil {
		t.Fatalf("errMsg must abort the segment, got upg=%v overlay=%v", am.upg, am.overlay)
	}
	if am.err == nil || !strings.Contains(am.err.Error(), "boom") {
		t.Fatalf("the segment error must surface on the console, got %v", am.err)
	}
}

// vaultCounts snapshots the three entity lists the upgrade must not touch.
func vaultCounts(t *testing.T, st *store.Store) (srv, prof, proj int) {
	t.Helper()
	servers, err := st.ListServers()
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := st.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	projects, err := st.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	return len(servers), len(profiles), len(projects)
}

// TestUpgrade_EscCancelsSegment (fix round F1): Esc on a FORM step must cancel
// the whole segment. The addr select pre-commits its default and the name form
// prefills the hostname, so the old empty-answer detection could never fire and
// a bare formDoneMsg ADVANCED the machine (first screen → admin notice →
// install; name form → a real device code minted). Drives the REAL path: the
// Esc KeyPressMsg goes through App.Update → formOverlay, whose cmd carries
// formDoneMsg{aborted:true} back into App.Update.
func TestUpgrade_EscCancelsSegment(t *testing.T) {
	withServeCertDirs(t)
	orig := serveInstall
	defer func() { serveInstall = orig }()
	called := false
	serveInstall = func(addr, tlsCert, tlsKey string, out io.Writer) error {
		called = true // must stay false in both scenarios below
		return nil
	}
	if err := roles.Save(roles.State{Role: roles.RoleStandalone, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	esc := tea.KeyPressMsg{Code: tea.KeyEsc}

	// Scenario 1: Esc on the addr form (first screen) → segment cancelled,
	// install never attempted.
	a := newTestApp(t)
	if a.role != roles.RoleStandalone {
		t.Fatalf("test premise: standalone role, got %q", a.role)
	}
	m, _ := a.Update(tea.KeyPressMsg{Code: 'u', Text: "u"})
	m2, cmd := m.Update(esc)
	msg, ok := cmd().(formDoneMsg)
	if !ok || !msg.aborted {
		t.Fatalf("formOverlay must convert Esc into formDoneMsg{aborted:true}, got %#v", msg)
	}
	m3, _ := m2.Update(msg)
	s1 := m3.(App)
	if s1.upg != nil || s1.overlay != nil {
		t.Fatalf("Esc on the addr form must cancel the segment, got upg=%v overlay=%v", s1.upg, s1.overlay)
	}
	if !strings.Contains(s1.status, "已取消升级") {
		t.Fatalf("cancel status expected, got %q", s1.status)
	}
	if called {
		t.Fatal("install must never be attempted after an Esc cancel")
	}

	// Scenario 2: Esc on the client-name form (post-install screens) → no
	// device code minted.
	b := newTestApp(t)
	// Plan 39: a profile must exist for the segment to reach the client-name
	// form (the mint binds to it).
	if _, err := b.st.AddProfile("upg-bind"); err != nil {
		t.Fatal(err)
	}
	mb, _ := b.Update(tea.KeyPressMsg{Code: 'u', Text: "u"})
	mb.(App).upg.serveAddr = "https://192.168.100.235:7878" // answer the addr form
	n1, _ := mb.Update(formDoneMsg{})                       // addr → admin notice
	n2, cmd2 := n1.Update(formDoneMsg{})                    // admin ack → install cmd
	n3, _ := n2.Update(cmd2())                              // serveInstalledMsg{nil} → probe
	n4, _ := n3.Update(serveProbeMsg{ok: true, detail: "401 Unauthorized"})
	n5, _ := n4.Update(formDoneMsg{}) // result dismissed → client-name form
	if s := n5.(App).upg.step; s != upgClientName {
		t.Fatalf("test premise: segment at the client-name form, got step=%d", s)
	}
	n6, cmd3 := n5.Update(esc)
	msg3, ok := cmd3().(formDoneMsg)
	if !ok || !msg3.aborted {
		t.Fatalf("Esc on the name form must emit formDoneMsg{aborted:true}, got %#v", msg3)
	}
	n7, _ := n6.Update(msg3)
	s2 := n7.(App)
	if s2.upg != nil || s2.overlay != nil {
		t.Fatalf("Esc on the client-name form must cancel the segment, got upg=%v overlay=%v", s2.upg, s2.overlay)
	}
	toks, err := s2.st.ListCacheTokens()
	if err != nil || len(toks) != 0 {
		t.Fatalf("Esc on the name form must mint NO device code, got %+v (%v)", toks, err)
	}
}

// TestUpgrade_PageKeysSuppressedInFlight (fix round F2): while a segment step
// is in flight (install/probe/deviceIssue — overlay==nil windows), page action
// keys must not open forms: the segment's next msg would clobber them.
func TestUpgrade_PageKeysSuppressedInFlight(t *testing.T) {
	withServeCertDirs(t)
	a := newTestApp(t)
	a.role = roles.RoleStandalone
	a.startUpgrade()
	a.upg.step, a.overlay = upgInstall, nil // simulate the in-flight install window
	m, _ := a.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	am := m.(App)
	if am.overlay != nil {
		t.Fatal("page action keys must be suppressed while the upgrade segment is in flight")
	}
	if am.upg == nil || am.upg.step != upgInstall {
		t.Fatalf("the in-flight segment must be untouched, got upg=%v", am.upg)
	}
}

// TestUpgrade_CompleteKeepsWarnView (final review M-C): upgradeComplete must
// refresh pages through refetchPages, not raw FetchAll — the servers page's
// `!` filter and ⚠-first sort survive the segment end (same contract as
// actionDoneMsg / tokenIssuedMsg), while the minted device code is still
// folded into the 设备码 page. Uses the install-failure completion (no
// roles.Save) so the test stays a pure page-refresh assertion.
func TestUpgrade_CompleteKeepsWarnView(t *testing.T) {
	withServeCertDirs(t)
	a := newTestApp(t)
	a.role = roles.RoleStandalone
	// a COMPLETE server so the filter has something to hide (servers carry an
	// FK to credentials — mint the row first)
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
	sp, _ := a.pages[pageServers].(*serversPage)
	sp.warnOnly = true
	sp.rebuild()
	// the minted device code exists in the store but NOT in the page snapshot
	up, err := a.st.AddProfile("upg-p")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.st.AddCacheToken("laptop", up); err != nil {
		t.Fatal(err)
	}
	a.upg = &upgradeSegment{installErr: ioErr("denied")} // failure path: completes without roles.Save

	m, _ := a.upgradeComplete()
	am := m.(App)
	sp2, _ := am.pages[pageServers].(*serversPage)
	if !sp2.warnOnly {
		t.Fatal("upgradeComplete must keep the ! filter (refetchPages, not raw FetchAll)")
	}
	if rows := sp2.Rows(); len(rows) != 1 || rows[0] != "⚠ gpu" {
		t.Fatalf("filter+sort must survive the segment end: %v", rows)
	}
	cp, _ := am.pages[pageTokens].(*cacheTokensPage)
	found := false
	for _, r := range cp.Rows() {
		if strings.Contains(r, "laptop") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the minted device code must still be folded into the 设备码 page: %v", cp.Rows())
	}
}
