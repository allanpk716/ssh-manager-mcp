package tui

// Plan 30 T3: wizardModel routing — delegation outermost, owned cases in the
// main switch (they beat the overlay target), default branch does target
// selection (w.ov first, else the form via the shared tail). 注记 1/2/3.

import (
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"

	"ssh-manager-mcp/internal/roles"
)

// Dispatch state B — static screen: unknown msgs reach w.ov via the default
// branch; owned msgs (errMsg) do NOT — they run wizard logic even while the
// screen is up (they sit in the main switch, which the default branch cannot
// preempt). (Dispatch state A, stepClient delegation, was retired with the
// client-role wizard flow in Plan 42 批1 T8.)
func TestWizardGateOwnedBeatsStaticScreen(t *testing.T) {
	spy := &spyOverlay{}
	w := wizardModel{step: stepToken, ov: spy}
	m, _ := w.Update(probeMsg{})
	_ = m
	if !spy.spySaw(probeMsg{}) {
		t.Fatal("unknown msg must reach the static screen")
	}
	spy.got = nil
	m, _ = w.Update(errMsg{err: errForTest()})
	if spy.spySaw(errMsg{}) {
		t.Fatal("owned errMsg must fall to the wizard switch, not the screen")
	}
	wm := m.(wizardModel)
	if wm.err == nil {
		t.Fatal("errMsg must be recorded by the wizard switch")
	}
}

func errForTest() error { return errors.New("boom") }

// Loop — a real form completes through the default branch: the standalone
// server loop, driven with REAL keypresses. huh advances fields via its
// unexported nextFieldMsg/nextGroupMsg protocol — those msgs must route back
// to the form (the shared tail) or the wizard is dead in a real terminal with
// every form stuck on its first field. Field order mirrors wizServerLoopForm:
// 名称/Host/SSH用户 required (typed), port keeps its 22 default, credential +
// structured fields are optional (Enter through).
func TestWizardLoopServerFormCompletes(t *testing.T) {
	vd, _ := withRoleDirs(t)
	seedWizardVault(t, vd)
	w := newWizardForTest()
	w.chooseRole(roles.RoleStandalone)
	defer w.closeStore() // one handle for the whole flow (copies share the pointer) — release for TempDir
	if w.step != stepServerAsk || w.form == nil {
		t.Fatalf("setup: standalone wizard must open at stepServerAsk, got step=%d", w.step)
	}

	m := tea.Model(w)
	// stepServerAsk confirm (Enter = 是) → openServerForm; its Init call is
	// what focuses the 名称 field (drain then feeds huh's protocol msgs home).
	m, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = drain(t, m, cmd)
	if wm := m.(wizardModel); wm.step != stepServerForm || wm.form == nil {
		t.Fatalf("server form must be open after the ask step, got step=%d", wm.step)
	}

	// Fill the required fields in form order, then Enter through the rest.
	for _, s := range []string{"box1", "192.0.2.10", "u"} {
		for _, r := range s {
			m, _ = m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
		}
		m, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		m = drain(t, m, cmd)
	}
	for i := 0; i < 20 && m.(wizardModel).step == stepServerForm; i++ {
		m, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		m = drain(t, m, cmd)
	}

	w2 := m.(wizardModel)
	if w2.step != stepServerConfirm {
		t.Fatalf("server form must complete and advance to stepServerConfirm, got step=%d", w2.step)
	}
	servers, err := w2.st.ListServers()
	if err != nil || len(servers) != 1 {
		t.Fatalf("the routed loop must persist the server, got %+v (%v)", servers, err)
	}
	if servers[0].Name != "box1" || servers[0].Host != "192.0.2.10" || servers[0].User != "u" {
		t.Fatalf("persisted server must carry the typed fields: %+v", servers[0])
	}
}
