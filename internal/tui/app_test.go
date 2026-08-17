package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/bubbles/v2/cursor"
	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// newTestApp opens a temp store seeded with one server and builds the broker App.
func newTestApp(t *testing.T) App {
	t.Helper()
	dir := t.TempDir()
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "t.db"), mk)
	if err != nil {
		t.Fatalf("open temp store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	// Servers carry a FK to credentials — seed one first.
	credID, err := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("p")})
	if err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	if _, err := st.AddServer(&models.Server{
		Name: "gpu", Host: "192.0.2.10", User: "u", Port: 22,
		AuthMethod: models.AuthPassword, CredentialID: credID,
	}); err != nil {
		t.Fatalf("seed server: %v", err)
	}
	a, err := NewBrokerApp(st)
	if err != nil {
		t.Fatalf("NewBrokerApp: %v", err)
	}
	return a
}

func TestApp_TabCyclesPages(t *testing.T) {
	a := newTestApp(t)
	if a.page != pageServers {
		t.Fatalf("start page = %v", a.page)
	}
	m2, _ := a.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if m2.(App).page != pageProfiles {
		t.Fatalf("tab: %v", m2.(App).page)
	}
	m3, _ := m2.(App).Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	if m3.(App).page != pageServers {
		t.Fatalf("shift-tab wrap: %v", m3.(App).page)
	}
}

func TestApp_QuitOnQ(t *testing.T) {
	a := newTestApp(t)
	_, cmd := a.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if cmd == nil {
		t.Fatal("q must produce a quit cmd")
	}
}

// TestServersPageDispatch (Plan 20 T1, paying the Plan 18 T6 dispatch-table
// debt): the key→page→action mapping on the servers page, driven through the
// real Update path. a/e/d/i must open their form overlay (right title + the
// overlay's Init cmd comes back); g must be a NO-OP here — it is the profiles
// page's grant key, and per-page dispatch is what keeps the overlapping
// letters (a/e/d on servers AND projects) from swallowing each other. `i`
// (ssh-config import) landed in Task 10. The emptyList cases (Plan 21 T3) pin
// the no-current-target guard: with ZERO servers, e/d must be silent no-ops
// (overlay stays nil, nil cmd, no panic) — `a`/`i` still work on an empty
// list, but the selection-dependent keys must not invent a target.
func TestServersPageDispatch(t *testing.T) {
	cases := []struct {
		key         string
		wantOverlay string // "" = the key must be a no-op on this page
		emptyList   bool   // drive against an EMPTY servers list
	}{
		{"a", "新增服务器", false},
		{"e", "编辑服务器", false},
		{"d", "删除服务器", false},
		{"i", "导入 ssh config", false},
		{"g", "", false},
		{"e", "", true},
		{"d", "", true},
	}
	for _, c := range cases {
		a := newTestApp(t) // fresh app per key: one seeded server at cursor 0
		if c.emptyList {
			sp, _ := a.pages[pageServers].(*serversPage)
			sp.items = nil
			sp.rebuild()
		}
		m, cmd := a.Update(tea.KeyPressMsg{Code: rune(c.key[0]), Text: c.key})
		got := m.(App)
		if c.wantOverlay == "" {
			if got.overlay != nil || cmd != nil {
				t.Fatalf("key %q on servers page must be a no-op: overlay=%v cmd=%v", c.key, got.overlay, cmd)
			}
			continue
		}
		if got.overlay == nil {
			t.Fatalf("key %q must open the %q overlay, got none", c.key, c.wantOverlay)
		}
		if title := got.overlay.Title(); title != c.wantOverlay {
			t.Fatalf("key %q opened %q, want %q", c.key, title, c.wantOverlay)
		}
		if cmd == nil {
			t.Fatalf("key %q must return the opened overlay's Init cmd", c.key)
		}
	}
}

