package tui

import (
	"fmt"
	"strings"

	"ssh-manager-mcp/internal/models"
)

// cacheTokensPage lists device-auth codes for offline cache pulls.
// Action keys arrive in Task 7.
type cacheTokensPage struct {
	items  []*models.CacheToken
	cursor int
}

func (p *cacheTokensPage) Title() string { return "Cache Tokens" }
func (p *cacheTokensPage) Cursor() int   { return p.cursor }
func (p *cacheTokensPage) Select(i int)  { p.cursor = i }

func (p *cacheTokensPage) Rows() []string {
	out := make([]string, len(p.items))
	for i, ct := range p.items {
		out[i] = ct.Name
	}
	return out
}

func (p *cacheTokensPage) Detail() string {
	if p.cursor < 0 || p.cursor >= len(p.items) {
		return "(空)"
	}
	ct := p.items[p.cursor]
	lastPull := "-"
	if !ct.LastPullAt.IsZero() {
		lastPull = ct.LastPullAt.Format("2006-01-02 15:04")
	}
	return fmt.Sprintf("名称    %s\nID      %s\nToken   %s…\n状态    %s\n最近拉取 %s\n创建    %s\n更新    %s",
		ct.Name, ct.ID, ct.TokenPrefix, ct.Status, lastPull,
		ct.CreatedAt.Format("2006-01-02 15:04"), ct.UpdatedAt.Format("2006-01-02 15:04"))
}

func (p *cacheTokensPage) current() *models.CacheToken {
	if p.cursor < 0 || p.cursor >= len(p.items) {
		return nil
	}
	return p.items[p.cursor]
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
