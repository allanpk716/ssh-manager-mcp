package tui

// Plan 30 T4: the clientModel gate — same shape as the App gate. Plan 42 批1
// T8 retired the connect-form overlay; the overlays are now the instance
// picker and (Plan 45 T3) the pairing wizard. The wizard's five INTERNAL async
// messages ride the gate's default branch (forwarded); only its two terminal
// messages + the picker's pair request are client-owned.

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

func newClientModelForGate(t *testing.T) clientModel {
	// newClientModel initializes the panelList — the 'c' keypress path runs
	// listUpdate before the action switch, so the list must be constructed.
	m := newClientModel()
	m.width, m.height = 80, 24
	return m
}

func TestClientGateForwardsUnknownAndHandsCmdBack(t *testing.T) {
	m := newClientModelForGate(t)
	spy := &spyOverlay{cmd: func() tea.Msg { return probeMsg{} }}
	m.overlay = spy
	m2, cmd := m.Update(probeMsg{})
	if !m2.(clientModel).overlay.(*spyOverlay).spySaw(probeMsg{}) {
		t.Fatal("unknown msg must reach the overlay")
	}
	if cmd == nil {
		t.Fatal("gate must hand the overlay's cmd back to the runtime")
	}
	if _, ok := cmd().(probeMsg); !ok {
		t.Fatal("handed-back cmd must be the spy's sentinel probeMsg")
	}
}

// TestClientGateOwnedFallsThrough: every client-owned type must run the
// model's own case even while an overlay is open — the gate may not starve
// the model of its messages.
func TestClientGateOwnedFallsThrough(t *testing.T) {
	m := newClientModelForGate(t)
	spy := &spyOverlay{}
	for _, owned := range []tea.Msg{
		dataReadyMsg{}, syncDoneMsg{},
		clientStatusMsg(""), errMsg{}, formDoneMsg{},
	} {
		m.overlay = spy
		nm, _ := m.Update(owned)
		if _, ok := nm.(clientModel); !ok {
			t.Fatalf("Update must return clientModel, got %T", nm)
		}
		if spy.spySaw(owned) {
			t.Fatalf("owned %T must fall through to clientModel's own case", owned)
		}
	}
}

// TestClientGateFormDoneClosesOverlay: formDoneMsg is owned AND closing — the
// model's own case (not the gate) must nil the overlay.
func TestClientGateFormDoneClosesOverlay(t *testing.T) {
	m := newClientModelForGate(t)
	m.overlay = &spyOverlay{}
	m2, _ := m.Update(formDoneMsg{})
	if m2.(clientModel).overlay != nil {
		t.Fatal("formDoneMsg closes the overlay (clientModel's own case)")
	}
}

func TestClientGateWindowSizeRecordsAndForwards(t *testing.T) {
	m := newClientModelForGate(t)
	spy := &spyOverlay{}
	m.overlay = spy
	m2, _ := m.Update(tea.WindowSizeMsg{Width: 60, Height: 30})
	cm := m2.(clientModel)
	if !spy.spySaw(tea.WindowSizeMsg{}) {
		t.Fatal("resize must reach the overlay")
	}
	if cm.width != 60 || cm.height != 30 {
		t.Fatalf("resize must be recorded, got %dx%d", cm.width, cm.height)
	}
}

// TestClientPanel_CKeyStartsPairWizard (Plan 45 T3; REWRITES Plan 42 批1 T8's
// TestClientPanel_CKeyPointsAtPair): the [c] key REALLY opens the pairing
// wizard again — Plan 42 had retired the connect-form and reduced [c] to a
// status-line pointer at `sshmgr pair`; Plan 45 gives the affordance its
// guided path back (pairwizard.go).
func TestClientPanel_CKeyStartsPairWizard(t *testing.T) {
	isolatedConfigDir(t) // clears both single-slot override envs
	m := newClientModelForGate(t)
	m2, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Text: "c"})
	cm := m2.(clientModel)
	if _, ok := cm.overlay.(*pairWizard); !ok {
		t.Fatalf("[c] must open the pairing wizard, got overlay %T", cm.overlay)
	}
	if cmd == nil {
		t.Fatal("[c] must hand the wizard's Init cmd back to the runtime")
	}
}

// TestClientPanel_CKeyRefusedUnderSingleSlotOverride: newPairWizard's own
// single-slot mutual exclusion stays the authority — a direct [c] under an
// override env opens nothing and surfaces the refusal as the panel error.
// (The footer stops advertising [c] in this mode; the guard is defense in
// depth, not the only line.)
func TestClientPanel_CKeyRefusedUnderSingleSlotOverride(t *testing.T) {
	isolatedConfigDir(t)
	t.Setenv("SSHMGR_CACHE_DIR", t.TempDir()) // AFTER the helper's clear: full override
	m := newClientModelForGate(t)
	m2, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Text: "c"})
	cm := m2.(clientModel)
	if cm.overlay != nil {
		t.Fatalf("single-slot override must refuse the wizard, got overlay %T", cm.overlay)
	}
	if cmd != nil {
		t.Fatal("a refused start must not hand back an init cmd")
	}
	if cm.err == nil {
		t.Fatal("the refusal must surface as a panel error")
	}
}

// TestClientGate_RegistersWizardMsgs (Plan 30 checklist): the wizard's two
// terminal messages + the picker's re-pair request are CLIENT-owned types —
// while ANY overlay is open they must fall through to clientModel's own
// switch, never be swallowed by the gate's default branch. The wizard's five
// INTERNAL async messages (discover/enroll/approval/write/tick) stay
// UNREGISTERED on purpose: the default branch forwards them to the overlay.
func TestClientGate_RegistersWizardMsgs(t *testing.T) {
	isolatedConfigDir(t)
	m := newClientModelForGate(t)
	spy := &spyOverlay{}
	for _, owned := range []tea.Msg{
		pairWizardDoneMsg{}, pairWizardClosedMsg{}, instancePickerPairMsg{},
	} {
		m.overlay = spy
		nm, _ := m.Update(owned)
		if _, ok := nm.(clientModel); !ok {
			t.Fatalf("Update must return clientModel, got %T", nm)
		}
		if spy.spySaw(owned) {
			t.Fatalf("owned %T must fall through to clientModel's own case", owned)
		}
	}
}

// TestClientPanel_CKeyEscFullChain: [c] → wizard → Esc → back on the page
// (overlay dropped, slot untouched) — the full escape hatch the brief pins
// ("Esc 全链退回原页"), exercised through the gate's default branch.
func TestClientPanel_CKeyEscFullChain(t *testing.T) {
	isolatedConfigDir(t)
	m := newClientModelForGate(t)
	m.instance = "agentA"
	m2, _ := m.Update(tea.KeyPressMsg{Code: 'c', Text: "c"})
	m = m2.(clientModel)
	if _, ok := m.overlay.(*pairWizard); !ok {
		t.Fatalf("precondition: [c] opens the wizard, got %T", m.overlay)
	}
	_, wcmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc}) // gate default → wizard
	closed, ok := wcmd().(pairWizardClosedMsg)
	if !ok {
		t.Fatalf("Esc must close the wizard, got %T", wcmd())
	}
	m3, _ := m.Update(closed)
	cm := m3.(clientModel)
	if cm.overlay != nil {
		t.Fatalf("the chain must land back on the page, got overlay %T", cm.overlay)
	}
	if cm.instance != "agentA" {
		t.Fatalf("a bare Esc must not switch the slot, got %q", cm.instance)
	}
}
