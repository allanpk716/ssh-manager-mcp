package snapshotdiff

import (
	"sort"
	"strings"

	"ssh-manager-mcp/internal/store"
)

// SecretSet 按名字比较当前快照里的机密项。新增、删除、换值与可写入服务
// 器列表变化分别成句;提示只使用机密项名字和服务器名,不读取机密项值。
type SecretSet struct{}

// Describe 返回机密项差异的描述分句。因 profile 裁剪而从快照消失的机密
// 项,在这里与被删除的机密项走同一个「删除」分支。
func (SecretSet) Describe(old, fresh *store.Snapshot) []string {
	oldSecrets := secretsByName(old)
	freshSecrets := secretsByName(fresh)

	var added, removed, changedValue, changedServers []string
	for name := range freshSecrets {
		if _, exists := oldSecrets[name]; !exists {
			added = append(added, name)
			continue
		}
		if freshSecrets[name].ValueVersion != oldSecrets[name].ValueVersion {
			changedValue = append(changedValue, name)
		}
		if !sameServerNameSet(allowedServerNames(old, name), allowedServerNames(fresh, name)) {
			changedServers = append(changedServers, name)
		}
	}
	for name := range oldSecrets {
		if _, exists := freshSecrets[name]; !exists {
			removed = append(removed, name)
		}
	}

	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(changedValue)
	sort.Strings(changedServers)
	var parts []string
	if len(added) > 0 {
		parts = append(parts, "新增机密项 "+strings.Join(added, "、"))
	}
	if len(removed) > 0 {
		parts = append(parts, "删除机密项 "+strings.Join(removed, "、"))
	}
	for _, name := range changedValue {
		parts = append(parts, "机密项 "+name+" 的值已更改，用到它的文件需要重新部署")
	}
	for _, name := range changedServers {
		addedServers, removedServers := serverNameDiff(allowedServerNames(old, name), allowedServerNames(fresh, name))
		details := listChangeText(addedServers, removedServers)
		parts = append(parts, "机密项 "+name+" 的可写入服务器列表已变化："+details)
	}
	return parts
}

func secretsByName(snap *store.Snapshot) map[string]store.SnapshotSecret {
	if snap == nil {
		return nil
	}
	out := make(map[string]store.SnapshotSecret, len(snap.Secrets))
	for _, secret := range snap.Secrets {
		out[secret.Name] = secret
	}
	return out
}

func allowedServerNames(snap *store.Snapshot, secretName string) []string {
	if snap == nil {
		return nil
	}
	secretIDs := make(map[string]bool, len(snap.Secrets))
	for _, secret := range snap.Secrets {
		if secret.Name == secretName {
			secretIDs[secret.ID] = true
		}
	}
	serverNames := make(map[string]string, len(snap.Servers))
	for _, server := range snap.Servers {
		serverNames[server.ID] = server.Name
	}
	var names []string
	for _, allow := range snap.SecretAllows {
		if !secretIDs[allow.SecretID] {
			continue
		}
		if name, exists := serverNames[allow.ServerID]; exists {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func sameServerNameSet(old, fresh []string) bool {
	if len(old) != len(fresh) {
		return false
	}
	oldCounts := make(map[string]int, len(old))
	for _, name := range old {
		oldCounts[name]++
	}
	for _, name := range fresh {
		oldCounts[name]--
		if oldCounts[name] < 0 {
			return false
		}
	}
	return true
}

func serverNameDiff(old, fresh []string) (added, removed []string) {
	oldCounts := make(map[string]int, len(old))
	for _, name := range old {
		oldCounts[name]++
	}
	freshCounts := make(map[string]int, len(fresh))
	for _, name := range fresh {
		freshCounts[name]++
	}
	for name, count := range freshCounts {
		if count > oldCounts[name] {
			added = append(added, name)
		}
	}
	for name, count := range oldCounts {
		if count > freshCounts[name] {
			removed = append(removed, name)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

func listChangeText(added, removed []string) string {
	var details []string
	if len(added) > 0 {
		details = append(details, "新增 "+strings.Join(added, "、"))
	}
	if len(removed) > 0 {
		details = append(details, "删除 "+strings.Join(removed, "、"))
	}
	return strings.Join(details, "、")
}
