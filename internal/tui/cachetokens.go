package tui

import (
	"fmt"

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
