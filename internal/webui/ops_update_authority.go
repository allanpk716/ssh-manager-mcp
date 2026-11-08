package webui

import (
	"bytes"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const opsUpdateRestartPath = "/ui/ops/update/restart"

// restartGraceDelay 给 net/http 一小段时间在处理器返回后补完协议层结束
// 标记（尤其是 HTTP/2 的结束流），再触发会终止本进程的服务重启。
const restartGraceDelay = 500 * time.Millisecond

// OpsRestartStore 是权威端重启服务的窄接口。afterResponse 由处理器提供，
// 只允许它在完整响应已经写出并刷新后触发传入的重启动作。
type OpsRestartStore interface {
	RequestServiceRestart(sourceIP string, afterResponse func(restart func())) error
}

// delayedRestartWriter 保存响应结束后才允许执行的动作。动作由外层处理器
// 在页面处理器返回并刷新响应后异步触发，不在页面处理过程中执行。
type delayedRestartWriter struct {
	http.ResponseWriter
	action func()
	once   sync.Once
}

func (w *delayedRestartWriter) afterResponse(action func()) {
	w.once.Do(func() { w.action = action })
}

// handleRestartAfterResponse 先执行页面处理器，再把完整响应刷新给浏览器，
// 最后异步触发已登记的重启动作。
func (h *handler) handleRestartAfterResponse(page func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		delayed := &delayedRestartWriter{ResponseWriter: w}
		page(delayed, r)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		if delayed.action != nil {
			action := delayed.action
			go func() {
				time.Sleep(restartGraceDelay)
				action()
			}()
		}
	}
}

// handleAuthorityOpsUpdateRestart 处理独立的重启服务表单。它先登记响应结束
// 后的动作，再写出「正在重启」页面；真正构造服务管理器与调用重启由动作执行。
func (h *handler) handleAuthorityOpsUpdateRestart(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != opsUpdateRestartPath || h.cfg.Role != RoleServer || h.cfg.Ops == nil ||
		!h.caps.CanRestartService || r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "重启服务只接受 POST 请求。")
		return
	}
	if !h.recentlyVerified(h.currentAuthoritySession(r)) {
		http.Redirect(w, r, reverifyPath(opsUpdateRestartPath), http.StatusSeeOther)
		return
	}
	restarts, ok := h.cfg.Ops.(OpsRestartStore)
	if !ok {
		h.renderError(w, r, http.StatusInternalServerError, "重启服务失败", "服务管理器未接线，无法重启。")
		return
	}
	delayed, ok := w.(*delayedRestartWriter)
	if !ok {
		h.renderError(w, r, http.StatusInternalServerError, "重启服务失败", "缺少响应结束调度器。")
		return
	}
	var restart func()
	if err := restarts.RequestServiceRestart(remoteIP(r), func(action func()) {
		restart = action
	}); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "重启服务失败", "无法安排服务重启，请稍后重试。")
		return
	}
	if !h.writeCompleteUpdateRestartPage(w, r) {
		return
	}
	delayed.afterResponse(restart)
}

// writeCompleteUpdateRestartPage 先把整页渲染进内存并声明 Content-Length，
// 再一次性写给浏览器。这样刷新响应时连接已经拿到有边界的完整响应，不会
// 依赖处理器返回后才补出的分块结束标记。
func (h *handler) writeCompleteUpdateRestartPage(w http.ResponseWriter, r *http.Request) bool {
	data := pageData{
		Title: "正在重启", Assets: h.assets, Caps: h.caps,
		VersionHint: h.cfg.versionHint(), OpsUpdateRestarting: true,
	}
	if session := h.currentSession(r); session != nil {
		data.CSRFToken = session.csrf
		data.Authenticated = true
	}
	parsed := h.pages["update.tmpl"]
	if parsed.full == nil {
		h.renderError(w, r, http.StatusInternalServerError, "重启服务失败", "页面模板加载失败。")
		return false
	}
	var body bytes.Buffer
	if err := parsed.full.ExecuteTemplate(&body, "layout", data); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "重启服务失败", "页面渲染失败。")
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

// handleAuthorityOpsUpdate 渲染权威端升级页并执行第一步「替换二进制」。
// 第二步「重启服务」是独立表单，处理器只负责把重启动作交给响应结束后调度。
func (h *handler) handleAuthorityOpsUpdate(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != opsUpdatePath || h.cfg.Ops == nil || !h.caps.CanUpgradeBinary ||
		h.cfg.Role != RoleServer {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		status, err := h.cfg.Ops.CheckUpdate("")
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "读取升级状态失败", "读取升级状态失败，请稍后重试。")
			return
		}
		h.renderPage(w, r, http.StatusOK, "update.tmpl", pageData{Title: "升级", OpsUpdate: &status})
	case http.MethodPost:
		if !h.recentlyVerified(h.currentAuthoritySession(r)) {
			http.Redirect(w, r, reverifyPath(opsUpdatePath), http.StatusSeeOther)
			return
		}
		result, err := h.cfg.Ops.ApplyUpdate("", remoteIP(r))
		status, statusErr := h.cfg.Ops.CheckUpdate("")
		data := pageData{Title: "升级"}
		if statusErr == nil {
			data.OpsUpdate = &status
		}
		if err != nil {
			var updateErr *OpsUpdateError
			if !asOpsUpdateError(err, &updateErr) {
				h.renderError(w, r, http.StatusInternalServerError, "升级失败", "升级失败，请稍后重试。")
				return
			}
			data.OpsUpdateError = updateErr
			h.renderPage(w, r, http.StatusBadRequest, "update.tmpl", data)
			return
		}
		data.OpsUpdateResult = &result
		h.renderPage(w, r, http.StatusOK, "update.tmpl", data)
	default:
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "升级页只接受 GET 与 POST 请求。")
	}
}
