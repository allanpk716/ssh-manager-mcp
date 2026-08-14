package tui

import (
	"fmt"
	"strings"

	"ssh-manager-mcp/internal/models"
)

// serversPage lists SSH targets. Action keys (a/e/d/g) arrive in Task 4.
type serversPage struct {
	items  []*models.Server
	cursor int
}

func (p *serversPage) Title() string { return "服务器" }
func (p *serversPage) Cursor() int   { return p.cursor }
func (p *serversPage) Select(i int)  { p.cursor = i }

// Rows returns bare names; the App view highlights the cursor row (so tests
// and the detail pane see clean names, no embedded cursor marker).
func (p *serversPage) Rows() []string {
	out := make([]string, len(p.items))
	for i, s := range p.items {
		out[i] = s.Name
	}
	return out
}

func (p *serversPage) Detail() string {
	if p.cursor < 0 || p.cursor >= len(p.items) {
		return "(空)"
	}
	s := p.items[p.cursor]
	cred := "已设置（输入新值以更换）"
	if s.CredentialID == "" {
		cred = "未设置"
	}
	return fmt.Sprintf("名称   %s\nHost   %s\n端口   %d\n用户   %s\n凭据   %s（%s）\n硬件   %s\n位置   %s\n角色   %s\n服务   %s\nCaveats %s\n标签   %s\n备注   %s",
		s.Name, s.Host, s.Port, s.User, cred, s.AuthMethod,
		orDash(s.Hardware), orDash(s.Location), orDash(s.Role), orDash(s.Services), orDash(s.Caveats),
		orDash(strings.Join(s.Tags, ",")), orDash(s.Description))
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (p *serversPage) current() *models.Server {
	if p.cursor < 0 || p.cursor >= len(p.items) {
		return nil
	}
	return p.items[p.cursor]
}
