package webui

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// ShareRevokeEntry 是共享存储详情页撤销授权区域的一台服务器条目。
type ShareRevokeEntry struct {
	ServerID    string
	ServerName  string
	DeviceName  string
	ShellType   string
	MountPoint  string
	Windows     bool
	ConfirmPath string
}

// ShareOutOfScopeRevokeEntry 是已移出可挂载范围、但最近一次现场探测或残
// 留检查登记显示远端可能仍留有挂载配置、凭据文件或挂载的服务器条目。
// Evidence 描述这份登记依据；入口只清理远端，不再修改登记。
type ShareOutOfScopeRevokeEntry struct {
	ShareRevokeEntry
	Evidence string
}

// SecretAuthorizationShareUnmount 是可挂载范围确认页里一台将移除服务器的
// 联动卸载选项。Mounted 与 Unknown 互斥：已挂载或挂载状态未知（不可达、
// 尚未探测）时提供「同时卸载」勾选；未挂载与 Windows 服务器不提供勾选。
// Selected 用于五分钟重新验证回跳后保留勾选。
type SecretAuthorizationShareUnmount struct {
	ShareID    string
	ShareName  string
	ServerID   string
	ServerName string
	Mounted    bool
	Unknown    bool
	Windows    bool
	Selected   bool
}

// ShareLatestCheckSource 由远端任务视图实现，返回一处共享存储最近一次完
// 成的现场探测或残留检查结果。
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
	// OutOfScope 表示启动任务时服务器已不在可挂载范围内；任务页据此展
	// 示“只清理远端、登记没有变化”的结论。
	OutOfScope  bool
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
	OutOfScope     bool
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
	OutOfScope       bool
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
			view.OutOfScope = status.OutOfScope
			view.PollPath = r.URL.RequestURI()
		}
	}
	if view.Server.ID == "" && !view.TaskInvalid {
		h.renderError(w, r, http.StatusNotFound, "服务器不在可挂载范围内",
			"这台服务器不在这个共享的可挂载范围内；登记显示远端仍有配置时，详情页会提供撤销授权入口，否则请使用「检查残留」。")
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
			"这台服务器不在这个共享的可挂载范围内；登记显示远端仍有配置时，详情页会提供撤销授权入口，否则请使用「检查残留」。")
		return
	}
	changePassword := r.PostFormValue("change_password") == "1"
	if view.OutOfScope && changePassword {
		h.renderError(w, r, http.StatusBadRequest, "不能同时更换共享密码",
			"这台服务器已不在可挂载范围内，这个入口只清理远端持久配置与挂载点，不修改登记或共享密码。")
		return
	}
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
		OutOfScope:  view.OutOfScope,
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

