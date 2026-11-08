package webui

import "net/http"

const (
	opsClientAuditPath = "/ui/ops/client-audit"
	opsClientAuditPage = 100
)

// handleOpsClientAudit 渲染客户端角色的本机审计页：只读取 `sshmgr ui` 启动
// 时选定的实例；会话内切换实例由后续客户端页接入，窄接口已带实例名参数。
func (h *handler) handleOpsClientAudit(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != opsClientAuditPath || h.cfg.Role != RoleClient || h.cfg.Ops == nil ||
		!h.caps.CanQueryClientAudit {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "本机审计页只接受 GET 请求。")
		return
	}
	rows, err := h.cfg.Ops.QueryClientAudit(h.cfg.ClientInstance, opsClientAuditPage)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取本机审计记录失败", "读取本机审计记录失败，请稍后重试。")
		return
	}
	instance := h.cfg.ClientInstance
	if instance == "" {
		instance = "默认实例"
	}
	h.renderPage(w, r, http.StatusOK, "client-audit.tmpl", pageData{
		Title: "本机审计", OpsClientAuditRows: rows, OpsClientInstance: instance,
	})
}
