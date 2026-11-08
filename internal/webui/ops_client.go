package webui

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// OpsClientInstance 是客户端页实例列表的一行展示数据。
type OpsClientInstance struct {
	Name             string
	DisplayName      string
	SnapshotAge      time.Duration
	HasSnapshot      bool
	Complete         bool
	MissingMaterials []string
}

// SnapshotAgeText 返回实例列表中的快照年龄文案。
func (i OpsClientInstance) SnapshotAgeText() string {
	if !i.HasSnapshot {
		return "无快照"
	}
	return i.SnapshotAge.Round(time.Second).String()
}

// MissingMaterialsText 返回缺少材料的文案；材料齐全时返回空串。
func (i OpsClientInstance) MissingMaterialsText() string {
	if len(i.MissingMaterials) == 0 {
		return ""
	}
	return strings.Join(i.MissingMaterials, "、")
}

// OpsClientStatus 是客户端页顶部的连接状态展示数据。
type OpsClientStatus struct {
	Instance               string
	InstanceDisplay        string
	AuthorityURL           string
	CertificateFingerprint string
	Profile                string
	ServerCount            int
	PulledAt               time.Time
	SnapshotRevision       int64
	RecommendedVersion     string
	ReadError              string
}

// OpsClientSyncError 是 Web 窄接口返回的立即同步失败分类。
type OpsClientSyncError struct {
	Kind    string
	Message string
}

func (e *OpsClientSyncError) Error() string { return e.Text() }

// Text 返回按现有错误分类整理后的中文提示。
func (e *OpsClientSyncError) Text() string {
	switch e.Kind {
	case "unreachable":
		return "无法连接权威端：" + e.Message
	case "device_code_revoked":
		return "设备码被撤销，请重新配对后拉取：" + e.Message
	case "certificate_fingerprint":
		return "证书公钥指纹不符，拒绝连接：" + e.Message
	default:
		if e.Message == "" {
			return "立即同步失败。"
		}
		return e.Message
	}
}

const (
	opsClientPath         = "/ui/ops/client"
	opsClientSyncPath     = "/ui/ops/client/sync"
	opsClientInstancePath = "/ui/ops/client/instance"
)

// handleOpsClient 渲染客户端页：连接状态、实例列表、立即同步与实例切换。
func (h *handler) handleOpsClient(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != opsClientPath || h.cfg.Role != RoleClient || h.cfg.Ops == nil ||
		!h.caps.CanUseClientOps {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "客户端页只接受 GET 请求。")
		return
	}
	data, response := h.renderOpsClientPage(w, r, "", "")
	if response {
		return
	}
	h.renderPage(w, r, http.StatusOK, "client.tmpl", data)
}

// renderOpsClientPage 读取客户端页数据。读取失败时输出错误页并返回 response=true。
func (h *handler) renderOpsClientPage(w http.ResponseWriter, r *http.Request, notice, errorMessage string) (pageData, bool) {
	instance := h.requestClientInstance(r)
	instances, err := h.cfg.Ops.ListClientInstances()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取实例列表失败", "读取实例列表失败，请稍后重试。")
		return pageData{}, true
	}
	clientStatus, err := h.cfg.Ops.ClientStatus(instance)
	if err != nil {
		clientStatus = OpsClientStatus{Instance: instance, InstanceDisplay: clientInstanceDisplay(instance), ReadError: err.Error()}
	}
	data := pageData{
		Title: "客户端", OpsClientStatus: &clientStatus, OpsClientInstances: instances,
		OpsClientNotice: notice, OpsClientError: errorMessage,
	}
	return data, false
}

// handleOpsClientInstance 只切换当前浏览器会话里的实例；成功后由运维窄接口写实例
// 本机审计，再更新会话内存值，不写任何持久默认实例。
func (h *handler) handleOpsClientInstance(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != opsClientInstancePath || h.cfg.Role != RoleClient || h.cfg.Ops == nil ||
		!h.caps.CanUseClientOps {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "实例切换只接受 POST 请求。")
		return
	}
	if err := r.ParseForm(); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "实例切换失败", "表单无法解析，请刷新页面后重试。")
		return
	}
	target := r.PostFormValue("instance")
	from := h.requestClientInstance(r)
	if err := h.cfg.Ops.SwitchClientInstance(from, target, remoteIP(r)); err != nil {
		data, response := h.renderOpsClientPage(w, r, "", err.Error())
		if response {
			return
		}
		h.renderPage(w, r, http.StatusBadRequest, "client.tmpl", data)
		return
	}
	h.setSessionClientInstance(r, target)
	http.Redirect(w, r, opsClientPath, http.StatusSeeOther)
}

// handleOpsClientSync 处理立即同步，并重新渲染连接状态与实例列表。
func (h *handler) handleOpsClientSync(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != opsClientSyncPath || h.cfg.Role != RoleClient || h.cfg.Ops == nil ||
		!h.caps.CanUseClientOps {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "立即同步只接受 POST 请求。")
		return
	}
	instance := h.requestClientInstance(r)
	status, err := h.cfg.Ops.SyncClient(instance, remoteIP(r))
	if err != nil {
		message := "立即同步失败，请稍后重试。"
		var syncErr *OpsClientSyncError
		if asOpsClientSyncError(err, &syncErr) {
			message = syncErr.Text()
		}
		data, response := h.renderOpsClientPage(w, r, "", message)
		if response {
			return
		}
		h.renderPage(w, r, http.StatusBadRequest, "client.tmpl", data)
		return
	}
	notice := fmt.Sprintf("已同步：快照修订号 %d，%d 台服务器。", status.SnapshotRevision, status.ServerCount)
	data, response := h.renderOpsClientPage(w, r, notice, "")
	if response {
		return
	}
	h.renderPage(w, r, http.StatusOK, "client.tmpl", data)
}

// asOpsClientSyncError 是错误类型判断的薄封装，保持处理器文件不直接引用错误包。
func asOpsClientSyncError(err error, target **OpsClientSyncError) bool {
	if cast, ok := err.(*OpsClientSyncError); ok {
		*target = cast
		return true
	}
	return false
}

// clientInstanceDisplay 把空实例名转换为「默认实例」。
func clientInstanceDisplay(instance string) string {
	if instance == "" {
		return "默认实例"
	}
	return instance
}