// handleShareRevokeCleanupPost 处理占用挂载后的强制清理重试。表单只提交
// 服务器标识；服务器名、系统类型与挂载点一律从登记信息回查，回查不到就
// 拒绝，避免伪造字段直接进入任务。
func (h *handler) handleShareRevokeCleanupPost(w http.ResponseWriter, r *http.Request, identifier string) {
	detail, err := h.cfg.Shares.ShareDetail(h.requestClientInstance(r), identifier)
	if err != nil {
		h.renderShareReadError(w, r, err)
		return
	}
	serverID := strings.TrimSpace(r.PostFormValue("server_id"))
	if detail.ID != r.PostFormValue("share_id") || serverID == "" {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的清理任务不存在。")
		return
	}
	server, err := h.cfg.Servers.ServerDetail(h.requestClientInstance(r), serverID)
	if err != nil {
		h.logError(r, "回查强制清理的服务器登记", err)
		h.renderError(w, r, http.StatusInternalServerError, "读取服务器登记失败",
			"读取服务器登记失败，请稍后重试。")
		return
	}
	if server == nil || server.ID != serverID {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的清理任务不存在。")
		return
	}
	shellType := models.NormalizeShellType(server.Shell)
	taskID, err := h.cfg.ShareRevokeRunner.StartShareRevoke(ShareRevokeRequest{
		ShareID: detail.ID, ShareName: detail.Name,
		ServerID: server.ID, ServerName: server.Name,
		ServerShell: string(shellType),
		MountPoint:  store.SharedStorageMountPoint(detail.Drive, detail.Name, shellType),
		Force:       r.PostFormValue("force") == "1", CleanupOnly: true,
		OutOfScope: shareServerByID(detail.MountableServers, server.ID) == nil,
		Source:     ShareWriteSource{RemoteAddress: remoteIP(r)},
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
	return h.outOfScopeShareRevokeView(r, detail, serverIdentifier)
}

// outOfScopeShareRevokeView 查找已移出可挂载范围、登记显示远端可能仍有
// 配置的服务器，并组装成撤销授权确认页；查不到时返回空值，页面按不存
// 在处理。
func (h *handler) outOfScopeShareRevokeView(
	r *http.Request, detail *ShareRow, serverIdentifier string,
) (*ShareRevokeView, error) {
	for _, entry := range h.shareOutOfScopeRevokeEntries(r, detail) {
		if entry.ServerID == serverIdentifier ||
			strings.EqualFold(entry.ServerName, strings.TrimSpace(serverIdentifier)) {
			return &ShareRevokeView{
				Share: detail,
				Server: ShareServerView{
					ID: entry.ServerID, Name: entry.ServerName, DeviceName: entry.DeviceName,
					ShellType: entry.ShellType, MountPoint: entry.MountPoint,
				},
				Windows:     entry.Windows,
				OutOfScope:  true,
				ConfirmPath: entry.ConfirmPath,
				// 已移出范围后的清理目标包含挂载点；默认勾选同时卸载，
				// owner 仍可以取消勾选只删持久配置。
				RecentlyMounted: true,
			}, nil
		}
	}
	return nil, nil
}

// shareOutOfScopeRevokeEntries 列出不在可挂载范围内、但最近一次现场探
// 测、成功挂载或残留检查留下的登记显示远端仍可能有挂载配置、凭据文件
// 或挂载的服务器条目。Windows 服务器不留持久配置，不会出现在这里；需
// 要重新检查时用详情页的「现场探测」或「检查残留」。
func (h *handler) shareOutOfScopeRevokeEntries(
	r *http.Request, detail *ShareRow,
) []ShareOutOfScopeRevokeEntry {
	if detail == nil || h.cfg.ShareLatestChecks == nil {
		return nil
	}
	latest, ok := h.cfg.ShareLatestChecks.LatestShareCheck(detail.ID)
	if !ok {
		return nil
	}
	entries := make([]ShareOutOfScopeRevokeEntry, 0)
	for _, result := range latest {
		if shareServerByID(detail.MountableServers, result.ServerID) != nil {
			continue
		}
		if result.Status != ShareCheckStatusMounted &&
			!result.Managed && !result.HasConfigEntry && !result.HasCredentialFile {
			continue
		}
		server, err := h.cfg.Servers.ServerDetail(h.requestClientInstance(r), result.ServerID)
		if err != nil || server == nil {
			continue
		}
		shellType := models.NormalizeShellType(server.Shell)
		if shellType.IsWindows() {
			continue
		}
		entry := ShareOutOfScopeRevokeEntry{
			ShareRevokeEntry: ShareRevokeEntry{
				ServerID:    server.ID,
				ServerName:  server.Name,
				DeviceName:  server.DeviceName,
				ShellType:   string(shellType),
				Windows:     false,
				MountPoint:  store.SharedStorageMountPoint(detail.Drive, detail.Name, shellType),
				ConfirmPath: shareRevokeConfirmPath(detail.ID, server.ID),
			},
			Evidence: outOfScopeRevokeEvidence(result),
		}
		entries = append(entries, entry)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].ServerName < entries[j].ServerName
	})
	return entries
}

