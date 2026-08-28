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

// TestClientPairCard_Copy (Plan 42 批1 T8): the pair card carries the real
// chosen address, the server fingerprint, the pair command as the唯一入网
// path (with the SAS note), and the documented manual cache-pull fallback.
func TestClientPairCard_Copy(t *testing.T) {
	v := viewString(clientPairCard("https://192.168.100.235:7878", "sha256:"+strings.Repeat("a", 64)))
	for _, want := range []string{
		"https://192.168.100.235:7878", "sha256:",
		"ssh-manager pair", "唯一入网", "SAS",
		"cache pull", "设备码页 [a]",
	} {
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
	for _, want := range []string{"安装失败", "denied", "serve install --addr 0.0.0.0:7878", "未验证", "排查", "入网卡"} {
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

// TestServerWizard_FreshFlowStartsAtServerAsk (Plan 42 批1 T8): a fresh server
// wizard (empty vault) goes straight to the shared server-entry loop — the
// 客户端机器名 question is retired (pair names the device at enroll).
func TestServerWizard_FreshFlowStartsAtServerAsk(t *testing.T) {
	withServeCertDirs(t)
	w := newWizardForTest()
	w.chooseRole(roles.RoleServer)
	if w.step != stepServerAsk || w.form == nil {
		t.Fatalf("fresh server flow must start at stepServerAsk with a form, got step=%d", w.step)
	}
	// submitting the skip gate routes into the profile step
	w.data.more = false
	m, _ := w.stepFormDone()
	wm, ok := m.(wizardModel)
	if !ok || wm.step != stepProfileGrant {
		t.Fatalf("skip submit must reach stepProfileGrant, got %+v", m)
	}
	wm.closeStore()
}

// TestServerWizard_ProfileDefaultIsHostname (Plan 42 批1 T8): with the client
// name step retired, the profile-grant form's prefilled default is the machine
// hostname (same as standalone).
func TestServerWizard_ProfileDefaultIsHostname(t *testing.T) {
	withServeCertDirs(t)
	w := newWizardForTest()
	w.chooseRole(roles.RoleServer)
	w.askFirstServer()
	w.data.more = false
	m, _ := w.stepFormDone() // skip → enterProfileGrant
	wm, ok := m.(wizardModel)
	if !ok || wm.step != stepProfileGrant {
		t.Fatalf("skip must land on stepProfileGrant, got %+v", m)
	}
	if want := defaultHostName(); wm.data.profileName != want {
		t.Fatalf("server profile default must be the hostname %q, got %q", want, wm.data.profileName)
	}
	wm.closeStore()
}

// TestServerWizard_TokenScreenUsage: the server role's project-token screen
// shows the token, the manual-path usage, AND the pair-era note that paired
// devices get their own token at approval (no 密钥 1/2 numbering — the wizard
// mints no second secret anymore).
func TestServerWizard_TokenScreenUsage(t *testing.T) {
	withServeCertDirs(t)
	w := newWizardForTest()
	w.role, w.step = roles.RoleServer, stepProject
	m, _ := w.Update(tokenIssuedMsg{title: "x", token: "tok-X"})
	wm := m.(wizardModel)
	if wm.ov == nil || wm.step != stepToken {
		t.Fatalf("tokenIssuedMsg must open the token overlay, got step=%d", wm.step)
	}
	v := viewString(wm.ov)
	for _, want := range []string{"tok-X", "client 机 .mcp.json", "ssh-manager pair"} {
		if !strings.Contains(v, want) {
			t.Fatalf("server token screen missing %q in:\n%s", want, v)
		}
	}
	if strings.Contains(v, "密钥 1/2") || strings.Contains(v, "密钥 2/2") {
		t.Fatalf("the two-secret numbering is retired (no device code mint):\n%s", v)
	}
	// Dismissing the token screen routes straight into the serve segment.
	m2, cmd := wm.Update(formDoneMsg{})
	wm2 := m2.(wizardModel)
	if wm2.step != stepAddr || wm2.form == nil || cmd == nil {
		t.Fatalf("token dismiss must reach stepAddr with a form, got step=%d form=%v cmd=%v", wm2.step, wm2.form, cmd)
	}
	wm2.closeStore()
}

// TestServerWizard_ResumeHeuristics (Plan 42 批1 T8 shape): the wizard itself
// mints profile+project only — profile+project done ⇒ straight to the serve
// segment (addr form, fp recovered from the cert) REGARDLESS of cache-token
// count (the device-code tier of the old heuristic is retired). A profile-only
// vault resumes at the project step. No resume path may create entities by
// itself.
func TestServerWizard_ResumeHeuristics(t *testing.T) {
	vd := withServeCertDirs(t)
	seedWizardVault(t, vd)
	st := openVault(t)
	pid, _ := st.AddProfile("p1")
	if _, _, err := st.AddProject("j1", pid); err != nil {
		t.Fatal(err)
	}
	// The old heuristic keyed the serve-segment jump on token count — now a
	// token makes no difference. Seed one anyway (a legacy pre-pair code).
	if _, _, err := st.AddCacheToken("laptop", pid); err != nil {
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

	// Same landing WITHOUT any cache token (the retired tier). All handles are
	// closed BEFORE the next seedWizardVault — an open sqlite handle pins the
	// db file and the re-seed would silently mutate the OLD database.
	seedWizardVault(t, vd)
	st = openVault(t)
	pid, _ = st.AddProfile("p2")
	if _, _, err := st.AddProject("j2", pid); err != nil {
		t.Fatal(err)
	}
	st.Close()
	w2 := newWizardForRole(roles.Launch{Kind: roles.LaunchBroker, Role: roles.RoleServer, ResumeSetup: true})
	if w2.step != stepAddr {
		t.Fatalf("profile+project (no token) resume must land on stepAddr too, got step=%d", w2.step)
	}
	st2 := openVault(t)
	profiles, _ := st2.ListProfiles()
	projects, _ := st2.ListProjects()
	if len(profiles) != 1 || len(projects) != 1 {
		t.Fatalf("resume must not create entities: %d profiles %d projects", len(profiles), len(projects))
	}
	st2.Close()
	w2.closeStore()

	// profile-only → the project step.
	seedWizardVault(t, vd)
	st = openVault(t)
	pid3, err := st.AddProfile("p3")
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	w3 := newWizardForRole(roles.Launch{Kind: roles.LaunchBroker, Role: roles.RoleServer, ResumeSetup: true})
	if w3.step != stepProject {
		t.Fatalf("profile-only resume must land on stepProject, got step=%d", w3.step)
	}
	if w3.data.profileID != pid3 {
		t.Fatalf("profile-only resume must preload the existing profile id, got %q want %q", w3.data.profileID, pid3)
	}
	w3.closeStore()
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
	if wm4.step != stepPairCard || wm4.ov == nil {
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

// TestServerWizard_ResumeMultiProfileOpensPicker (Plan 39, code-review #2):
// when a resume path needs a binding and SEVERAL profiles exist, the resume
// must NOT silently bind the alphabetically-first — a binding picker opens,
// and the chosen profile is what the resumed flow (the wizard's project mint)
// binds to. Plan 42 批1 T8: the picker lives only in the 0-projects branch —
// once the project exists, the serve segment needs no binding (pair approval
// picks the pair project's profile at approval time).
func TestServerWizard_ResumeMultiProfileOpensPicker(t *testing.T) {
	vd := withServeCertDirs(t)
	seedWizardVault(t, vd)
	st := openVault(t)
	defer st.Close()
	if _, err := st.AddProfile("alpha"); err != nil {
		t.Fatal(err)
	}
	bID, err := st.AddProfile("beta")
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	w := newWizardForTest()
	w.role = roles.RoleServer
	w.st = openVault(t)
	defer w.st.Close()
	w.resumeServerFlow()
	if w.step != stepBindProfile || w.form == nil {
		t.Fatalf("multi-profile 0-project resume must open the binding picker, got step=%v form=%v", w.step, w.form)
	}
	// (huh's Select pre-commits the first option into the bound value at
	// construction — same behavior as the addr picker. That is a VISIBLE
	// pre-selection the owner confirms/changes, not the silent-routing bug
	// this fix targets: the form being on screen IS the fix.)
	// The owner picks beta (NOT the alphabetical-first alpha)…
	w.data.profileID = bID
	// …and completing the picker re-routes to the original target: the
	// project step, bound to the chosen profile.
	m, _ := w.stepFormDone()
	wm, ok := m.(wizardModel)
	if !ok || wm.step != stepProject || wm.form == nil {
		t.Fatalf("picker completion must resume at stepProject, got step=%v", wm.step)
	}
	if wm.data.profileID != bID {
		t.Fatalf("chosen binding must survive the re-route, got %q", wm.data.profileID)
	}
}

// TestServerWizard_ResumeSoleProfileStillAutoBinds: exactly one profile keeps
// the silent auto-bind (no picker) — the common single-profile fleet shape;
// the resume lands on the project step bound to that sole profile. With the
// project already present the resume needs no binding at all → straight to
// the serve segment.
func TestServerWizard_ResumeSoleProfileStillAutoBinds(t *testing.T) {
	vd := withServeCertDirs(t)
	seedWizardVault(t, vd)
	st := openVault(t)
	aID, err := st.AddProfile("alpha")
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	w := newWizardForTest()
	w.role = roles.RoleServer
	w.st = openVault(t)
	defer w.st.Close()
	w.resumeServerFlow()
	if w.step != stepProject {
		t.Fatalf("sole-profile 0-project resume must auto-bind into stepProject, got step=%v", w.step)
	}
	if w.data.profileID != aID {
		t.Fatalf("sole-profile resume must auto-bind that profile, got %q", w.data.profileID)
	}

	// project done → serve segment, no binding question.
	if _, _, err := w.st.AddProject("proj-x", aID); err != nil {
		t.Fatal(err)
	}
	w.resumeServerFlow()
	if w.step != stepAddr {
		t.Fatalf("all-done resume must land on stepAddr, got step=%v", w.step)
	}
}