// TestServersPageFilterKey (Plan 20 T10): `!` toggles the ⚠ filter through the
// real Update path — no overlay, pure list state. The seeded gpu has no Role,
// so it is ⚠: unfiltered shows the prefixed row, filtered shows the same
// single row (the multi-row filter behavior is pinned page-level in
// TestServersPageWarnSortFilter).
func TestServersPageFilterKey(t *testing.T) {
	a := newTestApp(t)
	m, cmd := a.Update(tea.KeyPressMsg{Code: '!', Text: "!"})
	got := m.(App)
	if got.overlay != nil || cmd != nil {
		t.Fatalf("! must be a pure list toggle: overlay=%v cmd=%v", got.overlay, cmd)
	}
	sp, _ := got.pages[pageServers].(*serversPage)
	if !sp.warnOnly {
		t.Fatal("! must set warnOnly")
	}
	if rows := sp.Rows(); len(rows) != 1 || rows[0] != "⚠ gpu" {
		t.Fatalf("filtered rows: %v", rows)
	}
	// footer advertises leaving the filter once it is on
	if f := got.footer(); !strings.Contains(f, "[!]显示全部") {
		t.Fatalf("footer in filter mode: %q", f)
	}
	m2, _ := m.(App).Update(tea.KeyPressMsg{Code: '!', Text: "!"})
	if sp2, _ := m2.(App).pages[pageServers].(*serversPage); sp2.warnOnly {
		t.Fatal("second ! must clear the filter")
	}
}

// TestServersWarnViewSurvivesRefetch (Plan 20 T10): the ⚠ filter and the
// warn-first order must survive a data refresh (refetchPages is what every
// actionDoneMsg / tokenIssuedMsg path funnels through).
func TestServersWarnViewSurvivesRefetch(t *testing.T) {
	a := newTestApp(t)
	// seed a second, COMPLETE server so the filter has something to hide
	// (servers carry an FK to credentials — mint the row first)
	cid, err := a.st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("p")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.st.AddServer(&models.Server{
		Name: "done", Host: "192.0.2.99", User: "u", Port: 22,
		CredentialID: cid, Role: "app", AuthMethod: models.AuthPassword,
	}); err != nil {
		t.Fatal(err)
	}
	m, _ := a.Update(tea.KeyPressMsg{Code: '!', Text: "!"})
	got := m.(App)
	got.refetchPages()
	sp, _ := got.pages[pageServers].(*serversPage)
	if !sp.warnOnly {
		t.Fatal("refetchPages dropped the warnOnly filter")
	}
	rows := sp.Rows()
	if len(rows) != 1 || rows[0] != "⚠ gpu" {
		t.Fatalf("filter+sort after refetch: %v", rows)
	}
}