// outOfScopeRevokeEvidence 把最近一次现场探测、成功挂载或残留检查留下
// 的登记换成人能读懂的依据说明；不包含地址、挂载账号或密码。
func outOfScopeRevokeEvidence(result ShareCheckResult) string {
	mounted := result.Status == ShareCheckStatusMounted
	switch {
	case mounted && result.Managed:
		return "最近登记：已挂载，且已有 sshmgr 挂载配置"
	case mounted:
		return "最近登记：已挂载"
	case result.HasConfigEntry && result.HasCredentialFile:
		return "最近登记：已有 sshmgr 挂载配置与凭据文件"
	case result.Managed || result.HasConfigEntry:
		return "最近登记：已有 sshmgr 挂载配置"
	case result.HasCredentialFile:
		return "最近登记：已有 sshmgr 凭据文件"
	default:
		return "最近登记：已有 sshmgr 挂载痕迹"
	}
}

func (h *handler) latestShareCheckMounted(
	shareID, serverID string, shell models.ShellType,
) bool {
	if shell.IsWindows() || h.cfg.ShareLatestChecks == nil {
		return false
	}
	results, ok := h.cfg.ShareLatestChecks.LatestShareCheck(shareID)
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

// shareFromAuthorizationReturnTo 解析授权编辑的返回地址；只有从共享存储
// 详情页进入的授权预览才提供联动卸载。
func (h *handler) shareFromAuthorizationReturnTo(
	r *http.Request, returnTo string,
) *ShareRow {
	target, err := url.Parse(safeSecretReturnPath(returnTo))
	if err != nil || target.Path == "" {
		return nil
	}
	parts := strings.Split(strings.Trim(target.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "ui" || parts[1] != "shares" || parts[2] == "" {
		return nil
	}
	detail, err := h.cfg.Shares.ShareDetail(h.requestClientInstance(r), parts[2])
	if err != nil {
		return nil
	}
	return detail
}

// shareUnmountChoicesForAuthorization 按最近一次现场探测结果生成可挂载范
// 围确认页的联动卸载选项：已挂载与状态未知的服务器提供勾选，未挂载与
// Windows 服务器只展示状态说明。
func (h *handler) shareUnmountChoicesForAuthorization(
	r *http.Request, returnTo string, preview SecretAuthorizationPreview,
	selected map[string]bool,
) []SecretAuthorizationShareUnmount {
	if !h.caps.CanUnmountShares {
		return nil
	}
	detail := h.shareFromAuthorizationReturnTo(r, returnTo)
	if detail == nil {
		return nil
	}
	var latest []ShareCheckResult
	if h.cfg.ShareLatestChecks != nil {
		latest, _ = h.cfg.ShareLatestChecks.LatestShareCheck(detail.ID)
	}
	choices := make([]SecretAuthorizationShareUnmount, 0)
	for _, change := range preview.Changes {
		if change.SecretName != detail.PasswordSecret {
			continue
		}
		for _, server := range change.Removed {
			shell := shareServerShellType(detail.MountableServers, server.ID)
			windows := models.NormalizeShellType(shell).IsWindows()
			choice := SecretAuthorizationShareUnmount{
				ShareID: detail.ID, ShareName: detail.Name,
				ServerID: server.ID, ServerName: server.Name,
				Windows: windows, Selected: selected[server.ID],
			}
			switch shareLatestCheckStatus(latest, server.ID) {
			case ShareCheckStatusMounted:
				choice.Mounted = true
			case ShareCheckStatusUnmounted, ShareCheckStatusSkipped:
			default:
				choice.Unknown = true
			}
			if windows {
				choice.Mounted = false
				choice.Unknown = false
			}
			choices = append(choices, choice)
		}
	}
	return choices
}

// shareLatestCheckStatus 返回一台服务器最近一次现场探测的状态；没有探
// 测结果时返回空串，调用方按状态未知处理。
func shareLatestCheckStatus(results []ShareCheckResult, serverID string) string {
	for _, result := range results {
		if result.ServerID == serverID {
			return result.Status
		}
	}
	return ""
}

// shareServerShellType 从共享存储当前的可挂载清单里查一台服务器的系统类
// 型；服务器不在清单时返回空串。
func shareServerShellType(servers []ShareServerView, serverID string) string {
	for _, server := range servers {
		if server.ID == serverID {
			return server.ShellType
		}
	}
	return ""
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
