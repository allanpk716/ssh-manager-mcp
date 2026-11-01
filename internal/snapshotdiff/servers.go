package snapshotdiff

import (
	"sort"
	"strings"

	"ssh-manager-mcp/internal/store"
)

// ServerSet 按服务器 ID 比较当前 profile 可见的服务器集合。授权与撤销
// 授权在客户端视角下分别就是新增与删除,所以只比较集合本身。
type ServerSet struct{}

// Describe 返回新增与删除服务器的描述分句,服务器按名称排序,提示里
// 只出现服务器名。
func (ServerSet) Describe(old, fresh *store.Snapshot) []string {
	oldServers := serversByID(old)
	freshServers := serversByID(fresh)
	var added, removed []string
	for id, sv := range freshServers {
		if _, ok := oldServers[id]; !ok {
			added = append(added, sv.Name)
		}
	}
	for id, sv := range oldServers {
		if _, ok := freshServers[id]; !ok {
			removed = append(removed, sv.Name)
		}
	}
	var parts []string
	if len(added) > 0 {
		sort.Strings(added)
		parts = append(parts, "新增服务器 "+strings.Join(added, "、"))
	}
	if len(removed) > 0 {
		sort.Strings(removed)
		parts = append(parts, "删除服务器 "+strings.Join(removed, "、"))
	}
	return parts
}

func serversByID(snap *store.Snapshot) map[string]store.SnapshotServer {
	if snap == nil {
		return nil
	}
	out := make(map[string]store.SnapshotServer, len(snap.Servers))
	for _, sv := range snap.Servers {
		out[sv.ID] = sv
	}
	return out
}
