package servermanage

import (
	"ssh-manager-mcp/internal/models"
)

// 注意原因代号转用 models 里的统一取值，三处入口（终端图形界面、命令行、
// Web UI）共用同一份判断。
const (
	ReasonNoCredential    = models.ReasonNoCredential
	ReasonNoRole          = models.ReasonNoRole
	ReasonNeedsPassphrase = models.ReasonNeedsPassphrase
)

// ListItem 是列表里的一台服务器：完整条目加当前注意原因。
type ListItem struct {
	Server           *models.Server
	AttentionReasons []string
}

// List 返回服务器列表。attentionOnly 为真时只返回需要注意的服务器；顺序
// 沿用存储层按名字排序的结果，置顶展示由界面层负责。
func (m *Manager) List(attentionOnly bool) ([]ListItem, error) {
	servers, err := m.st.ListServers()
	if err != nil {
		return nil, err
	}
	out := make([]ListItem, 0, len(servers))
	for _, srv := range servers {
		reasons := models.ServerAttentionReasons(srv.CredentialID, srv.Role, srv.Tags)
		if attentionOnly && len(reasons) == 0 {
			continue
		}
		out = append(out, ListItem{Server: srv, AttentionReasons: reasons})
	}
	return out, nil
}
