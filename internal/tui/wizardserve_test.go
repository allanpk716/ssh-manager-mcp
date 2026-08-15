package tui

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/roles"
)

// withServeCertDirs extends withRoleDirs with pinned serve-cert paths so the
// wizard's LoadOrCreateServeCert calls never touch C:\ProgramData.
func withServeCertDirs(t *testing.T) (vaultDir string) {
	t.Helper()
	vd, _ := withRoleDirs(t)
	t.Setenv("SSHMGR_SERVE_CERT", filepath.Join(vd, "serve-cert.pem"))
	t.Setenv("SSHMGR_SERVE_KEY", filepath.Join(vd, "serve-key.pem"))
	t.Setenv("SSHMGR_SERVE_MARKER", filepath.Join(vd, "serve-cert.init"))
	return vd
}

// TestAccessCard_Copy (brief T4): the client access card must carry the real
// chosen address, the server fingerprint, both secret destinations (.mcp.json /
// cache pull), and the 去向 table framing.
func TestAccessCard_Copy(t *testing.T) {
	v := viewString(accessCard("https://192.168.100.235:7878", "sha256:"+strings.Repeat("a", 64)))
	for _, want := range []string{"https://192.168.100.235:7878", "sha256:", ".mcp.json", "cache pull", "去向"} {
		if !strings.Contains(v, want) {
			t.Fatalf("missing %q in:\n%s", want, v)
		}
	}
}

// TestProbeServe (brief T4): a live TLS server answering with ANY HTTP status
// counts as ok (we verify "listening + speaking TLS", not auth); a dead port
// must fail.
func TestProbeServe(t *testing.T) {
	// httptest TLS server standing in for serve
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "x", http.StatusUnauthorized)
	}))
	defer srv.Close()
	msg := probeServe(srv.URL)()
	p, ok := msg.(serveProbeMsg)
	if !ok {
		t.Fatalf("want serveProbeMsg, got %#v", msg)
	}
	if !p.ok {
		t.Fatalf("probe should pass on live TLS: %+v", p)
	}
	if msg2 := probeServe("https://127.0.0.1:1/x")(); msg2.(serveProbeMsg).ok {
		t.Fatal("dead port must fail")
	}
}

// TestLanAddrOptions_Values: option value AND display are the ready-to-use
// "https://<ip>:7878" string (value=display discipline — the access card shows
// the exact value the select carried); IPv6 and loopback are filtered; the
// default is the first option.
func TestLanAddrOptions_Values(t *testing.T) {
	opts, def := lanAddrOptions([]net.IP{
		net.ParseIP("fe80::1"),         // IPv6 → filtered
		net.ParseIP("127.0.0.1"),       // loopback → filtered
		net.ParseIP("192.168.100.235"), // kept (first → default)
		net.ParseIP("10.0.0.5"),        // kept
	})
	if len(opts) != 2 {
		t.Fatalf("want 2 options after filtering, got %d: %+v", len(opts), opts)
	}
	for i, want := range []string{"https://192.168.100.235:7878", "https://10.0.0.5:7878"} {
		if opts[i].Value != want || opts[i].Key != want {
			t.Fatalf("option %d must have value=key=%q, got key=%q value=%q", i, want, opts[i].Key, opts[i].Value)
		}
	}
	if def != "https://192.168.100.235:7878" {
		t.Fatalf("default must be first option's value, got %q", def)
	}
	// empty → no options, no default (manual-input fallback)
	if opts, def := lanAddrOptions(nil); len(opts) != 0 || def != "" {
		t.Fatalf("no IPv4s → no options/default, got %d/%q", len(opts), def)
	}
}

// TestWizAddrForm_PresetsDefault / manual fallback: with IPv4s the chosen
// pointer is pre-set to the first option; with none, the form degrades to a
// manual input without touching chosen.
func TestWizAddrForm(t *testing.T) {
	var chosen string
	f := wizAddrForm([]net.IP{net.ParseIP("192.168.1.9")}, &chosen)
	if f == nil {
		t.Fatal("form must build")
	}
	if chosen != "https://192.168.1.9:7878" {
		t.Fatalf("chosen must be preset to first option, got %q", chosen)
	}
	chosen2 := ""
	f2 := wizAddrForm(nil, &chosen2)
	if f2 == nil {
		t.Fatal("manual fallback form must build")
	}
	if chosen2 != "" {
		t.Fatalf("manual fallback must not preset chosen, got %q", chosen2)
	}
}

