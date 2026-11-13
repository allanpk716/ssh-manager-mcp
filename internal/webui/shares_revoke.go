package webui

import (
	"errors"
	"io"
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

// SecretAuthorizationShareRevoke 是可挂载范围确认页里的一条「移除授权且
// 最近现场探测仍挂载」提示。
type SecretAuthorizationShareRevoke struct {
	ShareID     string
	ShareName   string
	ServerID    string
	ServerName  string
	ConfirmPath string
}

// ShareLatestCheckSource 由远端任务视图实现，返回一处共享存储最近一次完
// 成的现场探测结果。
type ShareLatestCheckSource interface {
	LatestShareCheck(shareID string) ([]ShareCheckResult, bool)
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
	// CleanupOnly 表示登记撤销已提交，只需继续完成占挂载后的强制清理。
	CleanupOnly bool
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
	State          ShareTaskState
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
	Share            *ShareRow
	Server           ShareServerView
	Windows          bool
	ConfirmPath      string
	RecentlyMounted  bool
	RequestedUnmount bool
	ChangePassword   bool
	Error            string
	Task             *ShareRevokeTaskStatus
	TaskInvalid      bool
	PollPath         string
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
	view.RequestedUnmount = r.URL.Query().Get("unmount") == "1"
	view.ChangePassword = r.URL.Query().Get("change_password") == "1"
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
	if r.PostFormValue("retry_cleanup") == "1" {
		h.handleShareRevokeCleanupPost(w, r, identifier)
		return
	}
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
	changePassword := r.PostFormValue("change_password") == "1"
	if changePassword && h.cfg.Role == RoleServer && !h.recentlyVerified(h.currentSession(r)) {
		resume := url.Values{"change_password": {"1"}}
		if r.PostFormValue("unmount") == "1" {
			resume.Set("unmount", "1")
		}
		next := shareRevokeConfirmPath(view.Share.ID, view.Server.ID) + "&" + resume.Encode()
		http.Redirect(w, r, reverifyPath(next), http.StatusSeeOther)
		return
	}
	var newPassword []byte
	if changePassword {
		password, passwordErr := shareRevokePasswordFromRequest(r)
		if passwordErr != nil {
			view.RequestedUnmount = r.PostFormValue("unmount") == "1"
			view.ChangePassword = true
			view.Error = passwordErr.Error()
			h.renderPage(w, r, http.StatusBadRequest, "share-revoke.tmpl", pageData{
				Title: "撤销 " + view.Share.Name + " · " + view.Server.Name, ShareRevoke: view,
			})
			return
		}
		newPassword = password
	}
	taskID, err := h.cfg.ShareRevokeRunner.StartShareRevoke(ShareRevokeRequest{
		ShareID: view.Share.ID, ShareName: view.Share.Name,
		ServerID: view.Server.ID, ServerName: view.Server.Name,
		ServerShell: view.Server.ShellType, MountPoint: view.Server.MountPoint,
		Unmount:     r.PostFormValue("unmount") == "1",
		Force:       r.PostFormValue("force") == "1",
		NewPassword: newPassword,
		Source:      ShareWriteSource{RemoteAddress: remoteIP(r)},
	})
	if err != nil {
		h.logError(r, "启动共享存储撤销授权任务失败", err)
		h.renderError(w, r, http.StatusInternalServerError, "启动撤销授权失败",
			"启动撤销授权任务失败，请稍后重试。")
		return
	}
	http.Redirect(w, r, view.ConfirmPath+"&task="+url.QueryEscape(taskID), http.StatusSeeOther)
}

func (h *handler) handleShareRevokeCleanupPost(w http.ResponseWriter, r *http.Request, identifier string) {
	detail, err := h.cfg.Shares.ShareDetail(h.requestClientInstance(r), identifier)
	if err != nil {
		h.renderShareReadError(w, r, err)
		return
	}
	if detail.ID != r.PostFormValue("share_id") || r.PostFormValue("server_id") == "" {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的清理任务不存在。")
		return
	}
	taskID, err := h.cfg.ShareRevokeRunner.StartShareRevoke(ShareRevokeRequest{
		ShareID: detail.ID, ShareName: detail.Name,
		ServerID: r.PostFormValue("server_id"), ServerName: r.PostFormValue("server_name"),
		ServerShell: r.PostFormValue("server_shell"), MountPoint: r.PostFormValue("mount_point"),
		Force: r.PostFormValue("force") == "1", CleanupOnly: true,
		Source: ShareWriteSource{RemoteAddress: remoteIP(r)},
	})
	if err != nil {
		h.logError(r, "启动共享存储撤销授权清理任务失败", err)
		h.renderError(w, r, http.StatusInternalServerError, "启动强制清理失败",
			"启动强制清理任务失败，请稍后重试。")
		return
	}
	path := shareRevokeConfirmPath(detail.ID, r.PostFormValue("server_id")) +
		"&task=" + url.QueryEscape(taskID)
	http.Redirect(w, r, path, http.StatusSeeOther)
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
				RecentlyMounted: h.latestShareCheckMounted(
					detail.ID, server.ID, models.NormalizeShellType(server.ShellType),
				),
			}, nil
		}
	}
	return nil, nil
}

