package servermanage

import (
	"sort"

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

// ReasonLabel 把注意原因代号换成界面文案；未知代号原样返回。
func ReasonLabel(code string) string {
	switch code {
	case ReasonNoCredential:
		return "未挂凭据"
	case ReasonNoRole:
		return "未设角色"
	case ReasonNeedsPassphrase:
		return "待补私钥口令"
	default:
		return code
	}
}

// ReasonLabels 把一组注意原因代号换成界面文案。
func ReasonLabels(codes []string) []string {
	labels := make([]string, 0, len(codes))
	for _, code := range codes {
		labels = append(labels, ReasonLabel(code))
	}
	return labels
}

// SortNeedsAttentionFirst 稳定地把需要注意的服务器排到最前。
func SortNeedsAttentionFirst(items []ListItem) []ListItem {
	sorted := append([]ListItem(nil), items...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return len(sorted[i].AttentionReasons) > 0 && len(sorted[j].AttentionReasons) == 0
	})
	return sorted
}