// TestInstallServeStep_BindsWildcard: the wizard ALWAYS registers 0.0.0.0:7878
// (plan constraint: 严禁 127.0.0.1 默认 on the wizard path); the chosen addr is
// display-only. Errors ride serveInstalledMsg without panicking; a missing hook
// surfaces a clear error instead of a silent skip.
func TestInstallServeStep_BindsWildcard(t *testing.T) {
	orig := serveInstall
	defer func() { serveInstall = orig }()

	var gotAddr string
	serveInstall = func(addr, tlsCert, tlsKey string, out io.Writer) error {
		gotAddr = addr
		return nil
	}
	msg := installServeStep("https://192.168.1.9:7878")()
	m, ok := msg.(serveInstalledMsg)
	if !ok || m.err != nil {
		t.Fatalf("want clean serveInstalledMsg, got %#v", msg)
	}
	if gotAddr != "0.0.0.0:7878" {
		t.Fatalf("wizard must bind 0.0.0.0:7878, hook saw %q", gotAddr)
	}

	serveInstall = nil
	m2, ok := installServeStep("https://x")().(serveInstalledMsg)
	if !ok || m2.err == nil || !strings.Contains(m2.err.Error(), "未接线") {
		t.Fatalf("nil hook must produce a clear error, got %#v", m2)
	}
}

// TestServeResultScreen_Banners: install failure shows the raw elevated command
// and stays non-blocking; probe pass = 已就绪, probe fail = 未验证 + 排查提示.
func TestServeResultScreen_Banners(t *testing.T) {
	v := viewString(serveResultScreen(ioErr("denied"), serveProbeMsg{ok: false, detail: "timeout"}))
	for _, want := range []string{"安装失败", "denied", "serve install --addr 0.0.0.0:7878", "未验证", "排查", "接入卡"} {
		if !strings.Contains(v, want) {
			t.Fatalf("failure screen missing %q in:\n%s", want, v)
		}
	}
	v2 := viewString(serveResultScreen(nil, serveProbeMsg{ok: true, detail: "401 Unauthorized"}))
	for _, want := range []string{"已安装", "已就绪", "401 Unauthorized"} {
		if !strings.Contains(v2, want) {
			t.Fatalf("ok screen missing %q in:\n%s", want, v2)
		}
	}
}

// TestServerWizard_FreshFlowStartsAtClientName: a fresh server wizard (empty
// vault) asks the client machine name first — its answer becomes the profile
// default — then routes into the shared server-entry loop.
func TestServerWizard_FreshFlowStartsAtClientName(t *testing.T) {
	withServeCertDirs(t)
	w := newWizardForTest()
	w.chooseRole(roles.RoleServer)
	if w.step != stepClientName || w.form == nil {
		t.Fatalf("fresh server flow must start at stepClientName with a form, got step=%d", w.step)
	}
	// submitting the name routes into the shared server loop (skip gate)
	w.data.clientName = "laptop"
	m, _ := w.stepFormDone()
	if wm, ok := m.(wizardModel); !ok || wm.step != stepServerAsk {
		t.Fatalf("client name submit must reach stepServerAsk, got %+v", m)
	}
	w.closeStore()
}

// TestServerWizard_ProfileDefaultIsClientName: the profile-grant form's
// prefilled default for the server role is the CLIENT name (spec §2.4 ④).
func TestServerWizard_ProfileDefaultIsClientName(t *testing.T) {
	withServeCertDirs(t)
	w := newWizardForTest()
	w.chooseRole(roles.RoleServer)
	w.data.clientName = "laptop"
	w.askFirstServer()
	w.data.more = false
	m, _ := w.stepFormDone() // skip → enterProfileGrant
	wm, ok := m.(wizardModel)
	if !ok || wm.step != stepProfileGrant {
		t.Fatalf("skip must land on stepProfileGrant, got %+v", m)
	}
	if wm.data.profileName != "laptop" {
		t.Fatalf("server profile default must be client name, got %q", wm.data.profileName)
	}
	wm.closeStore()
}

