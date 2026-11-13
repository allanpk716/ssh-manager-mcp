package webui

import (
	"net/http"
	"net/url"
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

// ShareRevokeRequest 是一次单台撤销授权请求。NewPassword 只存在于启动请
// 求与后台调用参数里，不进入任务状态与页面结果。
type ShareRevokeRequest struct {
	ShareID     string
	ShareName   string
	ServerID    string
	ServerName  string
	ServerShell string
	MountPoint  string
	Unmount     bool
	Force       bool
	NewPassword []byte
	Source      ShareWriteSource
}

// ShareRevokeBusyProcess 是占用挂载点的一个进程，只含页面允许展示的进程
// 号、用户与命令名。
type ShareRevokeBusyProcess struct {
	ProcessID string
	User      string
	Command   string
}

// ShareRevokeTaskState 是撤销授权任务状态代号。
const (
	ShareRevokeTaskRunning = "running"
	ShareRevokeTaskDone    = "done"
	ShareRevokeTaskError   = "error"
)

// ShareRevokeResult 是撤销授权任务完成后的固定结论。
type ShareRevokeResult struct {
	Result                string
	Version               int64
	Unmount               bool
	Force                 bool
	MountPreserved        bool
	PasswordChanged       bool
	RemainingMountServers []string
	BusyProcesses         []ShareRevokeBusyProcess
	Notices               []string
}

// ShareRevokeTaskStatus 是进程内撤销授权任务的状态。任务只保存在当前
// Web UI 进程里，进程重启后不排队补跑。
type ShareRevokeTaskStatus struct {
	ID             string
	State          string
	ShareID        string
	ShareName      string
	ServerID       string
	ServerName     string
	ServerShell    string
	MountPoint     string
	FailureMessage string
	Result         *ShareRevokeResult
}

// ShareRevokeRunner 启动并读取共享存储单台撤销授权任务。
type ShareRevokeRunner interface {
	StartShareRevoke(request ShareRevokeRequest) (string, error)
	ShareRevokeStatus(taskID string) (*ShareRevokeTaskStatus, bool)
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
	Task            *ShareRevokeTaskStatus
	TaskInvalid     bool
	PollPath        string
}

// handleShareRevoke 渲染单台撤销授权确认页；提交入口由任务视图实现后接入。
func (h *handler) handleShareRevoke(w http.ResponseWriter, r *http.Request, identifier string) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		h.handleShareRevokePage(w, r, identifier)
	case http.MethodPost:
		h.handleShareRevokePost(w, r, identifier)
	default:
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "撤销授权页只接受 GET 与 POST 请求。")
	}
}

func (h *handler) handleShareRevokePage(w http.ResponseWriter, r *http.Request, identifier string) {
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
	detail, err := h.cfg.Shares.ShareDetail(h.requestClientInstance(r), identifier)
	if err != nil {
		h.renderShareReadError(w, r, err)
		return
	}
	if view == nil {
		view = &ShareRevokeView{Share: detail}
	}
	taskIdentifier := strings.TrimSpace(r.URL.Query().Get("task"))
	if taskIdentifier != "" {
		status, ok := h.cfg.ShareRevokeRunner.ShareRevokeStatus(taskIdentifier)
		if !ok || status == nil || status.ShareID != detail.ID ||
			status.ServerID != serverIdentifier {
			view.TaskInvalid = true
		} else {
			view.Task = status
			view.Server = ShareServerView{
				ID: status.ServerID, Name: status.ServerName,
				ShellType: status.ServerShell, MountPoint: status.MountPoint,
			}
			view.Windows = models.NormalizeShellType(status.ServerShell).IsWindows()
			view.PollPath = r.URL.RequestURI()
		}
	}
	if view.Server.ID == "" && !view.TaskInvalid {
		h.renderError(w, r, http.StatusNotFound, "服务器不在可挂载范围内",
			"这台服务器不在这个共享的可挂载范围内；范围外机器请使用「检查残留」。")
		return
	}
	if view.ConfirmPath == "" {
		view.ConfirmPath = shareRevokeConfirmPath(detail.ID, serverIdentifier)
	}
	titleServer := view.Server.Name
	if titleServer == "" {
		titleServer = serverIdentifier
	}
	h.renderPage(w, r, http.StatusOK, "share-revoke.tmpl", pageData{
		Title: "撤销 " + view.Share.Name + " · " + titleServer, ShareRevoke: view,
	})
}

func (h *handler) handleShareRevokePost(w http.ResponseWriter, r *http.Request, identifier string) {
	view, err := h.shareRevokeView(r, identifier, r.PostFormValue("server_id"))
	if err != nil {
		h.renderShareReadError(w, r, err)
		return
	}
	if view == nil {
		h.renderError(w, r, http.StatusNotFound, "服务器不在可挂载范围内",
			"这台服务器不在这个共享的可挂载范围内；范围外机器请使用「检查残留」。")
		return
	}
	taskID, err := h.cfg.ShareRevokeRunner.StartShareRevoke(ShareRevokeRequest{
		ShareID: view.Share.ID, ShareName: view.Share.Name,
		ServerID: view.Server.ID, ServerName: view.Server.Name,
		ServerShell: view.Server.ShellType, MountPoint: view.Server.MountPoint,
		Unmount: r.PostFormValue("unmount") == "1",
		Force:   r.PostFormValue("force") == "1",
		Source:  ShareWriteSource{RemoteAddress: remoteIP(r)},
	})
	if err != nil {
		h.logError(r, "启动共享存储撤销授权任务失败", err)
		h.renderError(w, r, http.StatusInternalServerError, "启动撤销授权失败",
			"启动撤销授权任务失败，请稍后重试。")
		return
	}
	http.Redirect(w, r, view.ConfirmPath+"&task="+url.QueryEscape(taskID), http.StatusSeeOther)
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

// shareRevokeResultLabel 把共享存储管理模块的固定结论分类换成页面文案；
// 未知分类原样保留，便于发现新的分类没有配文案。
func shareRevokeResultLabel(result string) string {
	switch result {
	case "success":
		return "撤销授权完成"
	case "busy":
		return "配置已删除，挂载点被占用并保留"
	case "ssh-error":
		return "连接目标服务器或执行固定撤销命令失败"
	case "missing-sudo":
		return "服务器条目没有配置 sudo 凭据"
	case "unsupported-system":
		return "目标服务器操作系统不支持共享存储撤销"
	case "not-managed":
		return "目标服务器上没有这个共享的 sshmgr 挂载配置"
	case "lock-timeout":
		return "另一个共享存储操作正在进行"
	case "unmount-failed":
		return "卸载共享存储挂载点失败"
	case "output-invalid":
		return "固定撤销命令返回了不合法结论"
	case "remote-version-newer":
		return "目标服务器上的共享存储版本号更新，已停止清理"
	default:
		return result
	}
}
