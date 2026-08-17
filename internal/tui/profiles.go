package tui

import (
	"fmt"

	"charm.land/bubbles/v2/list"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// profilesPage lists credential profiles. Action keys arrive in Task 5.
type profilesPage struct {
	items []*models.Profile
	st    *store.Store
	panelList
}

func (p *profilesPage) Title() string { return "Profiles" }

func newProfilesPage(items []*models.Profile, st *store.Store) *profilesPage {
	p := &profilesPage{items: items, st: st}
	p.panelList = newPanelList("Profiles")
	p.syncList()
	return p
}

// profileItem adapts a profile to the list panel: name, then member count +
// creation date.
type profileItem struct{ pr *models.Profile }

func (i profileItem) FilterValue() string { return i.pr.Name }
func (i profileItem) Title() string       { return i.pr.Name }
func (i profileItem) Description() string {
	return fmt.Sprintf("%d 台服务器 · 创建 %s", len(i.pr.ServerIDs), i.pr.CreatedAt.Format("2006-01-02"))
}

func (p *profilesPage) syncList() {
	items := make([]list.Item, len(p.items))
	for i, pr := range p.items {
		items[i] = profileItem{pr: pr}
	}
	p.setListItems(items, len(p.items))
}

func (p *profilesPage) Rows() []string {
	out := make([]string, len(p.items))
	for i, pr := range p.items {
		out[i] = pr.Name
	}
	return out
}

func (p *profilesPage) Detail() string {
	pr := p.current()
	if pr == nil {
		return "(空)"
	}
	count, members := len(pr.ServerIDs), joinIDs(pr.ServerIDs)
	if p.st != nil { // live view: resolve member ids to names
		if names, err := memberNames(p.st, pr.ID); err == nil {
			count, members = len(names), joinIDs(names)
		}
	}
	return fmt.Sprintf("名称     %s\nID       %s\n服务器   %d 台（%s）\n创建     %s\n更新     %s",
		pr.Name, pr.ID, count, orDash(members),
		pr.CreatedAt.Format("2006-01-02 15:04"), pr.UpdatedAt.Format("2006-01-02 15:04"))
}

// memberNames resolves the profile's member server ids to display names via an
// id→name map built from ListServers (ServerIDs alone carries only ids; a
// deleted server's stale id falls back to showing the id itself).
func memberNames(st *store.Store, profileID string) ([]string, error) {
	ids, err := st.ServersForProfile(profileID)
	if err != nil {
		return nil, err
	}
	servers, err := st.ListServers()
	if err != nil {
		return nil, err
	}
	byID := make(map[string]string, len(servers))
	for _, s := range servers {
		byID[s.ID] = s.Name
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		if n, ok := byID[id]; ok {
			out[i] = n
		} else {
			out[i] = id
		}
	}
	return out, nil
}

func joinIDs(ids []string) string {
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += ","
		}
		out += id
	}
	return out
}

func (p *profilesPage) current() *models.Profile {
	vis := p.list.VisibleItems()
	i := p.list.Index()
	if i < 0 || i >= len(vis) {
		return nil
	}
	it, ok := vis[i].(profileItem)
	if !ok {
		return nil
	}
	return it.pr
}

// Render draws the desktop-style body fitted to the terminal (shared panel
// machinery — see panels.go).
func (p *profilesPage) Render(width, height int) string {
	return renderPanel(&p.list, p.Detail(), width, height)
}
