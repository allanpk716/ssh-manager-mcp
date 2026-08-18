package tui

// Plan 30 T1: routing-test infrastructure shared by all gate/loop tests.

import (
	"path/filepath"
	"reflect"
	"testing"

	"charm.land/bubbles/v2/cursor"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// spyOverlay records every message the routing layer hands it. update returns
// a NEW instance when swap != nil — that lets tests assert the gate writes the
// overlay pointer back (Plan 30 注记 8).
type spyOverlay struct {
	got  []tea.Msg
	cmd  tea.Cmd // returned on every Update (sentinel hand-back assertion)
	swap *spyOverlay
}

func (s *spyOverlay) Title() string  { return "spy" }
func (s *spyOverlay) Init() tea.Cmd  { return nil }
func (s *spyOverlay) View() tea.View { return tea.NewView("spy") }
func (s *spyOverlay) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	s.got = append(s.got, msg)
	if s.swap != nil {
		return s.swap, s.cmd
	}
	return s, s.cmd
}

// spySaw reports whether the spy received a message of the same type as want.
func (s *spyOverlay) spySaw(want tea.Msg) bool {
	for _, m := range s.got {
		if reflect.TypeOf(m) == reflect.TypeOf(want) {
			return true
		}
	}
	return false
}

// drain simulates the bubbletea runtime: execute cmds, feed each produced msg
// back into m.Update, repeat. Blink/tick msgs are DROPPED — they self-perpetuate
// and would never end the loop (same discipline as the old editpage pump).
// tea.BatchMsg unfolds. Bounded: a runaway loop fails loudly instead of hanging.
func drain(t *testing.T, m tea.Model, cmds ...tea.Cmd) tea.Model {
	t.Helper()
	queue := append([]tea.Cmd(nil), cmds...)
	for steps := 0; len(queue) > 0; steps++ {
		if steps > 300 {
			t.Fatal("drain: runaway cmd loop (>300 steps)")
		}
		cmd := queue[0]
		queue = queue[1:]
		if cmd == nil {
			continue
		}
		msg := cmd()
		switch msg := msg.(type) {
		case nil, cursor.BlinkMsg, spinner.TickMsg:
			continue
		case tea.BatchMsg:
			queue = append(queue, msg...)
			continue
		}
		var next tea.Cmd
		m, next = m.Update(msg)
		queue = append(queue, next)
	}
	return m
}

// probeMsg is an unknown-to-everyone message type: the gate must forward it.
type probeMsg struct{}

// seedStoreApp is newTestApp (app_test.go:21) plus the store handle — loop
// tests need to assert persisted rows.
func seedStoreApp(t *testing.T) (App, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "t.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	credID, err := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("p")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddServer(&models.Server{
		Name: "gpu", Host: "192.0.2.10", User: "u", Port: 22,
		AuthMethod: models.AuthPassword, CredentialID: credID,
	}); err != nil {
		t.Fatal(err)
	}
	a, err := NewBrokerApp(st)
	if err != nil {
		t.Fatal(err)
	}
	return a, st
}
