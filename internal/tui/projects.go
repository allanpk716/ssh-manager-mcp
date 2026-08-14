package tui

import (
	"fmt"

	"ssh-manager-mcp/internal/models"
)

// projectsPage lists agent identities. Action keys arrive in Task 6.
type projectsPage struct {
	items  []*models.Project
	cursor int
}

func (p *projectsPage) Title() string { return "Projects" }
func (p *projectsPage) Cursor() int   { return p.cursor }
func (p *projectsPage) Select(i int)  { p.cursor = i }

func (p *projectsPage) Rows() []string {
	out := make([]string, len(p.items))
	for i, pr := range p.items {
		out[i] = pr.Name
	}
	return out
}

func (p *projectsPage) Detail() string {
	if p.cursor < 0 || p.cursor >= len(p.items) {
		return "(空)"
	}
	pr := p.items[p.cursor]
	return fmt.Sprintf("名称    %s\nID      %s\nToken   %s…\nProfile %s\n状态    %s\n创建    %s\n更新    %s",
		pr.Name, pr.ID, pr.TokenPrefix, pr.ProfileID, pr.Status,
		pr.CreatedAt.Format("2006-01-02 15:04"), pr.UpdatedAt.Format("2006-01-02 15:04"))
}

func (p *projectsPage) current() *models.Project {
	if p.cursor < 0 || p.cursor >= len(p.items) {
		return nil
	}
	return p.items[p.cursor]
}
