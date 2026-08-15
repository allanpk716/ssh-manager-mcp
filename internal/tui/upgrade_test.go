package tui

import (
	"io"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

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
