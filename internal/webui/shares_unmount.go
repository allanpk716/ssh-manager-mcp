package webui

import (
	"net/http"
	"net/url"
	"strings"

	"ssh-manager-mcp/internal/models"
)

// ShareUnmountRequest 是一次单台卸载请求的页面输入。标识符用于任务与页
// 面匹配；登记名传给共享存储管理模块并进入审计摘要。
type ShareUnmountRequest struct {
	ShareID    string
	ShareName  string
	ServerID   string
	ServerName string
	Force      bool
}

// ShareUnmountBusyProcess 是页面上一个占用挂载点的进程，只含进程号、用户
// 与命令名，不含完整命令参数。
type ShareUnmountBusyProcess struct {
	ProcessID string
	User      string
	Command   string
}

// ShareUnmountTaskStatus 是一次单台卸载任务的状态。State 是 running 或
// done；Result 是共享存储管理模块的固定结论分类，失败时 FailureMessage 给
// 出可直接展示的说明。
type ShareUnmountTaskStatus struct {
	ID             string
	State          string
	ShareID        string
	ShareName      string
	ServerID       string
	ServerName     string
	Force          bool
	Result         string
	Notices        []string
	BusyProcesses  []ShareUnmountBusyProcess
	FailureMessage string
	Version        int64
}

// ShareUnmountRunner 启动并读取共享存储单台卸载任务。单机与权威端提供；
// 客户端不提供，卸载路由不存在。实现必须立即返回任务标识，远端操作在后
// 台执行。
type ShareUnmountRunner interface {
	StartShareUnmount(request ShareUnmountRequest, source ShareWriteSource) (string, error)
	ShareUnmountStatus(taskID string) (*ShareUnmountTaskStatus, bool)
}

// ShareUnmountEntry 是详情页单台卸载区域的一台服务器条目。
type ShareUnmountEntry struct {
	ServerID    string
	ServerName  string
	DeviceName  string
	ShellType   string
	MountPoint  string
	Windows     bool
	ConfirmPath string
}

// shareUnmountEntries 把可挂载服务器清单整理成单台卸载区域；Linux 与
// macOS 服务器提供卸载入口，Windows 服务器说明没有持久改动可清理。
func shareUnmountEntries(share *ShareRow) []ShareUnmountEntry {
	entries := make([]ShareUnmountEntry, 0, len(share.MountableServers))
	for _, server := range share.MountableServers {
		entries = append(entries, ShareUnmountEntry{
			ServerID: server.ID, ServerName: server.Name,
			DeviceName: server.DeviceName, ShellType: server.ShellType,
			MountPoint:  server.MountPoint,
			Windows:     models.NormalizeShellType(server.ShellType).IsWindows(),
			ConfirmPath: "/ui/shares/" + share.ID + "/unmount?server=" + server.ID,
		})
	}
	return entries
}

// ShareUnmountView 是单台卸载确认页与任务结果页的展示数据。任务标识有效
// 时 Task 携带轮询结果；进程重启或任务被清理后 TaskInvalid 为真，页面提示
// 重新发起。
type ShareUnmountView struct {
	Share       *ShareRow
	Server      ShareServerView
	Windows     bool
	ConfirmPath string
	Task        *ShareUnmountTaskStatus
	TaskInvalid bool
	PollPath    string
}

// handleShareUnmount 渲染单台卸载确认页、任务结果页，或启动卸载任务。
func (h *handler) handleShareUnmount(w http.ResponseWriter, r *http.Request, identifier string) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		h.handleShareUnmountPage(w, r, identifier)
	case http.MethodPost:
		h.handleShareUnmountPost(w, r, identifier)
	default:
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "单台卸载页只接受 GET 与 POST 请求。")
	}
}

// shareUnmountView 读取共享存储详情并解析目标服务器；服务器不在可挂载范
// 围内时返回 nil。
func (h *handler) shareUnmountView(r *http.Request, identifier, serverIdentifier string) (*ShareUnmountView, error) {
	detail, err := h.cfg.Shares.ShareDetail(h.requestClientInstance(r), identifier)
	if err != nil {
		return nil, err
	}
	for _, server := range detail.MountableServers {
		if server.ID == serverIdentifier || strings.EqualFold(server.Name, strings.TrimSpace(serverIdentifier)) {
			return &ShareUnmountView{
				Share: detail, Server: server,
				Windows:     models.NormalizeShellType(server.ShellType).IsWindows(),
				ConfirmPath: "/ui/shares/" + detail.ID + "/unmount?server=" + server.ID,
			}, nil
		}
	}
	return nil, nil
}

