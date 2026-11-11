package snapshotdiff

import (
	"fmt"
	"sort"
	"strings"

	"ssh-manager-mcp/internal/store"
)

// SharedStorageSet 按登记标识符比较快照里的共享存储。新增、删除或不再
// 可见、服务器被移出可挂载范围、版本号变化分别成句；提示只使用共享名、
// 共享存储版本号、变更说明与服务器名，不读取挂载账号用户名、地址或密码。
type SharedStorageSet struct{}

// Describe 返回共享存储差异的描述分句。profile 视角下可挂载范围与授权
// 的交集变空时，共享存储不再随快照下发，在这里与被删除的登记走同一个
// 「删除或不再可见」分支。
func (SharedStorageSet) Describe(old, fresh *store.Snapshot) []string {
	oldShares := sharedStoragesByID(old)
	freshShares := sharedStoragesByID(fresh)

	var added, removed, changed []string
	changedDetails := make(map[string]string)
	for _, freshShare := range freshShares {
		oldShare, exists := oldShares[freshShare.id]
		if exists {
			revoked := sharedStorageRevokedServers(old, fresh, oldShare, freshShare)
			if freshShare.version != oldShare.version || len(revoked) > 0 {
				changed = append(changed, freshShare.name)
				var details []string
				if len(revoked) > 0 {
					details = append(details, fmt.Sprintf(
						"共享存储 %s 已从服务器 %s 撤销", freshShare.name, strings.Join(revoked, "、"),
					))
				}
				if freshShare.version != oldShare.version {
					details = append(details, sharedStorageVersionText(
						oldShare, freshShare,
						sharedStorageMountableServers(fresh, freshShare),
					))
				}
				changedDetails[freshShare.name] = strings.Join(details, "；")
			}
			continue
		}
		added = append(added, fmt.Sprintf("新增共享存储 %s（版本 %d）", freshShare.name, freshShare.version))
	}
	for _, oldShare := range oldShares {
		if _, exists := freshShares[oldShare.id]; exists {
			continue
		}
		removed = append(removed, "共享存储 "+oldShare.name+" 已删除或不再可见")
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(changed)
	parts := append(added, removed...)
	for _, name := range changed {
		parts = append(parts, changedDetails[name])
	}
	return parts
}

// sharedStorageVersionText 生成版本号变化的分句。可提醒的服务器名取新快
// 照仍可挂载的服务器：被移出的服务器由撤销分句单独说明，不再列入「可能
// 需要更新」；没有变更说明或没有可确定的服务器时省略对应子句。
func sharedStorageVersionText(
	oldShare, freshShare sharedStorageSummary,
	freshServers []string,
) string {
	var text strings.Builder
	fmt.Fprintf(&text, "共享存储 %s 版本 %d → %d", freshShare.name, oldShare.version, freshShare.version)
	if freshShare.changeNote != "" {
		text.WriteString("，说明：" + freshShare.changeNote)
	}
	servers := append([]string(nil), freshServers...)
	sort.Strings(servers)
	if len(servers) > 0 {
		text.WriteString("；以下服务器上的挂载可能需要更新：" + strings.Join(servers, "、"))
	}
	return text.String()
}

// sharedStorageRevokedServers 返回仍在新快照可见、但已不能挂载某处共享
// 存储的服务器名。服务器条目本身消失的情况不属于撤销，由服务器集合比较
// 项负责说明。
func sharedStorageRevokedServers(old, fresh *store.Snapshot, oldShare, freshShare sharedStorageSummary) []string {
	oldServers := sharedStorageMountableServers(old, oldShare)
	freshServers := sharedStorageMountableServers(fresh, freshShare)
	if len(oldServers) == 0 {
		return nil
	}
	freshSet := make(map[string]struct{}, len(freshServers))
	for _, name := range freshServers {
		freshSet[name] = struct{}{}
	}
	visible := make(map[string]struct{}, len(fresh.Servers))
	for _, server := range fresh.Servers {
		visible[server.Name] = struct{}{}
	}
	var revoked []string
	for _, name := range oldServers {
		if _, stillAllowed := freshSet[name]; stillAllowed {
			continue
		}
		if _, stillVisible := visible[name]; stillVisible {
			revoked = append(revoked, name)
		}
	}
	sort.Strings(revoked)
	return revoked
}

// sharedStorageMountableServers 返回这份快照里某处共享存储可挂载的服务器
// 名：挂载密码机密项的可写入服务器列表与当前快照可见的服务器取交集。
func sharedStorageMountableServers(snap *store.Snapshot, share sharedStorageSummary) []string {
	if snap == nil || share.passwordSecretID == "" {
		return nil
	}
	visibleNames := make(map[string]string, len(snap.Servers))
	for _, server := range snap.Servers {
		visibleNames[server.ID] = server.Name
	}
	out := make(map[string]struct{})
	for _, allow := range snap.SecretAllows {
		if allow.SecretID != share.passwordSecretID {
			continue
		}
		if name, visible := visibleNames[allow.ServerID]; visible {
			out[name] = struct{}{}
		}
	}
	names := make([]string, 0, len(out))
	for name := range out {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// sharedStorageSummary 只保留比较需要的共享存储信息，刻意不携带挂载账号
// 用户名等其余登记字段，防止比较环节意外把它们带进提示。
type sharedStorageSummary struct {
	id               string
	name             string
	version          int64
	changeNote       string
	passwordSecretID string
}

func sharedStoragesByID(snap *store.Snapshot) map[string]sharedStorageSummary {
	if snap == nil {
		return nil
	}
	out := make(map[string]sharedStorageSummary, len(snap.SharedStorages))
	for _, share := range snap.SharedStorages {
		out[share.ID] = sharedStorageSummary{
			id:               share.ID,
			name:             share.Name,
			version:          share.Version,
			changeNote:       share.ChangeNote,
			passwordSecretID: share.PasswordSecretID,
		}
	}
	return out
}
