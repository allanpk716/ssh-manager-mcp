package webui

import (
	"net/http"
	"net/url"
	"strings"

	"ssh-manager-mcp/internal/models"
)

// ShareReselectRequest 是一次重选地址请求的页面输入。标识符用于任务与页
// 面匹配；登记名传给共享存储管理模块并进入审计摘要。
type ShareReselectRequest struct {
	ShareID    string
	ShareName  string
	ServerID   string
	ServerName string
	Force      bool
}

// ShareReselectBusyProcess 是页面上一个占用挂载点的进程，只含进程号、用
// 户与命令名，不含完整命令参数。
type ShareReselectBusyProcess struct {
	ProcessID string
	User      string
	Command   string
}

// ShareReselectTaskStatus 是一次重选地址任务的状态。State 是 running 或
// done；Result 是共享存储管理模块的固定结论分类，失败时 FailureMessage
// 给出可直接展示的说明。
type ShareReselectTaskStatus struct {
	ID              string
	State           string
	ShareID         string
	ShareName       string
	ServerID        string
	ServerName      string
	Force           bool
	Result          string
	Notices         []string
	BusyProcesses   []ShareReselectBusyProcess
	FailureMessage  string
	Version         int64
	NetworkName     string
	NetworkSequence int
	Physical        bool
}

// ShareReselectRunner 启动并读取共享存储重选地址任务。单机与权威端提供；
// 客户端不提供，重选路由不存在。实现必须立即返回任务标识，远端操作在
// 后台执行。
type ShareReselectRunner interface {
	StartShareReselect(request ShareReselectRequest, source ShareWriteSource) (string, error)
	ShareReselectStatus(taskID string) (*ShareReselectTaskStatus, bool)
}

// ShareReselectEntry 是详情页重选地址区域的一台服务器条目。
type ShareReselectEntry struct {
	ServerID    string
	ServerName  string
	DeviceName  string
	ShellType   string
	MountPoint  string
	Windows     bool
	ConfirmPath string
}

// shareReselectEntries 把可挂载服务器清单整理成重选地址区域；Linux 与
// macOS 服务器提供重选入口，Windows 服务器说明每条命令执行前本来就会重
// 新选址。
func shareReselectEntries(share *ShareRow) []ShareReselectEntry {
	entries := make([]ShareReselectEntry, 0, len(share.MountableServers))
	for _, server := range share.MountableServers {
		entries = append(entries, ShareReselectEntry{
			ServerID: server.ID, ServerName: server.Name,
			DeviceName: server.DeviceName, ShellType: server.ShellType,
			MountPoint:  server.MountPoint,
			Windows:     models.NormalizeShellType(server.ShellType).IsWindows(),
			ConfirmPath: shareReselectConfirmPath(share.ID, server.ID),
		})
	}
	return entries
}

// ShareReselectView 是重选地址确认页与任务结果页的展示数据。任务标识有
// 效时 Task 携带轮询结果；进程重启或任务被清理后 TaskInvalid 为真，页面
// 提示重新发起。
type ShareReselectView struct {
	Share       *ShareRow
	Server      ShareServerView
	Windows     bool
	ConfirmPath string
	Task        *ShareReselectTaskStatus
	TaskInvalid bool
	PollPath    string
}

// handleShareReselect 渲染重选地址确认页、任务结果页，或启动重选任务。
func (h *handler) handleShareReselect(w http.ResponseWriter, r *http.Request, identifier string) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		h.handleShareReselectPage(w, r, identifier)
	case http.MethodPost:
		h.handleShareReselectPost(w, r, identifier)
	default:
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "重选地址页只接受 GET 与 POST 请求。")
	}
}

// shareReselectView 读取共享存储详情并解析目标服务器；服务器不在可挂载
// 范围内时返回 nil。
func (h *handler) shareReselectView(
	r *http.Request, identifier, serverIdentifier string,
) (*ShareReselectView, error) {
	detail, err := h.cfg.Shares.ShareDetail(h.requestClientInstance(r), identifier)
	if err != nil {
		return nil, err
	}
	for _, server := range detail.MountableServers {
		if server.ID == serverIdentifier || strings.EqualFold(server.Name, strings.TrimSpace(serverIdentifier)) {
			return &ShareReselectView{
				Share: detail, Server: server,
				Windows:     models.NormalizeShellType(server.ShellType).IsWindows(),
				ConfirmPath: shareReselectConfirmPath(detail.ID, server.ID),
			}, nil
		}
	}
	return nil, nil
}