// TestServerWizard_TokenScreenClientUsage: the server role's project-token
// screen says the token goes to the CLIENT machine's .mcp.json (usage label).
func TestServerWizard_TokenScreenClientUsage(t *testing.T) {
	withServeCertDirs(t)
	w := newWizardForTest()
	w.role, w.step = roles.RoleServer, stepProject
	m, _ := w.Update(tokenIssuedMsg{title: "x", token: "tok-X"})
	wm := m.(wizardModel)
	if wm.ov == nil || wm.step != stepToken {
		t.Fatalf("tokenIssuedMsg must open the token overlay, got step=%d", wm.step)
	}
	v := viewString(wm.ov)
	for _, want := range []string{"tok-X", "client 机 .mcp.json", "密钥 1/2"} {
		if !strings.Contains(v, want) {
			t.Fatalf("server token screen missing %q in:\n%s", want, v)
		}
	}
}

// TestServerWizard_DeviceIssueAndScreens: device-code issuance (cert first,
// then AddCacheToken named after the client) lands on the one-time screen with
// the merged cache-pull usage line; dismissing routes to the addr form.
func TestServerWizard_DeviceIssueAndScreens(t *testing.T) {
	vd := withServeCertDirs(t)
	seedWizardVault(t, vd)
	w := newWizardForTest()
	w.role = roles.RoleServer
	w.st = openVault(t)
	w.data.clientName = "laptop"
	w.step = stepDeviceIssue
	msg := w.issueDeviceCode()()
	dc, ok := msg.(deviceCodeIssuedMsg)
	if !ok {
		t.Fatalf("want deviceCodeIssuedMsg, got %#v", msg)
	}
	if !strings.HasPrefix(dc.fingerprint, "sha256:") {
		t.Fatalf("fingerprint must be a pin, got %q", dc.fingerprint)
	}
	m, _ := w.Update(msg)
	wm := m.(wizardModel)
	if wm.step != stepDeviceToken || wm.ov == nil {
		t.Fatalf("device code must open one-time screen, got step=%d", wm.step)
	}
	v := viewString(wm.ov)
	for _, want := range []string{dc.code, "cache pull --token '" + dc.code + ":" + dc.fingerprint + "'", "设备码页 [a] 重发", "密钥 2/2"} {
		if !strings.Contains(v, want) {
			t.Fatalf("device code screen missing %q in:\n%s", want, v)
		}
	}
	// any key → addr form (LAN select; deviceFp retained for the access card)
	m2, _ := wm.Update(formDoneMsg{})
	wm2 := m2.(wizardModel)
	if wm2.step != stepAddr || wm2.form == nil {
		t.Fatalf("device screen dismiss must reach stepAddr with a form, got step=%d", wm2.step)
	}
	if wm2.data.deviceFp != dc.fingerprint {
		t.Fatalf("fingerprint must be retained for the access card, got %q", wm2.data.deviceFp)
	}
	// the code was persisted under the client name
	st2 := openVault(t)
	defer st2.Close()
	toks, err := st2.ListCacheTokens()
	if err != nil || len(toks) != 1 || toks[0].Name != "laptop" {
		t.Fatalf("device code must be persisted as client name: %+v %v", toks, err)
	}
	wm2.closeStore()
}

