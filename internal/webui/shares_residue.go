package webui

import (
	"net/http"
	"strings"
)

// ShareResidueRequest 是一次残留检查请求的页面输入。AllShares 为真时检
// 查全部共享；否则只检查 ShareName 指定的共享。
type ShareResidueRequest struct {
	ShareID   string
	ShareName string
	AllShares bool
}

// ShareResidueFinding 是页面上一个共享在一台服务器上的残留发现，不含共
// 享地址、账号或密码。HasCleanup 表示可以执行与单台卸载相同的远端清理。
type ShareResidueFinding struct {
	ShareName         string
	Version           int64
	Reason            string
	HasConfigEntry    bool
	HasCredentialFile bool
	HasMount          bool
	HasRevokedMarker  bool
	RevokedMarkerOnly bool
	HasCleanup        bool
}

// ShareResidueServerResult 是页面上一台服务器的残留检查结论。
type ShareResidueServerResult struct {
	ServerID   string
	ServerName string
	System     string
	Status     string
	Reason     string
	Notice     string
	Findings   []ShareResidueFinding
}

// ShareResidueCounts 是残留报告的五类汇总台数。
type ShareResidueCounts struct {
	Residue    int
	MarkerOnly int
	Unchecked  int
	NoResidue  int
	Skipped    int
}

// ShareResidueTaskStatus 是一次残留检查任务的状态。State 是 running、
// done 或 error；Servers 是逐台结论，Counts 是五类汇总台数。
type ShareResidueTaskStatus struct {
	ID             string
	State          string
	ShareID        string
	ShareName      string
	AllShares      bool
	Servers        []ShareResidueServerResult
	Counts         ShareResidueCounts
	FailureMessage string
}

// ShareResidueRunner 启动并读取共享存储残留检查任务。单机与权威端提
// 供；客户端不提供，残留检查路由不存在。实现必须立即返回任务标识，
// 远端操作在后台执行。
type ShareResidueRunner interface {
	StartShareResidue(request ShareResidueRequest, source ShareWriteSource) (string, error)
	ShareResidueStatus(taskID string) (*ShareResidueTaskStatus, bool)
}

// ShareResidueView 是残留检查任务页的展示数据。Task 是检查任务；Cleanup
// 是一条残留的清理任务（复用单台卸载任务状态）。任务标识无效时对应的
// Invalid 字段为真，页面提示重新发起。
type ShareResidueView struct {
	Share          *ShareRow
	Task           *ShareResidueTaskStatus
	TaskInvalid    bool
	Cleanup        *ShareUnmountTaskStatus
	CleanupInvalid bool
	AllShares      bool
	PollPath       string
}

// handleShareResidue 渲染残留检查任务页，或启动一次按共享/全部共享的
// 残留检查。
func (h *handler) handleShareResidue(w http.ResponseWriter, r *http.Request, identifier string) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		h.handleShareResiduePage(w, r, identifier)
	case http.MethodPost:
		h.handleShareResiduePost(w, r, identifier)
	default:
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "残留检查页只接受 GET 与 POST 请求。")
	}
}

func (h *handler) handleShareResiduePage(w http.ResponseWriter, r *http.Request, identifier string) {
	view, err := h.shareResidueView(r, identifier)
	if err != nil {
		h.renderShareReadError(w, r, err)
		return
	}
	view.AllShares = strings.TrimSpace(r.URL.Query().Get("scope")) == "all"
	taskIdentifier := strings.TrimSpace(r.URL.Query().Get("task"))
	if taskIdentifier != "" {
		status, ok := h.cfg.ShareResidueRunner.ShareResidueStatus(taskIdentifier)
		if !ok || status == nil || status.ShareID != view.Share.ID {
			view.TaskInvalid = true
		} else {
			view.Task = status
			view.AllShares = status.AllShares
			view.PollPath = r.URL.RequestURI()
		}
	}
	cleanupIdentifier := strings.TrimSpace(r.URL.Query().Get("cleanup"))
	if cleanupIdentifier != "" {
		if h.cfg.ShareUnmountRunner == nil {
			view.CleanupInvalid = true
		} else {
			status, ok := h.cfg.ShareUnmountRunner.ShareUnmountStatus(cleanupIdentifier)
			if !ok || status == nil || status.ShareID != view.Share.ID {
				view.CleanupInvalid = true
			} else {
				view.Cleanup = status
				view.PollPath = r.URL.RequestURI()
			}
		}
	}
	title := "残留检查 · " + view.Share.Name
	h.renderPage(w, r, http.StatusOK, "share-residue.tmpl", pageData{
		Title: title, ShareResidue: view,
	})
}