func (h *handler) handleShareReselectPage(w http.ResponseWriter, r *http.Request, identifier string) {
	serverIdentifier := strings.TrimSpace(r.URL.Query().Get("server"))
	if serverIdentifier == "" {
		h.renderError(w, r, http.StatusBadRequest, "缺少服务器", "请从共享存储详情页选择要重选地址的服务器。")
		return
	}
	view, err := h.shareReselectView(r, identifier, serverIdentifier)
	if err != nil {
		h.renderShareReadError(w, r, err)
		return
	}
	if view == nil {
		h.renderError(w, r, http.StatusNotFound, "服务器不在可挂载范围内",
			"这台服务器不在这个共享的可挂载范围内；重选地址只面向仍可挂载的服务器。")
		return
	}
	taskIdentifier := strings.TrimSpace(r.URL.Query().Get("task"))
	if taskIdentifier != "" {
		status, ok := h.cfg.ShareReselectRunner.ShareReselectStatus(taskIdentifier)
		if !ok || status == nil || status.ShareID != view.Share.ID || status.ServerID != view.Server.ID {
			view.TaskInvalid = true
		} else {
			view.Task = status
			view.PollPath = r.URL.RequestURI()
		}
	}
	title := "重选地址 " + view.Share.Name + " · " + view.Server.Name
	h.renderPage(w, r, http.StatusOK, "share-reselect.tmpl", pageData{
		Title: title, ShareReselect: view,
	})
}

// handleShareReselectPost 校验目标服务器后启动重选任务并跳转到任务页。
// 服务器不在可挂载范围内时不启动任务；Windows 服务器交给共享存储管理模
// 块统一拒绝并写审计。
func (h *handler) handleShareReselectPost(w http.ResponseWriter, r *http.Request, identifier string) {
	view, err := h.shareReselectView(r, identifier, r.PostFormValue("server_id"))
	if err != nil {
		h.renderShareReadError(w, r, err)
		return
	}
	if view == nil {
		h.renderError(w, r, http.StatusNotFound, "服务器不在可挂载范围内",
			"这台服务器不在这个共享的可挂载范围内；重选地址只面向仍可挂载的服务器。")
		return
	}
	taskID, err := h.cfg.ShareReselectRunner.StartShareReselect(ShareReselectRequest{
		ShareID: view.Share.ID, ShareName: view.Share.Name,
		ServerID: view.Server.ID, ServerName: view.Server.Name,
		Force: r.PostFormValue("force") == "1",
	}, ShareWriteSource{RemoteAddress: remoteIP(r)})
	if err != nil {
		h.logError(r, "启动共享存储重选地址任务失败", err)
		h.renderError(w, r, http.StatusInternalServerError, "启动重选失败", "启动重选地址任务失败，请稍后重试。")
		return
	}
	http.Redirect(w, r, view.ConfirmPath+"&task="+url.QueryEscape(taskID), http.StatusSeeOther)
}

// shareReselectResultLabel 把共享存储管理模块的固定结论分类换成页面文
// 案；未知分类原样保留，便于发现新的分类没有配文案。
func shareReselectResultLabel(result string) string {
	switch result {
	case "success":
		return "已切换到更优地址"
	case "best-address-unchanged":
		return "已是更优地址，无需切换"
	case "busy":
		return "挂载被占用，保留旧地址与旧配置"
	case "windows-unsupported":
		return "Windows 服务器不能重选共享存储地址"
	case "outside-mountable-range":
		return "服务器不在共享存储的可挂载范围内"
	case "ssh-error":
		return "连接目标服务器或执行固定重选命令失败"
	case "missing-sudo":
		return "服务器条目没有配置 sudo 凭据"
	case "unsupported-system":
		return "目标服务器操作系统不支持共享存储重选"
	case "not-managed":
		return "目标服务器上没有这个共享的 sshmgr 挂载配置"
	case "lock-timeout":
		return "另一个共享存储操作正在进行"
	case "unmount-failed":
		return "卸载旧挂载点失败"
	case "output-invalid":
		return "固定重选命令返回了不合法结论"
	case "selection-failed":
		return "候选地址探测失败或网络拓扑不完整"
	case "missing-software":
		return "目标服务器缺少共享存储客户端软件"
	case "remote-version-newer":
		return "目标服务器上的共享存储版本号更新，已停止重选"
	case "share-revoked":
		return "共享已从这台服务器撤销，需要重新挂载"
	case "protocol-failed":
		return "共享存储协议协商失败"
	case "verify-failed":
		return "重新挂载后校验共享存储失败"
	case "no-device":
		return "找不到共享存储提供方设备"
	default:
		return result
	}
}

// shareReselectConfirmPath 返回一台服务器的重选地址确认页地址。
func shareReselectConfirmPath(shareID, serverID string) string {
	return "/ui/shares/" + shareID + "/reselect?server=" + serverID
}
