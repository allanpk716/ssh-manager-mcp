package tui

import (
	"fmt"
	"sort"
	"strings"

	"ssh-manager-mcp/internal/models"
)

// serversPage lists SSH targets with the ⚠ attention view (Plan 20 T10):
// servers needing attention sort FIRST (stable) and render with a "⚠ " row
// prefix; the `!` key (warnOnly) filters the list down to exactly those. view
// maps Rows()/Detail()/current() row positions to items indices, so the
// cursor, the rows and the detail pane always agree under sort+filter.
type serversPage struct {
	items    []*models.Server
	cursor   int
	warnOnly bool  // `!` toggle: list only ⚠ servers
	view     []int // items indices currently listed; nil = stale (rebuild lazily)
}

func (p *serversPage) Title() string { return "服务器" }
func (p *serversPage) Cursor() int   { return p.cursor }
func (p *serversPage) Select(i int)  { p.cursor = i }

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

// dropTag returns tags minus one occurrence of tag (the needs-passphrase
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
// warnOnly filter) and clamps the cursor into the visible range.
func (p *serversPage) rebuild() {
	p.sortWarnFirst()
	p.view = make([]int, 0, len(p.items))
	for i, s := range p.items {
		if p.warnOnly && !serverNeedsAttention(s) {
			continue
		}
		p.view = append(p.view, i)
	}
	if p.cursor >= len(p.view) {
		p.cursor = len(p.view) - 1
	}
	if p.cursor < 0 {
		p.cursor = 0
	}
}

// ensureView lazily rebuilds a stale view (a fresh page after FetchAll,
// before the first render).
func (p *serversPage) ensureView() {
	if p.view == nil {
		p.rebuild()
	}
}

// Rows returns display names — ⚠ servers carry a "⚠ " prefix — filtered to
// the warnOnly view when set. The App view highlights the cursor row; tests
// and the detail pane align on the same view (see current).
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
// warnOnly the cursor indexes the filtered list, not items.
func (p *serversPage) current() *models.Server {
	p.ensureView()
	if p.cursor < 0 || p.cursor >= len(p.view) {
		return nil
	}
	return p.items[p.view[p.cursor]]
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