// TestBrokerApp_RoleLoadFailsClosed (fix round F3): a corrupt role.json must
// surface as an error from NewBrokerApp, not silently default the App to
// standalone — matching roles' fail-closed design (a broken state guides
// `clear`; a silent downgrade would mislabel a machine and misroute the [u]
// upgrade affordance).
func TestBrokerApp_RoleLoadFailsClosed(t *testing.T) {
	vd, _ := withRoleDirs(t) // pins SSHMGR_STORE → role.json lives in vd
	if err := os.WriteFile(filepath.Join(vd, "role.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "t.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := NewBrokerApp(st); err == nil {
		t.Fatal("a corrupt role.json must fail NewBrokerApp (fail closed), got nil error")
	}
}

// TestApp_ViewUsesAltScreen pins the 2026-08-17 feedback fix: the console used
// to run inline (AltScreen unset), so each tab switch painted its frame BELOW
// the previous one and left residue in the scrollback. bubbletea v2 moved
// altscreen from a program option to a View field, so every top-level View
// must set it — including the overlay-delegated return, or opening a form
// would drop the session out of full-screen mid-flight.
func TestApp_ViewUsesAltScreen(t *testing.T) {
	a := newTestApp(t)
	if v := a.View(); !v.AltScreen {
		t.Fatal("App.View must set AltScreen (inline mode smears frames on tab switch)")
	}
	a.overlay = &secretView{title: "t", body: "b"}
	if v := a.View(); !v.AltScreen {
		t.Fatal("App.View must keep AltScreen on the overlay-delegated view")
	}
}

// TestApp_ColumnsFitTerminalWidth pins the 2026-08-17 feedback fix (内容串位):
// the list/detail join used to have zero gutter — the widest row kissed the
// detail border — and no width awareness, so long field values pushed the
// frame past the terminal edge (renderer hard-clips mid-border). With a
// WindowSizeMsg known: every display line must fit the width, and the widest
// list row must keep a 2-column gutter before the border.
func TestApp_ColumnsFitTerminalWidth(t *testing.T) {
	a := newTestApp(t)
	credID, err := a.st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("p")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.st.AddServer(&models.Server{
		Name: "NUC10-authoritative-broker", Host: "192.0.2.5", User: "allan", Port: 22,
		AuthMethod: models.AuthPassword, CredentialID: credID,
		Hardware: "NUC10 i7-10710U / 32G", Location: "客厅电视柜第三层", Role: "权威 broker",
		Services: "ssh-manager-serve:7878, docker, nginx, node-exporter",
		Caveats:  "BIOS 限功率 65%", Tags: []string{"broker", "core"},
		Description: "凭据 vault 权威端，跑 serve 服务，兼做内网跳板机和定时备份任务",
	}); err != nil {
		t.Fatal(err)
	}
	a.refetchPages()
	m2, _ := a.Update(tea.WindowSizeMsg{Width: 60})
	if m2.(App).width != 60 {
		t.Fatalf("WindowSizeMsg must be captured, width = %d", m2.(App).width)
	}
	content := m2.(App).View().Content
	for i, line := range strings.Split(content, "\n") {
		if w := lipgloss.Width(line); w > 60 {
			t.Fatalf("line %d width %d exceeds terminal width 60:\n%s", i, w, line)
		}
	}
	// bordered panels keep list and detail structurally apart — no row text
	// may touch a border wall directly
	for _, kiss := range []string{"broker╭", "broker│", "broker╰"} {
		if strings.Contains(content, kiss) {
			t.Fatalf("list row must not touch the detail border (%q):\n%s", kiss, content)
		}
	}
}

// TestServersPage_DesktopRender (2026-08-17 feedback round 3, 样张): the
// servers page renders as bordered panels sized to the terminal — the list
// PAGINATES instead of growing unbounded rows (footer can never fall off
// screen), with the built-in filter + help affordances.
func TestServersPage_DesktopRender(t *testing.T) {
	a := newTestApp(t)
	cid, err := a.st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("p")})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		if _, err := a.st.AddServer(&models.Server{
			Name: fmt.Sprintf("srv%02d", i), Host: "192.0.2.10", User: "u", Port: 22,
			AuthMethod: models.AuthPassword, CredentialID: cid, Role: "r",
		}); err != nil {
			t.Fatal(err)
		}
	}
	a.refetchPages()
	m, _ := a.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	v := m.(App).View().Content
	for i, line := range strings.Split(v, "\n") {
		if w := lipgloss.Width(line); w > 60 {
			t.Fatalf("line %d width %d exceeds 60:\n%s", i, w, line)
		}
	}
	if h := strings.Count(v, "\n") + 1; h > 20 {
		t.Fatalf("frame height %d exceeds terminal height 20 — pagination must bound it:\n%s", h, v)
	}
	if !strings.Contains(v, "服务器") {
		t.Fatalf("panel title missing:\n%s", v)
	}
	if !strings.Contains(v, "│") {
		t.Fatalf("panel borders missing:\n%s", v)
	}
}

