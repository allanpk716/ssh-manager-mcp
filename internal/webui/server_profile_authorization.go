package webui

import (
	"net/http"
	"strings"
)

func (h *handler) handleServerProfileAuthorizationPreview(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != serverProfilePreviewPath {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "profile 授权预览只接受 POST 请求。")
		return
	}
	serverID := r.PostFormValue("server_id")
	if serverID == "" {
		h.renderError(w, r, http.StatusBadRequest, "生成 profile 授权预览失败", "缺少服务器标识符。")
		return
	}
	preview, err := h.cfg.Access.PreviewServerProfileAuthorization(serverID, r.PostForm["profile"])
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, "生成 profile 授权预览失败", "生成 profile 授权预览失败，请核对服务器与 profile 后重试。")
		return
	}
	h.renderServerProfilePreviewPage(w, r, http.StatusOK, preview, "")
}

func (h *handler) handleServerProfileAuthorizationConfirm(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != serverProfileConfirmPath {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "确认 profile 授权只接受 POST 请求。")
		return
	}
	serverID := r.PostFormValue("server_id")
	if serverID == "" {
		h.renderError(w, r, http.StatusBadRequest, "确认 profile 授权失败", "缺少服务器标识符。")
		return
	}
	result, conflict, err := h.cfg.Access.ApplyServerProfileAuthorization(
		serverID, r.PostForm["profile"], r.PostFormValue("summary"),
		AccessWriteSource{RemoteAddress: remoteIP(r)},
	)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "确认 profile 授权失败", "确认 profile 授权失败，请稍后重试。")
		return
	}
	if conflict != nil {
		h.renderServerProfilePreviewPage(w, r, http.StatusConflict, conflict.Latest,
			"profile 授权已被其他入口修改，请核对最新集合后再确认。")
		return
	}
	notice := "没有变化，未写入数据或审计。"
	if result.Applied {
		notice = "已保存 profile 授权。"
	}
	h.renderServerProfilePreviewPage(w, r, http.StatusOK, result.Preview, notice)
}

func (h *handler) renderServerProfilePreviewPage(
	w http.ResponseWriter, r *http.Request, status int,
	preview ServerProfileAuthorizationPreview, notice string,
) {
	h.renderPage(w, r, status, "server-profile-authorization-preview.tmpl", pageData{
		Title: "profile 授权预览", ServerProfileAuthorizationPreview: preview,
		ServerProfileAuthorizationNotice: notice,
	})
}

func serverProfileNames(profiles []AccessProfileRef) string {
	names := make([]string, 0, len(profiles))
	for _, profile := range profiles {
		names = append(names, profile.Name)
	}
	return strings.Join(names, "、")
}
