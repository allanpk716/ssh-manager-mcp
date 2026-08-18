// editpage.go — the field-picker server edit page (Plan 29 T2): a two-state
// overlay replacing the three-group long form for the `e` flow. list state
// (bubbles list over editFields + a trailing save sentinel, paginated,
// per-field dirty marks ● + （已改）) ↔ field state (the field's single-field
// huh form). Save keeps submitServer's whole-draft semantics untouched —
// only the collection UX changed.
package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

type editPageState int

const (
	editStateList  editPageState = iota // field picker
	editStateField                      // single-field huh form
)

// saveItemTitle is the trailing sentinel row: Enter submits the whole draft.
const saveItemTitle = "✓ 保存并退出"

// editPageHeight is the list state's fixed row budget (the page is an
// overlay; the App gate forwards WindowSizeMsg and the width follows it, but
// the height stays this fixed budget and the paginator bounds the rows).
// 20 rows → ~6 items per page → 3 pages for the 16 edit rows: pagination
// stays visible (the user's original pain point) without excessive paging.
const editPageHeight = 20

// serverEditPage is the two-state edit overlay. Pointer semantics (the
// importFlow pattern): the App hands every KeyPressMsg to the SAME
// instance, and the huh Value-pointer bindings must stay pointed at one
// stable draft across Update calls.
type serverEditPage struct {
	st    *store.Store
	orig  *models.Server // edit target (nil = add-mode field set); submitServer's cur
	d     *serverDraft
	width int

	fields []editField       // editFields(orig != nil) — fixed order
	snap   map[string]string // page-entry snapshot: the dirty baseline
	list   list.Model        // list state's picker (pagination, cursor)

	state     editPageState // current half of the state machine
	field     editField     // field-state target
	fieldSnap string        // snapshotDraft(d)[field.Key] at field-state entry — the Esc restore source
	form      *huh.Form     // field-state embedded form

	// submit is the save seam (T3-compatible): the sentinel's Enter emits
	// formDoneMsg{after: submit()}. Defaults to submitServer; tests may
	// replace it to capture the call.
	submit func() tea.Cmd
}

// newServerEditPage builds the picker over a prefilled draft (the App's `e`
// flow: prefill(cur)). width is the terminal width from the App's
// WindowSizeMsg state; the field forms follow it too.
func newServerEditPage(st *store.Store, orig *models.Server, d *serverDraft, width int) *serverEditPage {
	name := "新增服务器"
	if orig != nil {
		name = orig.Name // orig, NOT d.Name — the draft's name may be edited
	}
	p := &serverEditPage{
		st: st, orig: orig, d: d, width: width,
		fields: editFields(orig != nil),
	}
	p.snap = snapshotDraft(d)
	p.list = list.New(nil, list.NewDefaultDelegate(), max(width-2, 20), editPageHeight)
	p.list.Title = "编辑服务器: " + name
	rebindListKeys(&p.list.KeyMap)
	// The page owns Enter/Esc; the list must not turn a stray q into tea.Quit.
	// The built-in filter stays off: its async FilterMatchesMsg now reaches
	// the page through the App gate, but the page's default branch only feeds
	// the FIELD-state form — the list state never consumes them, so enabling
	// the filter would still leave it half-broken.
	p.list.DisableQuitKeybindings()
	p.list.SetFilteringEnabled(false)
	p.list.SetShowStatusBar(false)
	p.list.SetShowHelp(false) // the page renders its own footer help
	p.refreshItems()
	p.submit = func() tea.Cmd { return submitServer(st, orig, d) }
	return p
}

// fieldItem adapts an editField row (or the save sentinel) to the list: the
// title carries the (possibly ●-prefixed, lipgloss-highlighted) label, the
// description the value preview from fieldPreview.
type fieldItem struct{ title, desc string }

func (i fieldItem) FilterValue() string { return i.title }
func (i fieldItem) Title() string       { return i.title }
func (i fieldItem) Description() string { return i.desc }

// refreshItems rebuilds the list rows from the current draft — dirty titles
// get the ● prefix (fieldPreview) rendered in the repo's attention color.
// Called at construction and on every return to list state.
func (p *serverEditPage) refreshItems() {
	dirty := dirtyAgainst(p.d, p.snap)
	items := make([]list.Item, 0, len(p.fields)+1)
	for _, f := range p.fields {
		title, desc := fieldPreview(f, p.d, dirty[f.Key])
		if dirty[f.Key] {
			title = warnStyle.Render(title)
		}
		items = append(items, fieldItem{title: title, desc: desc})
	}
	items = append(items, fieldItem{title: saveItemTitle, desc: "Enter 提交全部改动"})
	p.list.SetItems(items) // item count is fixed — no cursor clamp needed
}

func (p *serverEditPage) Title() string { return p.list.Title }
func (p *serverEditPage) Init() tea.Cmd { return nil }