// TestServersPage_ListFilterFlow: `/` opens the built-in filter, typing
// filters the visible rows, action letters typed while filtering must be
// consumed by the filter input (no overlay fires), Esc clears.
func TestServersPage_ListFilterFlow(t *testing.T) {
	a := newTestApp(t)
	cid, _ := a.st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("p")})
	if _, err := a.st.AddServer(&models.Server{
		Name: "nuc10", Host: "192.0.2.5", User: "allan", Port: 22,
		AuthMethod: models.AuthPassword, CredentialID: cid, Role: "broker",
	}); err != nil {
		t.Fatal(err)
	}
	a.refetchPages()
	// the desktop panel path needs the terminal size; without it the App
	// falls back to the plain columns layout where the list filter is moot.
	// App.Update has a VALUE receiver — keep the returned model.
	m0, _ := a.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	a = m0.(App)
	if a.width != 80 {
		t.Fatalf("width not captured: %d", a.width)
	}
	// both servers visible before filtering
	if v := a.View().Content; !strings.Contains(v, "gpu") || !strings.Contains(v, "nuc10") {
		t.Fatalf("pre-filter view must show both:\n%s", v)
	}
	m, _ := a.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
	// typing 'g' (a filter letter, NOT an action here) — the filter runs
	// ASYNC in bubbles: the matches arrive as FilterMatchesMsg via the
	// returned cmd, so pump it like a real runtime would.
	m1, cmd := m.(App).Update(tea.KeyPressMsg{Code: 'g', Text: "g"})
	if m1.(App).overlay != nil {
		t.Fatalf("key typed while filtering must not fire page actions")
	}
	m2 := pumpCmds(t, m1.(App), cmd)
	v := m2.View().Content
	if !strings.Contains(v, "gpu") {
		t.Fatalf("filter 'g' must keep gpu visible:\n%s", v)
	}
	if strings.Contains(v, "nuc10") {
		t.Fatalf("filter 'g' must hide nuc10:\n%s", v)
	}
	// Esc clears the filter
	m3, _ := m2.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if v := m3.(App).View().Content; !strings.Contains(v, "nuc10") {
		t.Fatalf("Esc must clear the filter:\n%s", v)
	}
}

// pumpCmds executes the cmds a real bubbletea runtime would and feeds every
// produced msg back through Update (bubbles list filtering is async: the
// matches ride a cmd → FilterMatchesMsg). Timer/cosmetic ticks (cursor blink,
// spinner) self-perpetuate in a synchronous test — drop them instead of
// chasing the schedule.
func pumpCmds(t *testing.T, a App, c tea.Cmd) App {
	t.Helper()
	if c == nil {
		return a
	}
	switch msg := c().(type) {
	case nil, cursor.BlinkMsg, spinner.TickMsg, list.FilterMatchesMsg:
		_ = msg // FilterMatchesMsg is routed by App.Update itself below
		nm, _ := a.Update(msg)
		return nm.(App)
	case tea.BatchMsg:
		for _, sub := range msg {
			a = pumpCmds(t, a, sub)
		}
		return a
	default:
		nm, cmd := a.Update(msg)
		return pumpCmds(t, nm.(App), cmd)
	}
}

// TestAllPages_PanelFit (2026-08-17 全页铺开): every broker page renders as
// bordered panels fitted to the terminal — no line wider than the terminal,
// frame never taller than the terminal (pagination bounds it), borders
// present.
func TestAllPages_PanelFit(t *testing.T) {
	a := newTestApp(t)
	cid, err := a.st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("p")})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		if _, err := a.st.AddServer(&models.Server{
			Name: fmt.Sprintf("srv%02d", i), Host: "192.0.2.10", User: "u", Port: 22,
			AuthMethod: models.AuthPassword, CredentialID: cid, Role: "r",
		}); err != nil {
			t.Fatal(err)
		}
	}
	pid, err := a.st.AddProfile("home-lab")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.st.AddProject("proj-x", pid); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.st.AddCacheToken("laptop"); err != nil {
		t.Fatal(err)
	}
	a.refetchPages()
	m, _ := a.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	app := m.(App)
	for pi := 0; pi < int(pageCount); pi++ {
		v := app.View().Content
		if !strings.Contains(v, "│") {
			t.Fatalf("page %d: panel borders missing:\n%s", pi, v)
		}
		for i, line := range strings.Split(v, "\n") {
			if w := lipgloss.Width(line); w > 60 {
				t.Fatalf("page %d line %d width %d exceeds 60:\n%s", pi, i, w, line)
			}
		}
		if h := strings.Count(v, "\n") + 1; h > 20 {
			t.Fatalf("page %d frame height %d exceeds 20:\n%s", pi, h, v)
		}
		nm, _ := app.Update(tea.KeyPressMsg{Code: tea.KeyTab})
		app = nm.(App)
	}
}