// TestServerWizard_ResumeHeuristics: mirrors T3's — profile+project+device
// code all exist → resume straight at the serve segment (addr form, fp
// recovered from the cert); profile+project but no device code → back at the
// client-name step and the next submit issues the missing code only.
func TestServerWizard_ResumeHeuristics(t *testing.T) {
	vd := withServeCertDirs(t)
	seedWizardVault(t, vd)
	st := openVault(t)
	pid, _ := st.AddProfile("p1")
	if _, _, err := st.AddProject("j1", pid); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AddCacheToken("laptop"); err != nil {
		t.Fatal(err)
	}
	st.Close()

	w := newWizardForRole(roles.Launch{Kind: roles.LaunchBroker, Role: roles.RoleServer, ResumeSetup: true})
	if w.step != stepAddr {
		t.Fatalf("all-done resume must land on stepAddr, got step=%d", w.step)
	}
	if !strings.HasPrefix(w.data.deviceFp, "sha256:") {
		t.Fatalf("resume must recover the cert fingerprint, got %q", w.data.deviceFp)
	}
	w.closeStore()

	// half-done: no cache token → client-name step, submit issues ONLY the code
	seedWizardVault(t, vd)
	st = openVault(t)
	pid, _ = st.AddProfile("p2")
	st.AddProject("j2", pid)
	st.Close()
	w2 := newWizardForRole(roles.Launch{Kind: roles.LaunchBroker, Role: roles.RoleServer, ResumeSetup: true})
	if w2.step != stepClientName {
		t.Fatalf("profile+project (no code) resume must land on stepClientName, got step=%d", w2.step)
	}
	w2.data.clientName = "laptop"
	m, _ := w2.stepFormDone()
	wm := m.(wizardModel)
	if wm.step != stepDeviceIssue {
		t.Fatalf("client-name submit must issue the device code, got step=%d", wm.step)
	}
	st2 := openVault(t)
	defer st2.Close()
	profiles, _ := st2.ListProfiles()
	projects, _ := st2.ListProjects()
	if len(profiles) != 1 || len(projects) != 1 {
		t.Fatalf("resume must not create entities: %d profiles %d projects", len(profiles), len(projects))
	}
	wm.closeStore()
}

// TestServerWizard_ServeSegmentNonBlocking: install failure does NOT block —
// the flow still probes and shows the result screen with the manual command;
// dismissing reaches the access card with the REAL chosen address + fp, and
// any key there finishes setup for the server role.
func TestServerWizard_ServeSegmentNonBlocking(t *testing.T) {
	withServeCertDirs(t)
	orig := serveInstall
	defer func() { serveInstall = orig }()
	serveInstall = func(addr, c, k string, out io.Writer) error { return ioErr("access denied") }

	w := newWizardForTest()
	w.role, w.step = roles.RoleServer, stepServeAdmin
	w.ov = serveAdminNotice()
	w.data.serveAddr = "https://192.168.100.235:7878"
	w.data.deviceFp = "sha256:" + strings.Repeat("b", 64)

	m, cmd := w.Update(formDoneMsg{}) // admin notice → install
	wm := m.(wizardModel)
	if wm.step != stepServeInstall || cmd == nil {
		t.Fatalf("admin notice must trigger installServeStep, got step=%d", wm.step)
	}
	m2, _ := wm.Update(cmd()) // serveInstalledMsg{err} → probe, non-blocking
	wm2 := m2.(wizardModel)
	if wm2.step != stepServeProbe {
		t.Fatalf("install failure must still advance to probe, got step=%d", wm2.step)
	}
	m3, _ := wm2.Update(serveProbeMsg{ok: false, detail: "refused"})
	wm3 := m3.(wizardModel)
	if wm3.step != stepServeResult || wm3.ov == nil {
		t.Fatalf("probe result must open the result screen, got step=%d", wm3.step)
	}
	v := viewString(wm3.ov)
	for _, want := range []string{"安装失败", "serve install --addr 0.0.0.0:7878", "未验证"} {
		if !strings.Contains(v, want) {
			t.Fatalf("result screen missing %q in:\n%s", want, v)
		}
	}
	m4, _ := wm3.Update(formDoneMsg{})
	wm4 := m4.(wizardModel)
	if wm4.step != stepAccessCard || wm4.ov == nil {
		t.Fatalf("result dismiss must open the access card, got step=%d", wm4.step)
	}
	if v4 := viewString(wm4.ov); !strings.Contains(v4, "https://192.168.100.235:7878") {
		t.Fatalf("access card must show the real chosen addr:\n%s", v4)
	}
	m5, cmd5 := wm4.Update(formDoneMsg{})
	if cmd5 == nil {
		t.Fatal("access card dismiss must run wizFinish")
	}
	final, _ := m5.Update(cmd5())
	if wf, ok := final.(wizardModel); !ok || !wf.done || wf.next != "broker" {
		t.Fatalf("server flow must finish into the broker handoff, got %+v", final)
	}
}

// ioErr is a tiny error helper to keep test literals short.
func ioErr(s string) error { return &strErr{s} }

type strErr struct{ s string }

func (e *strErr) Error() string { return e.s }