// shareResidueView 读取共享存储详情；不存在时返回页面错误。
func (h *handler) shareResidueView(r *http.Request, identifier string) (*ShareResidueView, error) {
	detail, err := h.cfg.Shares.ShareDetail(h.requestClientInstance(r), identifier)
	if err != nil {
		return nil, err
	}
	return &ShareResidueView{Share: detail}, nil
}

// handleShareResiduePost 启动一次残留检查任务并跳转到任务页。scope=all
// 时检查全部共享；否则只检查当前共享。
func (h *handler) handleShareResiduePost(w http.ResponseWriter, r *http.Request, identifier string) {
	view, err := h.shareResidueView(r, identifier)
	if err != nil {
		h.renderShareReadError(w, r, err)
		return
	}
	allShares := r.PostFormValue("scope") == "all"
	taskID, err := h.cfg.ShareResidueRunner.StartShareResidue(ShareResidueRequest{
		ShareID: view.Share.ID, ShareName: view.Share.Name, AllShares: allShares,
	}, ShareWriteSource{RemoteAddress: remoteIP(r)})
	if err != nil {
		h.logError(r, "启动共享存储残留检查任务失败", err)
		h.renderError(w, r, http.StatusInternalServerError, "启动残留检查失败",
			"启动残留检查任务失败，请稍后重试。")
		return
	}
	http.Redirect(w, r, "/ui/shares/"+view.Share.ID+"/residue?task="+taskID, http.StatusSeeOther)
}

// handleShareResidueCleanup 对一条残留发现启动与单台卸载相同的远端清理
// 任务，并跳转回残留页展示清理结论。
func (h *handler) handleShareResidueCleanup(w http.ResponseWriter, r *http.Request, identifier string) {
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "残留清理只接受 POST 请求。")
		return
	}
	view, err := h.shareResidueView(r, identifier)
	if err != nil {
		h.renderShareReadError(w, r, err)
		return
	}
	shareName := strings.TrimSpace(r.PostFormValue("share_name"))
	serverID := strings.TrimSpace(r.PostFormValue("server_id"))
	serverName := strings.TrimSpace(r.PostFormValue("server_name"))
	if shareName == "" || serverID == "" || serverName == "" {
		h.renderError(w, r, http.StatusBadRequest, "缺少清理目标",
			"请从残留检查结果页选择要清理的共享与服务器。")
		return
	}
	scope := "share"
	if r.PostFormValue("scope") == "all" {
		scope = "all"
	}
	taskID, err := h.cfg.ShareUnmountRunner.StartShareUnmount(ShareUnmountRequest{
		ShareID: view.Share.ID, ShareName: shareName,
		ServerID: serverID, ServerName: serverName,
		Force: r.PostFormValue("force") == "1",
	}, ShareWriteSource{RemoteAddress: remoteIP(r)})
	if err != nil {
		h.logError(r, "启动共享存储残留清理任务失败", err)
		h.renderError(w, r, http.StatusInternalServerError, "启动清理失败",
			"启动残留清理任务失败，请稍后重试。")
		return
	}
	http.Redirect(w, r, "/ui/shares/"+view.Share.ID+"/residue?cleanup="+taskID+"&scope="+scope,
		http.StatusSeeOther)
}

// shareResidueStatusLabel 把残留检查逐台结论分类换成页面文案；未知分类
// 原样保留。
func shareResidueStatusLabel(status string) string {
	switch status {
	case "residue":
		return "发现残留"
	case "marker-only":
		return "仅有撤销标记"
	case "no-residue":
		return "无残留"
	case "unchecked":
		return "未检查"
	case "skipped":
		return "跳过"
	default:
		return status
	}
}

// shareResidueReasonLabel 把残留原因分类换成页面文案。
func shareResidueReasonLabel(reason string) string {
	switch reason {
	case "share-unregistered":
		return "共享登记已删除"
	case "server-out-of-range":
		return "服务器不在可挂载范围内"
	case "ssh-error":
		return "连接服务器或执行枚举命令失败"
	case "missing-sudo":
		return "服务器条目没有配置 sudo 凭据"
	case "unsupported-system":
		return "目标服务器操作系统不支持共享存储枚举"
	case "output-invalid":
		return "枚举命令输出不合法"
	default:
		return reason
	}
}

// shareResidueFindingLabels 把一条残留发现包含的具体痕迹拼成一句展示文
// 案。
func shareResidueFindingLabels(finding ShareResidueFinding) string {
	labels := make([]string, 0, 4)
	if finding.HasConfigEntry {
		labels = append(labels, "配置条目")
	}
	if finding.HasCredentialFile {
		labels = append(labels, "凭据文件")
	}
	if finding.HasMount {
		labels = append(labels, "挂载")
	}
	if finding.HasRevokedMarker {
		labels = append(labels, "撤销标记")
	}
	return strings.Join(labels, "、")
}
