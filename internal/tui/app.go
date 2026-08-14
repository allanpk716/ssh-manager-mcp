package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"

	"ssh-manager-mcp/internal/store"
)

type page int

const (
	pageServers page = iota
	pageProfiles
	pageProjects
	pageTokens
	pageCount
)

// listPage is the shared shape of the four entity pages (each holds items+cursor).
type listPage interface {
	Title() string
	Rows() []string
	Detail() string
	Cursor() int
	Select(i int)
}

// overlay is a full-screen sub-model (huh form pages, token display) that owns
// keys until it finishes by emitting formDoneMsg.
type overlay interface {
	tea.Model
	Title() string
}

type App struct {
	mode    Mode
	st      *store.Store // client 模式为 nil
	page    page
	pages   [pageCount]listPage
	overlay overlay // nil = 无
	status  string
	err     error
}

// NewBrokerApp builds the broker console over an open store (caller owns Close).
func NewBrokerApp(st *store.Store) (App, error) {
	pages, err := FetchAll(st)
	if err != nil {
		return App{}, err
	}
	return App{mode: ModeBroker, st: st, pages: pages, status: "就绪"}, nil
}

// FetchAll loads the four entity pages in one shot.
func FetchAll(st *store.Store) ([pageCount]listPage, error) {
	var pages [pageCount]listPage
	servers, err := st.ListServers()
	if err != nil {
		return pages, err
	}
	profiles, err := st.ListProfiles()
	if err != nil {
		return pages, err
	}
	projects, err := st.ListProjects()
	if err != nil {
		return pages, err
	}
	tokens, err := st.ListCacheTokens()
	if err != nil {
		return pages, err
	}
	pages[pageServers] = &serversPage{items: servers}
	pages[pageProfiles] = &profilesPage{items: profiles, st: st}
	pages[pageProjects] = &projectsPage{items: projects}
	pages[pageTokens] = &cacheTokensPage{items: tokens}
	return pages, nil
}

func (a App) Init() tea.Cmd { return nil }

func (a App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.KeyPressMsg: // bubbletea v2: KeyMsg is an interface; presses are KeyPressMsg
		if a.overlay != nil { // overlay owns keys until done (form overlays send formDoneMsg)
			ov, cmd := a.overlay.Update(msg)
			a.overlay, _ = ov.(overlay)
			return a, cmd
		}
		k := m.Key()
		switch {
		case k.Code == 'c' && k.Mod == tea.ModCtrl:
			return a, tea.Quit
		case k.Code == tea.KeyTab && k.Mod == tea.ModShift:
			a.page = (a.page + pageCount - 1) % pageCount
			return a, nil
		case k.Code == tea.KeyTab && k.Mod == 0:
			a.page = (a.page + 1) % pageCount
			return a, nil
		case k.Code == tea.KeyUp && k.Mod == 0, k.Text == "k":
			a.move(-1)
			return a, nil
		case k.Code == tea.KeyDown && k.Mod == 0, k.Text == "j":
			a.move(1)
			return a, nil
		case k.Text == "q":
			return a, tea.Quit
		case k.Text == "a", k.Text == "e", k.Text == "d":
			if a.mode == ModeBroker && a.page == pageServers {
				sp, _ := a.pages[pageServers].(*serversPage)
				switch k.Text {
				case "a":
					draft := &serverDraft{}
					a.overlay = newFormOverlay("新增服务器", newServerForm(draft, false), func() tea.Cmd {
						return submitServer(a.st, nil, draft)
					})
				case "e":
					if cur := sp.current(); cur != nil {
						draft := prefill(cur)
						a.overlay = newFormOverlay("编辑服务器", newServerForm(draft, true), func() tea.Cmd {
							return submitServer(a.st, cur, draft)
						})
					}
				case "d":
					if cur := sp.current(); cur != nil {
						confirm := false
						form := huh.NewForm(huh.NewGroup(huh.NewConfirm().
							Title(fmt.Sprintf("删除服务器 %q？（profile 授权一并失效）", cur.Name)).Value(&confirm)))
						a.overlay = newFormOverlay("删除服务器", form, func() tea.Cmd {
							if !confirm {
								return nil
							}
							return doAction(a.st, func() (string, error) {
								return "已删除 " + cur.Name, a.st.DeleteServer(cur.ID)
							})
						})
					}
				}
				if a.overlay != nil {
					return a, a.overlay.Init()
				}
			}
		}
	case errMsg:
		a.err = m.err
		a.status = ""
		return a, nil
	case actionDoneMsg:
		a.err = nil
		a.status = m.desc
		pages, err := FetchAll(a.st)
		if err == nil {
			a.pages = pages
		}
		return a, nil
	case formDoneMsg:
		a.overlay = nil
		return a, m.after // run the deferred action (e.g. re-fetch)
	}
	return a, nil
}

