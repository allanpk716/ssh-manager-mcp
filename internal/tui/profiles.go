package tui

import (
	"fmt"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// profilesPage lists credential profiles. Action keys arrive in Task 5.
type profilesPage struct {
	items  []*models.Profile
	st     *store.Store
	cursor int
}

func (p *profilesPage) Title() string { return "Profiles" }
func (p *profilesPage) Cursor() int   { return p.cursor }
func (p *profilesPage) Select(i int)  { p.cursor = i }

func (p *profilesPage) Rows() []string {
	out := make([]string, len(p.items))
	for i, pr := range p.items {
		out[i] = pr.Name
	}
	return out
}

func (p *profilesPage) Detail() string {
	if p.cursor < 0 || p.cursor >= len(p.items) {
		return "(空)"
	}
	pr := p.items[p.cursor]
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
	if p.cursor < 0 || p.cursor >= len(p.items) {
		return nil
	}
	return p.items[p.cursor]
}
