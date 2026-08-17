package tui

import (
	"fmt"
	"sort"
	"strings"

	"charm.land/bubbles/v2/list"

	"ssh-manager-mcp/internal/models"
)

// serversPage lists SSH targets with the ⚠ attention view (Plan 20 T10):
// servers needing attention sort FIRST (stably) and render with a "⚠ " row
// prefix; the `!` key (warnOnly) filters the list down to exactly those. view
// maps Rows()/Detail()/current() row positions to items indices, so the
// cursor, the rows and the detail pane always agree under sort+filter. The
// cursor itself lives in the embedded panelList (desktop panel, 2026-08-17):
// view order is mirrored into the list so the panel and Rows() never
// disagree.
type serversPage struct {
	items []*models.Server
	panelList
	warnOnly bool  // `!` toggle: list only ⚠ servers
	view     []int // items indices currently listed; nil = stale (rebuild lazily)
}

func (p *serversPage) Title() string { return "服务器" }

func newServersPage(items []*models.Server) *serversPage {
	p := &serversPage{items: items}
	p.panelList = newPanelList("服务器")
	p.rebuild()
	return p
}

// serverItem adapts a server to the list: two-line rows — the ⚠-aware name,
// then user@host — with the name as filter value.
type serverItem struct{ srv *models.Server }

func (i serverItem) FilterValue() string { return i.Title() }
func (i serverItem) Title() string {
	name := i.srv.Name
	if serverNeedsAttention(i.srv) {
		name = "⚠ " + name
	}
	return name
}
func (i serverItem) Description() string {
	host := i.srv.Host
	if i.srv.Port != 0 && i.srv.Port != 22 {
		host = fmt.Sprintf("%s:%d", host, i.srv.Port)
	}
	return i.srv.User + "@" + host
}

// serverNeedsAttention is the ⚠ rule: no credential attached (Plan 20 C0
// credential-less), no role yet, or carrying the needs-passphrase tag an
// encrypted-key import left behind (both import flows write it; every
// re-credential path — the supplement loop's passphrase step, TUI edit, CLI
// edit — removes it).
func serverNeedsAttention(s *models.Server) bool {
	return s.CredentialID == "" || s.Role == "" || hasTag(s, "needs-passphrase")
}

func hasTag(s *models.Server, tag string) bool {
	for _, t := range s.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

// dropTag removes all occurrences of tag from tags (the needs-passphrase
// removal path; returns a fresh slice, never nils the input).
func dropTag(tags []string, tag string) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if t != tag {
			out = append(out, t)
		}
	}
	return out
}

// sortWarnFirst stably moves ⚠ servers to the top of items. Stable is the
// contract: the relative order inside (and below) the ⚠ block must not churn
// on every refresh.
func (p *serversPage) sortWarnFirst() {
	sort.SliceStable(p.items, func(i, j int) bool {
		return serverNeedsAttention(p.items[i]) && !serverNeedsAttention(p.items[j])
	})
}

// rebuild recomputes view (items are ⚠-sorted first, then the optional
// warnOnly filter), mirrors it into the list panel, and clamps the cursor
// into the visible range.
func (p *serversPage) rebuild() {
	p.sortWarnFirst()
	p.view = make([]int, 0, len(p.items))
	for i, s := range p.items {
		if p.warnOnly && !serverNeedsAttention(s) {
			continue
		}
		p.view = append(p.view, i)
	}
	p.syncList()
}

// syncList mirrors p.view into the list panel (order preserved, so the
// panel, Rows() and the detail pane always agree).
func (p *serversPage) syncList() {
	items := make([]list.Item, len(p.view))
	for vi, idx := range p.view {
		items[vi] = serverItem{srv: p.items[idx]}
	}
	p.setListItems(items, len(p.view))
}

// ensureView lazily rebuilds a stale view (a fresh page after FetchAll,
// before the first render).
func (p *serversPage) ensureView() {
	if p.view == nil {
		p.rebuild()
	}
}

// Rows returns display names — ⚠ servers carry a "⚠ " prefix — filtered to
// the warnOnly view when set. Tests and the fallback (unsized) layout align
// on the same view (see current).
func (p *serversPage) Rows() []string {
	p.ensureView()
	out := make([]string, len(p.view))
	for i, idx := range p.view {
		name := p.items[idx].Name
		if serverNeedsAttention(p.items[idx]) {
			name = "⚠ " + name
		}
		out[i] = name
	}
	return out
}

// Detail renders the cursor row's server — resolved through view so it stays
// in sync with the (possibly filtered) rows above.
func (p *serversPage) Detail() string {
	s := p.current()
	if s == nil {
		return "(空)"
	}
	cred := "已设置（输入新值以更换）"
	if s.CredentialID == "" {
		cred = "未设置"
	}
	return fmt.Sprintf("名称   %s\nHost   %s\n端口   %d\n用户   %s\n凭据   %s（%s）\n硬件   %s\n位置   %s\n角色   %s\n服务   %s\nCaveats %s\n标签   %s\n备注   %s",
		s.Name, s.Host, s.Port, s.User, cred, s.AuthMethod,
		orDash(s.Hardware), orDash(s.Location), orDash(s.Role), orDash(s.Services), orDash(s.Caveats),
		orDash(strings.Join(s.Tags, ",")), orDash(s.Description))
}

// current resolves the cursor to the server it points at IN THE VIEW — under
// warnOnly (and under the list's own `/` text filter) the cursor indexes the
// visible subset, not items.
func (p *serversPage) current() *models.Server {
	p.ensureView()
	vis := p.list.VisibleItems()
	i := p.list.Index()
	if i < 0 || i >= len(vis) {
		return nil
	}
	it, ok := vis[i].(serverItem)
	if !ok {
		return nil
	}
	return it.srv
}

// Render draws the desktop-style body fitted to the terminal (shared panel
// machinery — see panels.go).
func (p *serversPage) Render(width, height int) string {
	return renderPanel(&p.list, p.Detail(), width, height)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
