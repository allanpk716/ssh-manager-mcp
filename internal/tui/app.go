package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"

	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/roles"
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
	st      *store.Store
	page    page
	pages   [pageCount]listPage
	overlay overlay         // nil = 无
	role    roles.Role      // this machine's deployment role (T6: gates the [u] upgrade)
	upg     *upgradeSegment // nil = upgrade segment not running (T6)
	status  string
	err     error
}

// NewBrokerApp builds the broker console over an open store (caller owns Close).
func NewBrokerApp(st *store.Store) (App, error) {
	pages, err := FetchAll(st)
	if err != nil {
		return App{}, err
	}
	role, err := detectBrokerRole()
	if err != nil {
		return App{}, err // fail closed: a corrupt role.json surfaces to the CLI
	}
	return App{st: st, pages: pages, status: "就绪", role: role}, nil
}

// detectBrokerRole resolves the App's role: role.json first (authoritative
// since T1) — an unreadable/corrupt file is an ERROR (roles' fail-closed
// design: a broken state must guide `clear`, never silently degrade; in
// practice ResolveMode gates this earlier on the launch path, this is the
// second line). A nil state (pre-Plan-19 machine with a vault but no
// role.json) falls back to the probe inference in roles.ResolveMode —
// accepting only its standalone/server answers, since a broker App can never
// be the client role; when even that cannot decide, RoleStandalone is the
// safe default: it only ADDS the [u] upgrade affordance and never removes any
// capability.
func detectBrokerRole() (roles.Role, error) {
	s, err := roles.Load()
	if err != nil {
		return "", err
	}
	if s != nil {
		return s.Role, nil
	}
	if l, err := roles.ResolveMode(""); err == nil && l.Role == roles.RoleServer {
		return roles.RoleServer, nil
	}
	return roles.RoleStandalone, nil
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
	pages[pageProjects] = &projectsPage{items: projects, st: st}
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
		case k.Text == "u" && a.role == roles.RoleStandalone && a.upg == nil:
			// T6: non-destructive standalone→server upgrade. Only offered (and
			// only dispatched) on a standalone machine with no segment running;
			// a mid-flight press is swallowed by the upg guard.
			a.startUpgrade()
			return a, a.overlay.Init()
		case k.Text == "a", k.Text == "e", k.Text == "d", k.Text == "g":
			// F2 (fix round): while an upgrade segment is in flight (install/
			// probe/deviceIssue — overlay==nil windows), page action keys are
			// suppressed: opening a form overlay here would be clobbered by the
			// segment's next msg (serveInstalledMsg etc.) and leak a stray form.
			if a.upg != nil {
				return a, nil
			}
			// One case for all action keys: overlapping letters across pages
			// (a/e/d on servers AND projects, a on profiles) make separate
			// cases order-dependent — an earlier case would swallow a later
			// page's key. Dispatch by page instead.
			if a.page == pageServers {
				if cmd := a.serversKey(k); cmd != nil {
					return a, cmd
				}
			}
			if a.page == pageProfiles {
				pp, _ := a.pages[pageProfiles].(*profilesPage)
				switch k.Text {
				case "a":
					name := ""
					a.overlay = newFormOverlay("新增 Profile", newProfileForm(&name), func() tea.Cmd {
						return doAction(a.st, func() (string, error) {
							_, err := a.st.AddProfile(name)
							return "已新增 Profile " + name, err
						})
					})
				case "g": // grant servers to the current profile (multi-select by id)
					if cur := pp.current(); cur != nil {
						servers, err := a.st.ListServers()
						if err != nil {
							a.err = err
							a.status = ""
							return a, nil
						}
						if len(servers) == 0 {
							a.status = "无服务器可授权"
							return a, nil
						}
						chosen := []string{}
						a.overlay = newFormOverlay("授权服务器 → "+cur.Name, newGrantForm(servers, &chosen), func() tea.Cmd {
							return submitGrant(a.st, cur.ID, cur.Name, chosen)
						})
					}
				}
			}
			if a.page == pageProjects {
				pj, _ := a.pages[pageProjects].(*projectsPage)
				switch k.Text {
				case "a":
					profiles, err := a.st.ListProfiles()
					if err != nil {
						a.err = err
						a.status = ""
						return a, nil
					}
					if len(profiles) == 0 {
						a.status = "无 Profile 可绑定：请先在 Profiles 页创建"
						return a, nil
					}
					d := &projectDraft{}
					a.overlay = newFormOverlay("新增项目", newProjectForm(d, profiles), func() tea.Cmd {
						// The mutation runs AFTER the form closes; its token
						// rides tokenIssuedMsg straight into the secretView
						// overlay — one msg, then only the overlay holds it.
						return func() tea.Msg {
							_, token, err := a.st.AddProject(d.Name, d.ProfileID)
							if err != nil {
								return errMsg{err}
							}
							return tokenIssuedMsg{title: "项目 token", token: token}
						}
					})
				case "e": // rotate: old token dies, new one shown once
					if cur := pj.current(); cur != nil {
						confirm := false
						form := huh.NewForm(huh.NewGroup(huh.NewConfirm().
							Title(fmt.Sprintf("轮换 %q 的 token？（旧 token 立即失效）", cur.Name)).Value(&confirm)))
						a.overlay = newFormOverlay("轮换 token — "+cur.Name, form, func() tea.Cmd {
							if !confirm {
								return nil
							}
							return func() tea.Msg {
								token, err := a.st.RotateProject(cur.ID)
								if err != nil {
									return errMsg{err}
								}
								return tokenIssuedMsg{title: "项目 token（已轮换）", token: token}
							}
						})
					}
				case "d": // revoke: permanent, hidden from the list going forward
					if cur := pj.current(); cur != nil {
						confirm := false
						form := huh.NewForm(huh.NewGroup(huh.NewConfirm().
							Title(fmt.Sprintf("吊销项目 %q？（永久生效，不可恢复）", cur.Name)).Value(&confirm)))
						a.overlay = newFormOverlay("吊销项目", form, func() tea.Cmd {
							if !confirm {
								return nil
							}
							return doAction(a.st, func() (string, error) {
								return "已吊销 " + cur.Name, a.st.SetProjectStatus(cur.ID, models.ProjectRevoked)
							})
						})
					}
				}
			}
			if a.page == pageTokens {
				cp, _ := a.pages[pageTokens].(*cacheTokensPage)
				switch k.Text {
				case "a": // issue: form (name + serve addr hint) → tokenIssuedMsg
					d := &deviceDraft{}
					a.overlay = newFormOverlay("签发设备码", newCacheTokenForm(d), func() tea.Cmd {
						// Mutation + fingerprint load run AFTER the form closes;
						// the code rides tokenIssuedMsg straight into the
						// secretView overlay — one msg, then only the overlay
						// holds it. ServeURL is display-only and never stored.
						return func() tea.Msg {
							// cert first: a failing cert load must not mint an orphan device code (Plan 20 A4)
							_, _, fp, err := mcpserver.LoadOrCreateServeCert()
							if err != nil {
								return errMsg{fmt.Errorf("load serve cert for fingerprint: %w (run `serve cert-info` to diagnose)", err)}
							}
							_, code, err := a.st.AddCacheToken(strings.TrimSpace(d.Name))
							if err != nil {
								return errMsg{err}
							}
							return tokenIssuedMsg{title: "设备码 — " + d.Name, token: deviceCodeBody(d.ServeURL, code, fp)}
						}
					})
				case "d": // revoke (Lazy): the device's next pull is rejected
					if cur := cp.current(); cur != nil {
						confirm := false
						form := huh.NewForm(huh.NewGroup(huh.NewConfirm().
							Title(fmt.Sprintf("吊销设备码 %q？（该设备下次拉取将被拒绝）", cur.Name)).Value(&confirm)))
						a.overlay = newFormOverlay("吊销设备码", form, func() tea.Cmd {
							if !confirm {
								return nil
							}
							return doAction(a.st, func() (string, error) {
								return "已吊销 " + cur.Name, a.st.RevokeCacheToken(cur.Name)
							})
						})
					}
				}
			}
			if a.overlay != nil {
				return a, a.overlay.Init()
			}
		}
	case errMsg:
		a.err = m.err
		a.status = ""
		if a.upg != nil {
			// A segment action failed (e.g. device-code mint). Abort the
			// segment cleanly: back on the standalone console with the error
			// visible, [u] restarts it. Vault data is untouched — the mint is
			// cert-first/code-second, so a retry stays idempotent.
			a.upg, a.overlay = nil, nil
		}
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
		if a.upg != nil { // upgrade segment owns its formDoneMsg progression (T6)
			return a.upgradeFormDone(m)
		}
		a.overlay = nil
		return a, m.after // run the deferred action (e.g. re-fetch)
	case serveInstalledMsg:
		if a.upg == nil {
			return a, nil // defensive: only the upgrade segment produces this in the App
		}
		// Install outcome — either way the segment CONTINUES to the probe
		// (T4 discipline: install failure 不阻断; the result screen shows the
		// manual command, and upgradeComplete decides the role flip).
		a.upg.installErr = m.err
		a.upg.step = upgProbe
		a.overlay = nil
		return a, probeServe(a.upg.serveAddr)
	case serveProbeMsg:
		if a.upg == nil {
			return a, nil
		}
		a.upg.step = upgResult
		a.overlay = serveResultScreen(a.upg.installErr, m)
		return a, nil
	case deviceCodeIssuedMsg:
		if a.upg == nil {
			return a, nil
		}
		// The code transits this one message and then lives only inside the
		// overlay (same discipline as tokenIssuedMsg). Pages re-fetch so the
		// 设备码 tab lists the new code behind the one-time screen.
		a.upg.step = upgDeviceCode
		a.upg.deviceFp = m.fingerprint
		a.err, a.status = nil, ""
		a.overlay = wizTokenScreen("设备码 — "+a.upg.clientName, m.code,
			fmt.Sprintf("填到 client 机向导；或拼 cache pull --token '%s:%s'", m.code, m.fingerprint),
			"主控台 设备码页 [a] 重发")
		if pages, err := FetchAll(a.st); err == nil {
			a.pages = pages
		}
		return a, nil
	case tokenIssuedMsg:
		// Mutation succeeded and minted a token: take over the screen with the
		// one-time secret view. Pages are re-fetched now so the list is fresh
		// when the user dismisses (dismiss is a plain formDoneMsg{}).
		a.err = nil
		a.status = ""
		a.overlay = &secretView{title: m.title, body: m.token}
		if pages, err := FetchAll(a.st); err == nil {
			a.pages = pages
		}
		return a, nil
	}
	return a, nil
}

