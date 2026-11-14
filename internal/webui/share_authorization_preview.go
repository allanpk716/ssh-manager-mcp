package webui

import (
	"net/http"
	"sort"
	"strings"
)

// secretAuthorizationDisplay 是授权预览页的展示数据。Preview 驱动按机密项
// 汇总的主表；ShareImpactRows 只在从共享存储详情页进入时填充，按共享名逐
// 行列出影响面。
type secretAuthorizationDisplay struct {
	Preview         SecretAuthorizationPreview
	ShareImpactRows []SecretAuthorizationChange
}

// authorizationPreviewDisplay 把授权差异换成页面展示数据：服务器详情页
// 的共享存储反向编辑把主表换成共享名；共享存储详情页的可挂载范围编辑保
// 留机密项主表，并追加按共享名列出的影响面；其他入口保持原样。
func (h *handler) authorizationPreviewDisplay(
	r *http.Request, preview SecretAuthorizationPreview, view, serverID, shareID string,
) (secretAuthorizationDisplay, error) {
	if view == "server-shares" && serverID != "" {
		detail, err := h.cfg.Servers.ServerDetail("", serverID)
		if err != nil || detail == nil {
			if err == nil {
				err = ErrServerNotFound
			}
			return secretAuthorizationDisplay{}, err
		}
		namesBySecret := shareNamesBySecretFromServerDetail(detail)
		preview.Changes = authorizationChangesByShareName(
			authorizationChangesForServer(preview.Changes, serverID), namesBySecret,
		)
		return secretAuthorizationDisplay{Preview: preview}, nil
	}
	if view == "share-scope" && shareID != "" {
		shares, err := h.cfg.Shares.ListShares(h.requestClientInstance(r), "", "")
		if err != nil {
			return secretAuthorizationDisplay{}, err
		}
		namesBySecret, found := shareNamesBySecretFromRows(shares, shareID)
		if !found {
			return secretAuthorizationDisplay{Preview: preview}, nil
		}
		return secretAuthorizationDisplay{
			Preview: preview, ShareImpactRows: authorizationChangesByShareName(preview.Changes, namesBySecret),
		}, nil
	}
	return secretAuthorizationDisplay{Preview: preview}, nil
}

// authorizationChangesByShareName 把按挂载密码机密项汇总的授权差异换算成
// 按共享存储名逐行列出的影响面；共享存储详情页与服务器详情页共用这一
// 份换算逻辑，不复制两份。
func authorizationChangesByShareName(
	changes []SecretAuthorizationChange, namesBySecret map[string][]string,
) []SecretAuthorizationChange {
	rows := make([]SecretAuthorizationChange, 0, len(changes))
	for _, change := range changes {
		names := namesBySecret[change.SecretID]
		if len(names) == 0 {
			rows = append(rows, change)
			continue
		}
		sort.Strings(names)
		for _, name := range names {
			rows = append(rows, SecretAuthorizationChange{
				SecretID: change.SecretID, DisplayName: name,
				Added: change.Added, Removed: change.Removed,
			})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].DisplayName < rows[j].DisplayName
	})
	return rows
}

// authorizationChangesForServer 只保留一台服务器条目上的差异，供服务器
// 详情页的共享存储反向编辑使用。
func authorizationChangesForServer(
	changes []SecretAuthorizationChange, serverID string,
) []SecretAuthorizationChange {
	rows := make([]SecretAuthorizationChange, 0, len(changes))
	for _, change := range changes {
		row := change
		row.Added = serverRefsFor(change.Added, serverID)
		row.Removed = serverRefsFor(change.Removed, serverID)
		rows = append(rows, row)
	}
	return rows
}

func serverRefsFor(refs []SecretAuthorizationServer, serverID string) []SecretAuthorizationServer {
	for index := range refs {
		if refs[index].ID == serverID {
			return []SecretAuthorizationServer{refs[index]}
		}
	}
	return nil
}

// shareNamesBySecretFromServerDetail 按挂载密码机密项汇总服务器详情页提
// 供的全部共享名。
func shareNamesBySecretFromServerDetail(detail *ServerDetailView) map[string][]string {
	namesBySecret := make(map[string][]string, len(detail.SharedStorageChoices))
	for _, choice := range detail.SharedStorageChoices {
		for _, share := range choice.SharedStorages {
			namesBySecret[choice.PasswordSecretID] = append(namesBySecret[choice.PasswordSecretID], share.Name)
		}
	}
	return namesBySecret
}

// shareNamesBySecretFromRows 按挂载密码机密项汇总全部共享名，并确认路径
// 里的共享存储确实存在；不存在时返回 false，页面回退为普通机密项展示。
func shareNamesBySecretFromRows(rows []ShareRow, shareID string) (map[string][]string, bool) {
	namesBySecret := make(map[string][]string, len(rows))
	found := false
	for _, row := range rows {
		namesBySecret[row.PasswordSecretID] = append(namesBySecret[row.PasswordSecretID], row.Name)
		if row.ID == shareID || strings.EqualFold(row.Name, strings.TrimSpace(shareID)) {
			found = true
		}
	}
	return namesBySecret, found
}