func (h *handler) handleShareUnmountPage(w http.ResponseWriter, r *http.Request, identifier string) {
	serverIdentifier := strings.TrimSpace(r.URL.Query().Get("server"))
	if serverIdentifier == "" {
		h.renderError(w, r, http.StatusBadRequest, "缺少服务器", "请从共享存储详情页选择要卸载的服务器。")
		return
	}
	view, err := h.shareUnmountView(r, identifier, serverIdentifier)
	if err != nil {
		h.renderShareReadError(w, r, err)
		return
	}
	if view == nil {
		h.renderError(w, r, http.StatusNotFound, "服务器不在可挂载范围内",
			"这台服务器不在这个共享的可挂载范围内；范围外机器请使用「检查残留」。")
		return
	}
	taskIdentifier := strings.TrimSpace(r.URL.Query().Get("task"))
	if taskIdentifier != "" {
		status, ok := h.cfg.ShareUnmountRunner.ShareUnmountStatus(taskIdentifier)
		if !ok || status == nil || status.ShareID != view.Share.ID || status.ServerID != view.Server.ID {
			view.TaskInvalid = true
		} else {
			view.Task = status
			view.PollPath = r.URL.RequestURI()
		}
	}
	title := "卸载 " + view.Share.Name + " · " + view.Server.Name
	h.renderPage(w, r, http.StatusOK, "share-unmount.tmpl", pageData{
		Title: title, ShareUnmount: view,
	})
}

// shareUnmountResultLabel 把共享存储管理模块的固定结论分类换成页面文案；
// 未知分类原样保留，便于发现新的分类没有配文案。
func shareUnmountResultLabel(result string) string {
	switch result {
	case "success":
		return "卸载完成"
	case "busy":
		return "配置已删除，挂载点被占用并保留"
	case "ssh-error":
		return "连接目标服务器或执行固定卸载命令失败"
	case "missing-sudo":
		return "服务器条目没有配置 sudo 凭据"
	case "unsupported-system":
		return "目标服务器操作系统不支持共享存储卸载"
	case "not-managed":
		return "目标服务器上没有这个共享的 sshmgr 挂载配置"
	case "lock-timeout":
		return "另一个共享存储操作正在进行"
	case "unmount-failed":
		return "卸载共享存储挂载点失败"
	case "output-invalid":
		return "固定卸载命令返回了不合法结论"
	case "remote-version-newer":
		return "目标服务器上的共享存储版本号更新，已停止清理"
	case "windows-unsupported":
		return "Windows 服务器不能执行共享存储卸载"
	default:
		return result
	}
}

// handleShareUnmountPost 校验目标服务器后启动卸载任务并跳转到任务页。服
// 务器不在可挂载范围内时不启动任务；Windows 服务器交给共享存储管理模块
// 统一拒绝并写审计。
func (h *handler) handleShareUnmountPost(w http.ResponseWriter, r *http.Request, identifier string) {
	view, err := h.shareUnmountView(r, identifier, r.PostFormValue("server_id"))
	if err != nil {
		h.renderShareReadError(w, r, err)
		return
	}
	if view == nil {
		h.renderError(w, r, http.StatusNotFound, "服务器不在可挂载范围内",
			"这台服务器不在这个共享的可挂载范围内；范围外机器请使用「检查残留」。")
		return
	}
	taskID, err := h.cfg.ShareUnmountRunner.StartShareUnmount(ShareUnmountRequest{
		ShareID: view.Share.ID, ShareName: view.Share.Name,
		ServerID: view.Server.ID, ServerName: view.Server.Name,
		Force: r.PostFormValue("force") == "1",
	}, ShareWriteSource{RemoteAddress: remoteIP(r)})
	if err != nil {
		h.logError(r, "启动共享存储单台卸载任务失败", err)
		h.renderError(w, r, http.StatusInternalServerError, "启动卸载失败", "启动卸载任务失败，请稍后重试。")
		return
	}
	h.renderUnmountTaskRedirect(w, r, view, taskID)
}

// renderUnmountTaskRedirect 跳转到带任务标识的卸载任务页。
func (h *handler) renderUnmountTaskRedirect(
	w http.ResponseWriter, r *http.Request, view *ShareUnmountView, taskID string,
) {
	target := view.ConfirmPath + "&task=" + url.QueryEscape(taskID)
	http.Redirect(w, r, target, http.StatusSeeOther)
}