func (a *App) move(d int) {
	p := a.pages[a.page]
	if p == nil {
		return
	}
	rows := p.Rows()
	if len(rows) == 0 {
		return
	}
	c := p.Cursor() + d
	if c < 0 {
		c = 0
	}
	if c >= len(rows) {
		c = len(rows) - 1
	}
	p.Select(c)
}

type errMsg struct{ err error }
type actionDoneMsg struct{ desc string }
type formDoneMsg struct{ after tea.Cmd }

func (a App) View() tea.View {
	if a.overlay != nil {
		return a.overlay.View()
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render(fmt.Sprintf(" ssh-manager%s ", modeTag(a.mode))) + "\n")
	tabs := make([]string, pageCount)
	for i := page(0); i < pageCount; i++ {
		t := "(?)"
		if a.pages[i] != nil {
			t = a.pages[i].Title()
		}
		if i == a.page {
			t = selStyle.Render("["+t+"]")
		} else {
			t = "[" + t + "]"
		}
		tabs[i] = t
	}
	b.WriteString(strings.Join(tabs, " ") + footerStyle.Render("  Tab 切页") + "\n")
	p := a.pages[a.page]
	left, right := "（空）", ""
	if p != nil {
		rows := p.Rows()
		if len(rows) > 0 {
			// re-render with the selected row highlighted
			for i, r := range rows {
				if i == p.Cursor() {
					rows[i] = selStyle.Render(r)
				}
			}
			left = strings.Join(rows, "\n")
		}
		right = detailStyle.Render(p.Detail())
	}
	b.WriteString(lipColumns(left, right))
	b.WriteString("\n")
	if a.err != nil {
		b.WriteString(errStyle.Render("✗ "+a.err.Error()) + "\n")
	} else if a.status != "" {
		b.WriteString(footerStyle.Render("✓ "+a.status) + "\n")
	}
	b.WriteString(footerStyle.Render(a.footer()))
	return tea.NewView(b.String())
}

func modeTag(m Mode) string {
	if m == ModeClient {
		return " (client)"
	}
	return ""
}

func (a App) footer() string {
	if a.mode == ModeClient {
		return "[s]同步 [c]编辑连接 [t]TTL  q 退出"
	}
	return "[a]新增 [e]编辑 [d]删除 [g]授权  Tab 切页  q 退出"
}

// lipColumns renders two columns side by side (width-aware lipgloss join).
func lipColumns(left, right string) string {
	return lipgloss.JoinHorizontal(lipgloss.Top, left, right)
}

// clientPlaceholder is the Task-8 stand-in: client mode is not implemented yet.
type clientPlaceholder struct{}

func (clientPlaceholder) Init() tea.Cmd { return nil }
func (c clientPlaceholder) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := msg.(tea.KeyPressMsg); ok {
		return c, tea.Quit
	}
	return c, nil
}
func (clientPlaceholder) View() tea.View {
	return tea.NewView("client 模式将在后续版本提供（按任意键退出）")
}
