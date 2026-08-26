package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/list"

	"ssh-manager-mcp/internal/models"
)

// cacheTokensPage lists device-auth codes for offline cache pulls.
// profileNames maps profileID → name for the Plan-39 binding display ("-" for
// unbound legacy codes). Action keys arrive in Task 7.
type cacheTokensPage struct {
	items        []*models.CacheToken
	profileNames map[string]string
	panelList
}

func (p *cacheTokensPage) Title() string { return "Cache Tokens" }

func newCacheTokensPage(items []*models.CacheToken, profileNames map[string]string) *cacheTokensPage {
	p := &cacheTokensPage{items: items, profileNames: profileNames}
	p.panelList = newPanelList("设备码")
	p.syncList()
	return p
}

// profileLabel renders a token's binding for display ("-" = unbound legacy).
func (p *cacheTokensPage) profileLabel(ct *models.CacheToken) string {
	if ct.ProfileID == "" {
		return "-"
	}
	if n, ok := p.profileNames[ct.ProfileID]; ok {
		return n
	}
	return "?" // bound to a profile this page build didn't see (deleted race)
}

// cacheTokenItem adapts a device code to the list panel: name, then status +
// bound profile + last pull.
type cacheTokenItem struct {
	ct    *models.CacheToken
	names map[string]string // profileID → name (binding display)
}

func (i cacheTokenItem) FilterValue() string { return i.ct.Name }
func (i cacheTokenItem) Title() string       { return i.ct.Name }

// profileLabel renders the item's binding ("-" = unbound legacy code).
func (i cacheTokenItem) profileLabel() string {
	if i.ct.ProfileID == "" {
		return "-"
	}
	if n, ok := i.names[i.ct.ProfileID]; ok {
		return n
	}
	return "?" // bound to a profile this page build didn't see (deleted race)
}

func (i cacheTokenItem) Description() string {
	lastPull := "-"
	if !i.ct.LastPullAt.IsZero() {
		lastPull = i.ct.LastPullAt.Format("2006-01-02 15:04")
	}
	return fmt.Sprintf("%s · profile %s · 最近拉取 %s", i.ct.Status, i.profileLabel(), lastPull)
}

func (p *cacheTokensPage) syncList() {
	items := make([]list.Item, len(p.items))
	for i, ct := range p.items {
		items[i] = cacheTokenItem{ct: ct, names: p.profileNames}
	}
	p.setListItems(items, len(items))
}

func (p *cacheTokensPage) Rows() []string {
	out := make([]string, len(p.items))
	for i, ct := range p.items {
		out[i] = ct.Name
	}
	return out
}

func (p *cacheTokensPage) Detail() string {
	ct := p.current()
	if ct == nil {
		return "(空)"
	}
	lastPull := "-"
	if !ct.LastPullAt.IsZero() {
		lastPull = ct.LastPullAt.Format("2006-01-02 15:04")
	}
	return fmt.Sprintf("名称    %s\nID      %s\nToken   %s…\n状态    %s\nProfile %s\n最近拉取 %s\n创建    %s\n更新    %s",
		ct.Name, ct.ID, ct.TokenPrefix, ct.Status, p.profileLabel(ct), lastPull,
		ct.CreatedAt.Format("2006-01-02 15:04"), ct.UpdatedAt.Format("2006-01-02 15:04"))
}

func (p *cacheTokensPage) current() *models.CacheToken {
	vis := p.list.VisibleItems()
	i := p.list.Index()
	if i < 0 || i >= len(vis) {
		return nil
	}
	it, ok := vis[i].(cacheTokenItem)
	if !ok {
		return nil
	}
	return it.ct
}

// Render draws the desktop-style body fitted to the terminal (shared panel
// machinery — see panels.go).
func (p *cacheTokensPage) Render(width, height int) string {
	return renderPanel(&p.list, p.Detail(), width, height)
}

// deviceCodeBody composes the one-time 设备码 view body: the code itself, the
// serve cert's SPKI fingerprint, and the ready-to-paste cache pull invocation
// with the pin embedded as "<code>:<fingerprint>" (spec §3.3 形态 A — the form
// clientops.SplitTokenPin consumes). serveURL comes from the issue form's hint field;
// it is display-only and never persisted. It is TrimSpace'd HERE (the single
// composition point) — the form only validates nonEmpty, so whitespace can
// ride in and would break the pasted command.
func deviceCodeBody(serveURL, code, fingerprint string) string {
	serveURL = strings.TrimSpace(serveURL)
	return fmt.Sprintf("设备码  %s\n\n指纹    %s\n\n在工作机上执行：\nssh-manager cache pull --url %s --token '%s:%s'",
		code, fingerprint, serveURL, code, fingerprint)
}
