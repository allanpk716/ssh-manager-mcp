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
	oldIndex := newSecretIndex(old)
	freshIndex := newSecretIndex(fresh)

	var added, removed, changedValue, changedServers []string
	for name, freshSecret := range freshIndex.byName {
		oldSecret, exists := oldIndex.byName[name]
		if !exists {
			added = append(added, name)
			continue
		}
		if freshSecret.version != oldSecret.version {
			changedValue = append(changedValue, name)
		}
		if _, _, difference := serverNameDiff(
			oldIndex.allowedServerNames(oldSecret.id), freshIndex.allowedServerNames(freshSecret.id),
		); difference {
			changedServers = append(changedServers, name)
		}
	}
	for name := range oldIndex.byName {
		if _, exists := freshIndex.byName[name]; !exists {
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
		freshSecret := freshIndex.byName[name]
		oldSecret := oldIndex.byName[name]
		addedServers, removedServers, _ := serverNameDiff(
			oldIndex.allowedServerNames(oldSecret.id), freshIndex.allowedServerNames(freshSecret.id),
		)
		details := listChangeText(addedServers, removedServers)
		parts = append(parts, "机密项 "+name+" 的可写入服务器列表已变化："+details)
	}
	return parts
}

// secretSummary 只保留比较需要的机密项信息,刻意不携带值,防止比较环节
// 意外把值带进提示。
type secretSummary struct {
	id      string
	version int64
}

// secretIndex 是一份快照里的机密项索引,并预先解析每个机密项的可写入服
// 务器名。服务器名在快照内唯一,列表按名字排序后可直接比较。
type secretIndex struct {
	byName      map[string]secretSummary
	serversByID map[string][]string
}

func newSecretIndex(snap *store.Snapshot) secretIndex {
	if snap == nil {
		return secretIndex{}
	}
	out := secretIndex{
		byName:      make(map[string]secretSummary, len(snap.Secrets)),
		serversByID: make(map[string][]string, len(snap.SecretAllows)),
	}
	for _, secret := range snap.Secrets {
		out.byName[secret.Name] = secretSummary{id: secret.ID, version: secret.ValueVersion}
	}
	serverNames := make(map[string]string, len(snap.Servers))
	for _, server := range snap.Servers {
		serverNames[server.ID] = server.Name
	}
	secretIDs := make(map[string]bool, len(out.byName))
	for _, summary := range out.byName {
		secretIDs[summary.id] = true
	}
	for _, allow := range snap.SecretAllows {
		if !secretIDs[allow.SecretID] {
			continue
		}
		if name, exists := serverNames[allow.ServerID]; exists {
			out.serversByID[allow.SecretID] = append(out.serversByID[allow.SecretID], name)
		}
	}
	for secretID := range out.serversByID {
		sort.Strings(out.serversByID[secretID])
	}
	return out
}

func (index secretIndex) allowedServerNames(secretID string) []string {
	return index.serversByID[secretID]
}

func serverNameDiff(old, fresh []string) (added, removed []string, changed bool) {
	oldSet := make(map[string]struct{}, len(old))
	freshSet := make(map[string]struct{}, len(fresh))
	for _, name := range old {
		oldSet[name] = struct{}{}
	}
	for _, name := range fresh {
		freshSet[name] = struct{}{}
	}
	for name := range freshSet {
		if _, exists := oldSet[name]; !exists {
			added = append(added, name)
		}
	}
	for name := range oldSet {
		if _, exists := freshSet[name]; !exists {
			removed = append(removed, name)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed, len(added) > 0 || len(removed) > 0
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
