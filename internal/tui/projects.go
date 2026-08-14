package tui

import (
	"fmt"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// projectsPage lists agent identities. Action keys (a/e/d) live in app.go.
type projectsPage struct {
	items  []*models.Project
	st     *store.Store // live view: resolves ProfileID to a readable name
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
	pn := pr.ProfileID
	if p.st != nil {
		pn = profileNameFor(p.st, pr.ProfileID)
	}
	return fmt.Sprintf("名称    %s\nID      %s\nToken   %s…\nProfile %s\n状态    %s\n创建    %s\n更新    %s",
		pr.Name, pr.ID, pr.TokenPrefix, pn, pr.Status,
		pr.CreatedAt.Format("2006-01-02 15:04"), pr.UpdatedAt.Format("2006-01-02 15:04"))
}

// profileNameFor resolves a profile id to its display name, falling back to
// the id itself when the profile is gone or the store can't be read.
func profileNameFor(st *store.Store, id string) string {
	profs, err := st.ListProfiles()
	if err != nil {
		return id
	}
	for _, p := range profs {
		if p.ID == id {
			return p.Name
		}
	}
	return id
}

func (p *projectsPage) current() *models.Project {
	if p.cursor < 0 || p.cursor >= len(p.items) {
		return nil
	}
	return p.items[p.cursor]
}
