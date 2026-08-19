package tui

// Plan 30 T1: the App gate — while an overlay is open, everything except the
// App's owned msgs reaches the overlay, and the overlay's cmd goes back to the
// (simulated) runtime. These are the regression net for the class "tests all
// green, real terminal dead".

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"ssh-manager-mcp/internal/models"
)

func TestGateForwardsUnknownAndHandsCmdBack(t *testing.T) {
	a, _ := seedStoreApp(t)
	spy := &spyOverlay{cmd: func() tea.Msg { return probeMsg{} }}
	a.overlay = spy
	m, cmd := a.Update(probeMsg{})
	if !m.(App).overlay.(*spyOverlay).spySaw(probeMsg{}) {
		t.Fatal("unknown msg must reach the overlay")
	}
	if cmd == nil {
		t.Fatal("gate must hand the overlay's cmd back to the runtime")
	}
	if _, ok := cmd().(probeMsg); !ok {
		t.Fatal("handed-back cmd must be the spy's sentinel")
	}
}

func TestGateWritesOverlayBack(t *testing.T) {
	a, _ := seedStoreApp(t)
	replacement := &spyOverlay{}
	spy := &spyOverlay{swap: replacement}
	a.overlay = spy
	m, _ := a.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if got := m.(App).overlay; got != tea.Model(replacement) {
		t.Fatalf("gate must write the overlay pointer back, got %T", got)
	}
}

func TestGateOwnedFallsThrough(t *testing.T) {
	a, _ := seedStoreApp(t)
	spy := &spyOverlay{}
	a.overlay = spy
	// every owned type must NOT reach the overlay and must run App logic
	for _, owned := range []tea.Msg{
		errMsg{}, actionDoneMsg{}, formDoneMsg{},
		serveInstalledMsg{}, serveProbeMsg{}, deviceCodeIssuedMsg{},
		tokenIssuedMsg{},
	} {
		a.overlay = spy
		m, _ := a.Update(owned)
		app := m.(App)
		if spy.spySaw(owned) {
			t.Fatalf("owned %T must fall through to the App switch", owned)
		}
		_ = app
	}
	// formDoneMsg specifically closes the overlay
	a.overlay = spy
	m, _ := a.Update(formDoneMsg{})
	if m.(App).overlay != nil {
		t.Fatal("formDoneMsg must close the overlay")
	}
}

func TestGateWindowSizeRecordsAndForwards(t *testing.T) {
	a, _ := seedStoreApp(t)
	spy := &spyOverlay{}
	a.overlay = spy
	m, _ := a.Update(tea.WindowSizeMsg{Width: 60, Height: 30})
	app := m.(App)
	if !spy.spySaw(tea.WindowSizeMsg{}) {
		t.Fatal("resize must reach the overlay")
	}
	if app.width != 60 || app.height != 30 {
		t.Fatalf("resize must be recorded, got %dx%d", app.width, app.height)
	}
}

func TestAppLoopProfileFormCompletes(t *testing.T) {
	a, st := seedStoreApp(t)
	m, _ := a.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyTab})        // servers → profiles
	m, cmd := m.Update(tea.KeyPressMsg{Code: 'a', Text: "a"}) // 新增 Profile
	if cmd == nil {
		t.Fatal("'a' must open the form and return its Init cmd")
	}
	m = drain(t, m, cmd)
	for _, r := range "gp" {
		m, _ = m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	m, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = drain(t, m, cmd)
	if m.(App).overlay != nil {
		t.Fatal("single-field form must complete and close the overlay")
	}
	profiles, err := st.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 1 || profiles[0].Name != "gp" {
		t.Fatalf("profile gp must be persisted, got %+v", profiles)
	}
}

// The 3-group server form must complete through the routed loop. Field order
// (forms.go newServerForm): 名称/Host/SSH用户/端口 | 密码/私钥路径/密钥口令/
// sudo密码 | 硬件/位置/… (structuredFields) — all optional after the first 3.
// 端口 field pre-或空值: type "22" (valid in both cases).
func TestAppLoopServerFormCompletes(t *testing.T) {
	a, st := seedStoreApp(t)
	m, _ := a.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m, cmd := m.Update(tea.KeyPressMsg{Code: 'a', Text: "a"}) // servers 新增
	if cmd == nil {
		t.Fatal("'a' must open the server form")
	}
	m = drain(t, m, cmd)
	typeWord := func(word string) {
		for _, r := range word {
			m, _ = m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
		}
		var c tea.Cmd
		m, c = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		m = drain(t, m, c)
	}
	typeWord("web")      // 名称
	typeWord("10.0.0.9") // Host
	typeWord("ops")      // SSH 用户
	typeWord("22")       // 端口(pre-fill "22" 或空,两种输入皆合法)
	// 其余字段全可选:Enter-only 推进直至表单完成(有界)
	for i := 0; i < 30 && m.(App).overlay != nil; i++ {
		var c tea.Cmd
		m, c = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		m = drain(t, m, c)
	}
	if m.(App).overlay != nil {
		if ov, ok := m.(App).overlay.(*formOverlay); ok {
			t.Fatalf("3-group form must complete within the Enter bound\nform state: %+v\nview:\n%s",
				ov.form.State, ov.View().Content)
		}
		t.Fatalf("3-group form must complete within the Enter bound, overlay still %T", m.(App).overlay)
	}
	servers, err := st.ListServers()
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 2 {
		t.Fatalf("seeded gpu + new web expected, got %d", len(servers))
	}
}

// v0.8.7: Projects 页 `x` 只删已吊销的行——active 行拒绝(状态行提示,
// 不开表单);revoked 行走 Confirm(y)→ 落库删除。
func TestAppProjectsXDeletesRevokedOnly(t *testing.T) {
	st := newStore(t)
	pid, _ := st.AddProfile("dev")
	projID, _, err := st.AddProject("agent", pid)
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewBrokerApp(st)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := a.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyTab}) // → profiles
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyTab}) // → projects

	// active 行:x → 不开表单,状态行提示先吊销
	// (the status hint — not just any non-empty status — proves the projects
	// 'x' branch actually ran; the outer key gate must list "x")
	m, cmd := m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if cmd != nil || m.(App).overlay != nil {
		t.Fatal("'x' on an ACTIVE project must not open a form")
	}
	if !strings.Contains(m.(App).status, "吊销") {
		t.Fatalf("active 'x' must leave the revoke-first hint in the status line, got %q", m.(App).status)
	}

	// revoke it, refetch (actionDoneMsg), then x → Confirm(y) → deleted
	if err := st.SetProjectStatus(projID, models.ProjectRevoked); err != nil {
		t.Fatal(err)
	}
	m, _ = m.Update(actionDoneMsg{desc: "refetch"}) // refetchPages keeps the page list current
	m, cmd = m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if cmd == nil {
		t.Fatalf("'x' on a REVOKED project must open the confirm overlay, status=%q", m.(App).status)
	}
	m = drain(t, m, cmd)
	var c tea.Cmd
	m, c = m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"}) // Confirm 肯定单键
	m = drain(t, m, c)
	if p, _ := st.GetProjectByName("agent"); p != nil {
		t.Fatalf("revoked project must be deleted, got %+v", p)
	}
	if m.(App).overlay != nil {
		t.Fatal("overlay must close after the delete")
	}
}
