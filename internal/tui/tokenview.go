package tui

import (
	tea "charm.land/bubbletea/v2"
)

// tokenIssuedMsg carries a freshly minted token from a store mutation cmd to
// App.Update, which swaps in a secretView overlay. The plaintext transits this
// one message and then lives only inside the overlay — never in form state.
type tokenIssuedMsg struct{ title, token string }

// secretView shows a one-time secret full-screen. Any key dismisses it
// (via formDoneMsg{}), after which the plaintext is gone for good — the store
// keeps only the hash/prefix, so it can never be shown again.
type secretView struct{ title, body string }

func (s *secretView) Title() string { return s.title }
func (s *secretView) Init() tea.Cmd { return nil }
func (s *secretView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := msg.(tea.KeyPressMsg); ok { // bubbletea v2: KeyMsg is an interface; presses are KeyPressMsg
		return s, func() tea.Msg { return formDoneMsg{} }
	}
	return s, nil
}
func (s *secretView) View() tea.View {
	return tea.NewView(titleStyle.Render(" "+s.title+" ") + "\n\n" +
		secretStyle.Render(s.body) +
		"\n\n⚠ 仅此一次显示（关闭后不可再看）。按任意键返回。\n")
}
