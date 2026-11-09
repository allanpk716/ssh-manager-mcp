package webui

import (
	"bytes"
	"net/http"
	"strconv"
	"time"
)

const (
	opsUpgradeServerPath        = "/ui/ops/upgrade-server"
	opsUpgradeServerConfirmPath = "/ui/ops/upgrade-server/confirm"
	// opsUpgradeShutdownDelay 给浏览器留出接收完整成功页的时间；页面先带
	// Content-Length 写出并刷新，随后才触发优雅关闭。
	opsUpgradeShutdownDelay = 500 * time.Millisecond
)

// MachineInitializer 是「把已有 vault 的单机升级为权威端」的注入接口。真实
// 适配器调用 machineinit.Initialize；测试与未来调用方可提供替身。
type MachineInitializer interface {
	InitializeServer() error
}

func (h *handler) handleOpsUpgradeServer(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != opsUpgradeServerPath || h.cfg.Role != RoleStandalone || h.cfg.MachineInit == nil {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "升级为权威端确认页只接受 GET 请求。")
		return
	}
	h.renderPage(w, r, http.StatusOK, "ops-upgrade-server.tmpl", pageData{Title: "升级为权威端"})
}

func (h *handler) handleOpsUpgradeServerConfirm(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != opsUpgradeServerConfirmPath || h.cfg.Role != RoleStandalone || h.cfg.MachineInit == nil {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "确认升级为权威端只接受 POST 请求。")
		return
	}
	if r.PostFormValue("confirm") != "1" {
		h.renderPage(w, r, http.StatusBadRequest, "ops-upgrade-server.tmpl", pageData{
			Title: "升级为权威端", OpsUpgradeServerError: initializationPlainError("请先勾选确认，再升级为权威端。"),
		})
		return
	}
	if err := h.cfg.MachineInit.InitializeServer(); err != nil {
		h.renderUpgradeServerError(w, r, err)
		return
	}
	if !h.writeCompleteUpgradeServerSuccessPage(w, r) {
		return
	}
	if h.cfg.RequestShutdown != nil {
		shutdown := h.cfg.RequestShutdown
		go func() {
			time.Sleep(opsUpgradeShutdownDelay)
			shutdown(ShutdownReasonStandaloneUpgrade)
		}()
	}
}

func (h *handler) renderUpgradeServerError(w http.ResponseWriter, r *http.Request, failure error) {
	h.renderPage(w, r, http.StatusInternalServerError, "ops-upgrade-server.tmpl", pageData{
		Title: "升级为权威端", OpsUpgradeServerError: initializationErrorWithAdvice(failure),
	})
}

func (h *handler) writeCompleteUpgradeServerSuccessPage(w http.ResponseWriter, r *http.Request) bool {
	data := pageData{
		Title: "已升级为权威端", Assets: h.assets, Caps: h.caps,
		VersionHint: h.cfg.versionHint(), OpsUpgradeServerSuccess: true,
	}
	if session := h.currentSession(r); session != nil {
		data.CSRFToken = session.csrf
		data.Authenticated = true
	}
	parsed := h.pages["ops-upgrade-server.tmpl"]
	if parsed.full == nil {
		h.renderError(w, r, http.StatusInternalServerError, "升级为权威端失败", "页面模板加载失败。")
		return false
	}
	var body bytes.Buffer
	if err := parsed.full.ExecuteTemplate(&body, "layout", data); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "升级为权威端失败", "页面渲染失败。")
		return false
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(body.Len()))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body.Bytes()); err != nil {
		return false
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return true
}