// serversKey dispatches the servers page's action keys: a=新增, e=编辑,
// d=删除 — each opens its form overlay (setting a.overlay) and returns the
// overlay's Init cmd. Any other key is a no-op returning nil (notably g: the
// profiles page's grant key must do nothing here — per-page dispatch is what
// keeps the overlapping letters across pages from swallowing each other).
// Extracted from Update in Plan 20 T1 so the key→page→action mapping is
// table-testable (TestServersPageDispatch).
func (a *App) serversKey(k tea.Key) tea.Cmd {
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
	if a.overlay == nil {
		return nil
	}
	return a.overlay.Init()
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

// formDoneMsg closes a form overlay. aborted is set ONLY by formOverlay's
// Esc/huh-abort path: the user backed out and the huh-bound answer values are
// untrustworthy (a select may have already committed its preset default into
// them). Consumers that advance on the answers (the upgrade segment) treat it
// as a cancel; bare dismissals (static screens, secretView) leave it false.
type formDoneMsg struct {
	after   tea.Cmd
	aborted bool
}

func (a App) View() tea.View {
	if a.overlay != nil {
		return a.overlay.View()
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render(" ssh-manager ") + "\n")
	tabs := make([]string, pageCount)
	for i := page(0); i < pageCount; i++ {
		t := "(?)"
		if a.pages[i] != nil {
			t = a.pages[i].Title()
		}
		if i == a.page {
			t = selStyle.Render("[" + t + "]")
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

func (a App) footer() string {
	// Per-page key hints: keys that don't apply on a page aren't advertised.
	keys := ""
	switch a.page {
	case pageServers:
		keys = "[a]新增 [e]编辑 [d]删除"
	case pageProfiles:
		keys = "[a]新增 [g]授权"
	case pageProjects:
		keys = "[a]新增 [e]轮换 [d]吊销"
	case pageTokens:
		keys = "[a]签发 [d]吊销"
	}
	tail := "Tab 切页  q 退出"
	if keys != "" {
		tail = keys + "  " + tail
	}
	if a.role == roles.RoleStandalone {
		// T6: only a standalone machine can upgrade; the footer hint disappears
		// the moment the role flips (upgradeComplete sets a.role).
		tail = "[u]升级为 server  " + tail
	}
	return tail
}

// lipColumns renders two columns side by side (width-aware lipgloss join).
func lipColumns(left, right string) string {
	return lipgloss.JoinHorizontal(lipgloss.Top, left, right)
}
