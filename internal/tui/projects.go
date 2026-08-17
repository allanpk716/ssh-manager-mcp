package tui

import (
	"fmt"

	"charm.land/bubbles/v2/list"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// projectsPage lists agent identities. Action keys (a/e/d) live in app.go.
type projectsPage struct {
	items []*models.Project
	st    *store.Store // live view: resolves ProfileID to a readable name
	panelList
}

func (p *projectsPage) Title() string { return "Projects" }

func newProjectsPage(items []*models.Project, st *store.Store) *projectsPage {
	p := &projectsPage{items: items, st: st}
	p.panelList = newPanelList("Projects")
	p.syncList()
	return p
}

// projectItem adapts a project to the list panel: name, then status +
// creation date.
type projectItem struct{ pr *models.Project }

func (i projectItem) FilterValue() string { return i.pr.Name }
func (i projectItem) Title() string       { return i.pr.Name }
func (i projectItem) Description() string {
	return fmt.Sprintf("%s · 创建 %s", i.pr.Status, i.pr.CreatedAt.Format("2006-01-02"))
}

func (p *projectsPage) syncList() {
	items := make([]list.Item, len(p.items))
	for i, pr := range p.items {
		items[i] = projectItem{pr: pr}
	}
	p.setListItems(items, len(p.items))
}

func (p *projectsPage) Rows() []string {
	out := make([]string, len(p.items))
	for i, pr := range p.items {
		out[i] = pr.Name
	}
	return out
}

func (p *projectsPage) Detail() string {
	pr := p.current()
	if pr == nil {
		return "(空)"
	}
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
	vis := p.list.VisibleItems()
	i := p.list.Index()
	if i < 0 || i >= len(vis) {
		return nil
	}
	it, ok := vis[i].(projectItem)
	if !ok {
		return nil
	}
	return it.pr
}

// Render draws the desktop-style body fitted to the terminal (shared panel
// machinery — see panels.go).
func (p *projectsPage) Render(width, height int) string {
	return renderPanel(&p.list, p.Detail(), width, height)
}
