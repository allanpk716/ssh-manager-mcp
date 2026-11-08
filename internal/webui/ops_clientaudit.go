package webui

import "net/http"

const (
	opsClientAuditPath = "/ui/ops/client-audit"
	opsClientAuditPage = 100
)

// handleOpsClientAudit 渲染客户端角色的本机审计页，按当前浏览器会话中的实
// 例读取本机审计日志文件。
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
	instance := h.requestClientInstance(r)
	rows, err := h.cfg.Ops.QueryClientAudit(instance, opsClientAuditPage)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取本机审计记录失败", "读取本机审计记录失败，请稍后重试。")
		return
	}
	h.renderPage(w, r, http.StatusOK, "client-audit.tmpl", pageData{
		Title: "本机审计", OpsClientAuditRows: rows, OpsClientInstance: instance,
	})
}
