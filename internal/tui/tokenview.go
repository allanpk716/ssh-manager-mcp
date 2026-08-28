package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

// tokenIssuedMsg carries a freshly minted token from a store mutation cmd to
// App.Update, which swaps in a secretView overlay. The plaintext transits this
// one message and then lives only inside the overlay — never in form state.
//
// usage/recovery/snippet are OPTIONAL guidance (zero value = the historical
// bare-token behavior; the device-code emitter puts its whole body in `token`
// and the wizard emits none of these — it owns its own two-screen flow):
//   - usage:    "token 去哪"一行（wizTokenScreen 的用途行同款纪律）
//   - recovery: "丢失→"一行（store 只存 hash，明文不可恢复）
//   - snippet:  mcp.json 引导块 = mcpConfigLines 完整输出
//     （引导语 + JSON + 说明块都在其中）；nil 时整块不渲染。
type tokenIssuedMsg struct {
	title, token string
	usage        string
	recovery     string
	snippet      []string
}

// body renders the full secretView body: token first (always), then the
// optional guidance blocks in fixed order (用途 → 丢失 → 片段).
func (m tokenIssuedMsg) body() string {
	var b strings.Builder
	b.WriteString(m.token)
	if m.usage != "" {
		b.WriteString("\n\n用途：" + m.usage)
	}
	if m.recovery != "" {
		b.WriteString("\n⚠ 仅此一次。丢失 → " + m.recovery)
	}
	if len(m.snippet) > 0 {
		b.WriteString("\n\n" + strings.Join(m.snippet, "\n"))
	}
	return b.String()
}

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
