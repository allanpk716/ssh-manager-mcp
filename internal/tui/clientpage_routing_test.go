package tui

// Plan 30 T4: the clientModel gate — same shape as the App gate. The client's
// only form overlay is editConnForm → newFormOverlay (already transparent, so
// NO layer-2 change is needed here — the gate's default branch forwards huh's
// unexported protocol msgs straight through to it).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"ssh-manager-mcp/internal/clientops"
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
		dataReadyMsg{}, syncDoneMsg{}, pullSucceededMsg{}, connSavedMsg{},
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

// TestClientLoopEditConnFormCompletes (真表单回环): drive editConnForm to
// completion through Update + drain. huh advances fields via its unexported
// nextFieldMsg cmds — only a correct gate routes them back into the form, so
// pre-gate every Enter chain dies on the first field.
//
// Deviations from the task brief, each forced by a real definition:
//   - values: validServeURL requires an https:// URL and validPin requires
//     sha256:<64 hex> — the brief's "127.0.0.1:1" / "sha256/AAAA" are invalid
//     (the brief itself defers to the validators' real rules).
//   - entry: the brief opens the form with wizard=true, but a wizard submit
//     (connSavedMsg) starts a REAL first pull whose failure REOPENS the form
//     (input preservation, TestClientWizard_PullFailureReopensFormWithDraft)
//     — the overlay would never be nil at the end. Panel mode + a seeded
//     Token-only cred opens the SAME form with the SAME completion
//     assertions and stays offline: the post-save path is connSavedMsg →
//     refreshDataCmdFor(m.instance) → errMsg (no cache DEK), so no pull
//     is ever attempted. The empty URL/Pin also mean no prefill — typed chars
//     land where intended.
//   - mechanics: fields advance via Enter + drain (T3's
//     TestWizardLoopServerFormCompletes pattern); typing all fields then one
//     Enter would pile every char into field 1. The brief's final
//     os.Stat(filepath.Join(t.TempDir(), …)) is also a SECOND TempDir — a
//     fresh dir that never sees the write; capture the dir once.
func TestClientLoopEditConnFormCompletes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", dir)
	// Spec rev5 §4 (review F1): an empty-field submit on a four-file VACUUM
	// resolved slot now refuses instead of silently rewriting cache.auth.json.
	// This happy-path driver only needs a non-vacuum slot — one bare bin
	// marker defeats the vacuum judgment and keeps the test's original intent
	// (full keystroke round → overlay closes → auth written).
	if err := os.WriteFile(filepath.Join(dir, "cache.bin"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newClientModelForGate(t)
	m.cred = &clientops.CacheCred{Token: "existing-token"} // panel 'c' guard; empty URL/Pin = no prefill
	m2, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Text: "c"})
	if cmd == nil {
		t.Fatal("'c' must open editConnForm and return its Init cmd")
	}
	m3 := drain(t, m2, cmd)
	for _, s := range []string{
		"https://127.0.0.1:8443",            // serve 地址 (validServeURL: https + host)
		"abcd1234",                          // 设备码 (replaces the seeded token)
		"sha256:" + strings.Repeat("a", 64), // pin (validPin: SPKI fingerprint)
	} {
		for _, r := range s {
			m3, _ = m3.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
		}
		var enterCmd tea.Cmd
		m3, enterCmd = m3.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		m3 = drain(t, m3, enterCmd)
	}
	if cm := m3.(clientModel); cm.overlay != nil {
		t.Fatal("editConnForm must complete and close")
	}
	if _, err := os.Stat(filepath.Join(dir, "cache.auth.json")); err != nil {
		t.Fatalf("cache.auth.json must be written: %v", err)
	}
}