func (h *handler) latestShareCheckMounted(
	shareID, serverID string, shell models.ShellType,
) bool {
	if shell.IsWindows() || h.cfg.SharesRemoteTasks == nil {
		return false
	}
	source, ok := h.cfg.SharesRemoteTasks.(ShareLatestCheckSource)
	if !ok {
		return false
	}
	results, ok := source.LatestShareCheck(shareID)
	if !ok {
		return false
	}
	for _, result := range results {
		if result.ServerID == serverID && result.Status == ShareCheckStatusMounted {
			return true
		}
	}
	return false
}

func (h *handler) shareRevokeChoicesForAuthorization(
	r *http.Request, returnTo string, preview SecretAuthorizationPreview,
) []SecretAuthorizationShareRevoke {
	if !h.caps.CanRevokeShares || h.cfg.SharesRemoteTasks == nil {
		return nil
	}
	target, err := url.Parse(safeSecretReturnPath(returnTo))
	if err != nil || target.Path == "" {
		return nil
	}
	parts := strings.Split(strings.Trim(target.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "ui" || parts[1] != "shares" || parts[2] == "" {
		return nil
	}
	detail, readErr := h.cfg.Shares.ShareDetail(h.requestClientInstance(r), parts[2])
	if readErr != nil {
		return nil
	}
	choices := make([]SecretAuthorizationShareRevoke, 0)
	for _, change := range preview.Changes {
		if change.SecretName != detail.PasswordSecret {
			continue
		}
		for _, server := range change.Removed {
			if !h.latestShareCheckMounted(detail.ID, server.ID, models.ShellType("")) {
				continue
			}
			choices = append(choices, SecretAuthorizationShareRevoke{
				ShareID: detail.ID, ShareName: detail.Name,
				ServerID: server.ID, ServerName: server.Name,
				ConfirmPath: shareRevokeConfirmPath(detail.ID, server.ID) + "&unmount=1",
			})
		}
	}
	return choices
}

func shareRevokeConfirmPath(shareID, serverID string) string {
	return "/ui/shares/" + shareID + "/revoke?server=" + serverID
}

func isShareRevokePasswordUpload(r *http.Request) bool {
	return r != nil && r.Method == http.MethodPost &&
		strings.HasPrefix(r.URL.Path, sharesPath) && strings.HasSuffix(r.URL.Path, "/revoke")
}

func shareRevokePasswordFromRequest(r *http.Request) ([]byte, error) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if r.MultipartForm == nil {
			if err := r.ParseMultipartForm(secretUploadMaxRequestBody); err != nil {
				return nil, errors.New("读取表单失败，请重试")
			}
		}
		file, header, err := r.FormFile("new_password_file")
		if err == nil {
			defer file.Close()
			if r.PostFormValue("new_password") != "" {
				return nil, errors.New("隐藏输入框与上传文件不能同时提供")
			}
			if header.Size > secretValueLimit {
				return nil, errSecretUploadTooLarge
			}
			value, readErr := io.ReadAll(io.LimitReader(file, secretValueLimit+1))
			if readErr != nil {
				return nil, errors.New("读取上传文件失败，请重试")
			}
			if len(value) > secretValueLimit {
				return nil, errSecretUploadTooLarge
			}
			return value, nil
		}
	}
	password := []byte(r.PostFormValue("new_password"))
	if len(password) == 0 {
		return nil, errors.New("请输入新的共享密码，或上传按字节原样保存的文件")
	}
	return password, nil
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
