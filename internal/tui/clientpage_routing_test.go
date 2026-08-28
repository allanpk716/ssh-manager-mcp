package tui

// Plan 30 T4: the clientModel gate — same shape as the App gate. Plan 42 批1
// T8 retired the connect-form overlay; the remaining overlay is the instance
// picker (transparent to the gate's default branch either way).

import (
	"strings"
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

// TestClientPanel_CKeyPointsAtPair (Plan 42 批1 T8, 批1 前置 #4): the [c] key
// no longer opens a form — it surfaces the pair guidance line (pair is the
// only guided onboarding path; the connect-form is retired).
func TestClientPanel_CKeyPointsAtPair(t *testing.T) {
	m := newClientModelForGate(t)
	m2, _ := m.Update(tea.KeyPressMsg{Code: 'c', Text: "c"})
	cm := m2.(clientModel)
	if cm.overlay != nil {
		t.Fatal("[c] must not open an overlay anymore")
	}
	if !strings.Contains(cm.status, "ssh-manager pair") {
		t.Fatalf("[c] must point at ssh-manager pair, got %q", cm.status)
	}
}