// Update routes one message. The App gate forwards every non-owned msg to
// the overlay (keys, WindowSizeMsg, huh's unexported protocol msgs) and
// hands the returned cmds back to the runtime: list state owns Enter (open
// field / save) and Esc (abort), every other key goes to the list (↑↓
// paging); field state forwards to the embedded form and RETURNS the form's
// cmds — huh advances fields via nextFieldMsg/nextGroupMsg whose messages
// now route back through the gate (Plan 30; the synchronous pump that
// worked around their being dropped is gone).
func (p *serverEditPage) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.KeyPressMsg:
		k := m.Key()
		if p.state == editStateField {
			if k.Code == tea.KeyEsc {
				return p.restoreField()
			}
			return p.feedForm(m)
		}
		switch {
		case k.Code == tea.KeyEsc:
			// whole-page cancel: no store write, the draft dies with the page
			return p, func() tea.Msg { return formDoneMsg{aborted: true} }
		case k.Code == tea.KeyEnter:
			return p.openCurrent()
		default:
			// nav/paging keys: the list owns them; its cmd stream is empty
			// with the filter off and the spinner idle
			var cmd tea.Cmd
			p.list, cmd = p.list.Update(m)
			_ = cmd
			return p, nil
		}
	case tea.WindowSizeMsg:
		if m.Width > 0 {
			p.width = m.Width
			p.list.SetSize(max(p.width-2, 20), editPageHeight)
		}
		return p, nil
	default:
		// Plan 30 layer-2: huh's unexported protocol msgs land here via the
		// App gate. Field state only — the list state has no form.
		if p.state == editStateField && p.form != nil {
			return p.feedForm(msg)
		}
		return p, nil
	}
}

// openCurrent handles Enter on the list state's cursor row: the save
// sentinel emits the formDoneMsg that closes the page with the submit
// chained as its after (the mutation runs after the overlay closes — the
// formOverlay lifecycle); a field row snapshots the field's value and opens
// its single-field form.
func (p *serverEditPage) openCurrent() (tea.Model, tea.Cmd) {
	i := p.list.Index()
	if i < 0 {
		return p, nil
	}
	if i >= len(p.fields) {
		return p, func() tea.Msg { return formDoneMsg{after: p.submit()} }
	}
	p.field = p.fields[i]
	// T1 seam rule: the Esc-restore source is the SNAPSHOT value, never
	// f.Get (secret Gets return status strings, not values). Set accepts
	// the snapshot's canonical forms.
	p.fieldSnap = snapshotDraft(p.d)[p.field.Key]
	p.form = p.field.Build(p.d)
	p.form.WithWidth(formWidth(p.width))
	p.state = editStateField
	return p, p.form.Init() // async: the cmd's msgs (blink/focus) now route back (Plan 30)
}

// restoreField is field-state Esc: undo this field's edits and return to
// the list. huh binds Value pointers directly, so the user's keystrokes
// have already mutated the draft — the single-field snapshot taken at entry
// is the only undo source.
func (p *serverEditPage) restoreField() (tea.Model, tea.Cmd) {
	p.field.Set(p.d, p.fieldSnap)
	p.form, p.field, p.fieldSnap = nil, editField{}, ""
	p.state = editStateList
	p.refreshItems()
	return p, nil
}

// feedForm forwards one message to the embedded form and runs the post-update
// tail (field-level undo on abort; commit + dirty refresh on complete). The
// returned cmd goes back to the App's routing — the ASYNC round trip replaces
// the old synchronous pump (Plan 30 T5; the pump's 530ms blink-block hazard
// class is gone with it).
func (p *serverEditPage) feedForm(msg tea.Msg) (tea.Model, tea.Cmd) {
	fm, cmd := p.form.Update(msg)
	if nf, ok := fm.(*huh.Form); ok {
		p.form = nf
	}
	if p.form.State == huh.StateAborted { // ctrl+c inside huh: field-level undo, like Esc
		return p.restoreField()
	}
	if p.form.State == huh.StateCompleted {
		p.form, p.field, p.fieldSnap = nil, editField{}, ""
		p.state = editStateList
		p.refreshItems()
		return p, nil
	}
	return p, cmd
}

// formWidth sizes the embedded single-field form to the terminal: the huh
// default (80) overflows a 60-col terminal, so clamp under the page width
// with a little chrome margin, floored at 24 and capped at the default.
func formWidth(pageWidth int) int {
	if pageWidth <= 0 {
		return 80
	}
	return min(max(pageWidth-6, 24), 80)
}

// View renders the current state. Every line is hard-clipped to the page
// width (narrow terminals lose the tail gracefully instead of the renderer
// breaking mid-glyph).
func (p *serverEditPage) View() tea.View {
	var b strings.Builder
	if p.state == editStateField {
		b.WriteString(titleStyle.Render(" "+p.list.Title+" — "+p.field.Label+" ") + "\n\n")
		b.WriteString(p.form.View() + "\n\n")
		b.WriteString(footerStyle.Render("Enter 确认 · Esc 放弃本字段") + "\n")
	} else {
		b.WriteString(p.list.View() + "\n")
		b.WriteString(footerStyle.Render(fmt.Sprintf("↑↓ 选择 · Enter 编辑 · Esc 取消 · 第 %d/%d 页",
			p.list.Paginator.Page+1, p.list.Paginator.TotalPages)) + "\n")
	}
	return tea.NewView(clipLines(b.String(), p.width))
}

// clipLines truncates every display line to width (ANSI-aware). Width 0
// (unknown) renders unclipped — same policy as the App's clip helper.
func clipLines(s string, width int) string {
	if width <= 0 {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if lipgloss.Width(l) > width {
			lines[i] = ansi.Truncate(l, width, "…")
		}
	}
	return strings.Join(lines, "\n")
}
