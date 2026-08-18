package tui

// Plan 30 T1: the App gate — while an overlay is open, everything except the
// App's owned msgs reaches the overlay, and the overlay's cmd goes back to the
// (simulated) runtime. These are the regression net for the class "tests all
// green, real terminal dead".

import (
	"testing"

	tea "charm.land/bubbletea/v2"
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
		serveInstalledMsg{}, serveProbeMsg{}, tokenIssuedMsg{},
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
