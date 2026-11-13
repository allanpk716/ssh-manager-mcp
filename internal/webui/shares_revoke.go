package webui

import (
	"net/http"
	"strings"

	"ssh-manager-mcp/internal/models"
)

// ShareRevokeEntry 是共享存储详情页撤销授权区域的一台服务器条目。
type ShareRevokeEntry struct {
	ServerID    string
	ServerName  string
	DeviceName  string
	ShellType   string
	Windows     bool
	ConfirmPath string
}

// shareRevokeEntries 根据可挂载服务器清单生成撤销授权入口。Windows 服务
// 器也提供入口：撤销只修改登记，不发起远端连接。
func shareRevokeEntries(share *ShareRow) []ShareRevokeEntry {
	if share == nil {
		return nil
	}
	entries := make([]ShareRevokeEntry, 0, len(share.MountableServers))
	for _, server := range share.MountableServers {
		entries = append(entries, ShareRevokeEntry{
			ServerID: server.ID, ServerName: server.Name, DeviceName: server.DeviceName,
			ShellType:   server.ShellType,
			Windows:     models.NormalizeShellType(server.ShellType).IsWindows(),
			ConfirmPath: shareRevokeConfirmPath(share.ID, server.ID),
		})
	}
	return entries
}

// ShareRevokeView 是单台撤销授权确认页的展示数据。
type ShareRevokeView struct {
	Share           *ShareRow
	Server          ShareServerView
	Windows         bool
	ConfirmPath     string
	RecentlyMounted bool
}

// handleShareRevoke 渲染单台撤销授权确认页；提交入口由任务视图实现后接入。
func (h *handler) handleShareRevoke(w http.ResponseWriter, r *http.Request, identifier string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "撤销授权页只接受 GET 请求。")
		return
	}
	serverIdentifier := strings.TrimSpace(r.URL.Query().Get("server"))
	if serverIdentifier == "" {
		h.renderError(w, r, http.StatusBadRequest, "缺少服务器", "请从共享存储详情页选择要撤销授权的服务器。")
		return
	}
	view, err := h.shareRevokeView(r, identifier, serverIdentifier)
	if err != nil {
		h.renderShareReadError(w, r, err)
		return
	}
	if view == nil {
		h.renderError(w, r, http.StatusNotFound, "服务器不在可挂载范围内",
			"这台服务器不在这个共享的可挂载范围内；范围外机器请使用「检查残留」。")
		return
	}
	h.renderPage(w, r, http.StatusOK, "share-revoke.tmpl", pageData{
		Title: "撤销 " + view.Share.Name + " · " + view.Server.Name, ShareRevoke: view,
	})
}

func (h *handler) shareRevokeView(
	r *http.Request, identifier, serverIdentifier string,
) (*ShareRevokeView, error) {
	detail, err := h.cfg.Shares.ShareDetail(h.requestClientInstance(r), identifier)
	if err != nil {
		return nil, err
	}
	for _, server := range detail.MountableServers {
		if server.ID == serverIdentifier || strings.EqualFold(server.Name, strings.TrimSpace(serverIdentifier)) {
			return &ShareRevokeView{
				Share: detail, Server: server,
				Windows:     models.NormalizeShellType(server.ShellType).IsWindows(),
				ConfirmPath: shareRevokeConfirmPath(detail.ID, server.ID),
			}, nil
		}
	}
	return nil, nil
}

func shareRevokeConfirmPath(shareID, serverID string) string {
	return "/ui/shares/" + shareID + "/revoke?server=" + serverID
}
