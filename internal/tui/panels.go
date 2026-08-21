package tui

// panels.go — the shared desktop-panel machinery every entity page uses
// (2026-08-17 样张 → 全页铺开): each page embeds panelList, a bubbles list
// that owns the cursor/pagination//`/`-filter, and renders itself through
// renderPanel (bordered list panel beside the bordered detail box, fitted to
// the terminal). serversPage additionally keeps its ⚠ view logic (see
// servers.go) — the ORDER it mirrors into the list is its ⚠-sorted view.

import (
	"strings"

	"charm.land/bubbles/v2/cursor"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// panelList is the embedded list panel: the cursor owner for a page's items
// (two-line rows, pagination, built-in `/` filter, help bar).
type panelList struct {
	list list.Model
}

// newPanelList builds the list with this console's keymap — rebindListKeys is
// FIRST-class here: the DEFAULT keymap binds single letters that collide with
// the console's action keys (d=下一页 vs [d]删除, u=上一页 vs [u]升级,
// g=跳到开头 vs [g]授权) — paging/jump keep only non-letter keys; k/j/up/down
// stay because they ARE this console's nav keys.
func newPanelList(title string) panelList {
	l := list.New(nil, list.NewDefaultDelegate(), 30, 12)
	l.Title = title
	rebindListKeys(&l.KeyMap)
	return panelList{list: l}
}

func rebindListKeys(km *list.KeyMap) {
	km.PrevPage = key.NewBinding(key.WithKeys("left", "pgup"), key.WithHelp("←pgup", "上页"))
	km.NextPage = key.NewBinding(key.WithKeys("right", "pgdn"), key.WithHelp("→pgdn", "下页"))
	km.GoToStart = key.NewBinding(key.WithKeys("home"), key.WithHelp("home", "到顶"))
	km.GoToEnd = key.NewBinding(key.WithKeys("end"), key.WithHelp("end", "到底"))
}

func (p *panelList) filtering() bool { return p.list.FilterState() == list.Filtering }

func (p *panelList) Cursor() int { return p.list.Index() }

func (p *panelList) Select(i int) {
	if p.list.Paginator.PerPage > 0 { // Select divides by PerPage — guard the unsized case
		p.list.Select(i)
	}
}

// listUpdate feeds one message to the list and returns its cmd.
func (p *panelList) listUpdate(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	p.list, cmd = p.list.Update(msg)
	return cmd
}

// setListItems mirrors items into the list (caller fixes the order — for the
// servers page that is its ⚠-sorted view) and clamps the cursor into n when
// the set shrank. While the `/` text filter is taking input the list owns its
// cursor — no clamping against the unfiltered count there.
func (p *panelList) setListItems(items []list.Item, n int) {
	p.list.SetItems(items)
	if p.filtering() {
		return
	}
	if n > 0 {
		if p.list.Index() >= n {
			p.Select(n - 1)
		}
	} else if p.list.Index() != 0 {
		p.Select(0)
	}
}

// listMsg reports whether msg belongs to a list panel's own event stream:
// keypresses (nav + the `/` filter input), the async filter's results, and
// the cursor-blink / spinner ticks its sub-components emit.
func listMsg(msg tea.Msg) bool {
	switch msg.(type) {
	case tea.KeyPressMsg, list.FilterMatchesMsg, cursor.BlinkMsg, spinner.TickMsg:
		return true
	}
	return false
}

// panelStyle frames the list panel with the same rounded border as the detail
// box — the two-panel desktop look.
var panelStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)

// renderPanel draws the desktop-style body fitted to the terminal: the
// bordered list panel (pagination, `/` filter and help bar come from the
// list) beside the bordered detail box. The list paginates to the height it
// is given, so a long list never grows the frame past the screen; the
// wrapped detail is CLIPPED to the same budget (lipgloss Height pads short
// content but does not truncate long content — an over-tall detail would
// grow the frame, e.g. many wrapped lines on a short terminal).
func renderPanel(l *list.Model, detail string, width, height int) string {
	width, height = max(width, 24), max(height, 6)
	listW := min(max(width*2/5, 18), width-14)
	// Style.Height sets the CONTENT height (borders add 2) — size both
	// panels to height-2 so the joined body is exactly `height` rows.
	l.SetSize(listW-4, height-4)
	avail := width - listW - colGutter - detailChrome
	if avail > 0 {
		detail = ansi.Wrap(detail, avail, "-")
	}
	if content := height - 2; content > 0 {
		if lines := strings.Split(detail, "\n"); len(lines) > content {
			detail = strings.Join(lines[:content], "\n")
		}
	}
	return lipgloss.JoinHorizontal(lipgloss.Top,
		panelStyle.Height(height-2).Width(listW-2).Render(l.View()),
		detailStyle.Height(height-2).Render(detail),
	)
}
