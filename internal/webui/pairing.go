package webui

import (
	"errors"
	"net/http"
	"strings"
)

// AccessPairingSummary 是配对页的一行展示数据。短认证码只有存在时才非空；
// ForeignTarget 是服务端按客户端声明的目标地址重新计算出的异常标记。
type AccessPairingSummary struct {
	ID                 string
	Name               string
	TargetURL          string
	ShortCode          string
	SourceAddress      string
	ProfileHint        string
	ProfileName        string
	SuggestedProfileID string
	ReplaceInactive    bool
	State              string
	ForeignTarget      bool
	RemainingSeconds   int64
	HasShortCode       bool
}

// registerPairingRoutes 只在权威端注册配对路由：配对请求由客户端经主监听
// 写入队列表，owner 在权威端 Web UI 裁决。
func (h *handler) registerPairingRoutes() {
	h.mux.HandleFunc(accessPairingPath, h.handleAccessPairing)
	h.mux.HandleFunc(accessPairingListPath, h.handleAccessPairingList)
	h.mux.HandleFunc(accessPairingPrefix, h.handleAccessPairingAction)
}

func (h *handler) handleAccessPairing(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != accessPairingPath {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "配对页只接受 GET 与 HEAD 请求。")
		return
	}
	data, err := h.pairingPageData()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取配对请求失败", "读取配对请求失败，请稍后重试。")
		return
	}
	h.renderPage(w, r, http.StatusOK, "pairing.tmpl", data)
}

func (h *handler) handleAccessPairingList(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != accessPairingListPath {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "配对列表只接受 GET 与 HEAD 请求。")
		return
	}
	data, err := h.pairingPageData()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取配对请求失败", "读取配对请求失败，请稍后重试。")
		return
	}
	h.renderPage(w, r, http.StatusOK, "pairing-list.tmpl", data)
}

func (h *handler) handleAccessPairingAction(w http.ResponseWriter, r *http.Request) {
	id, action, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, accessPairingPrefix), "/")
	if !ok || id == "" || (action != "approve" && action != "reject") {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "配对裁决只接受 POST 请求。")
		return
	}

	var actionErr error
	if action == "approve" {
		row, listErr := h.accessPairingByID(id)
		if listErr != nil {
			h.renderError(w, r, http.StatusInternalServerError, "读取配对请求失败", "读取配对请求失败，请稍后重试。")
			return
		}
		if row != nil && !row.HasShortCode {
			actionErr = errors.New("行缺少短认证码，无法人工比对，只能拒绝")
		} else {
			actionErr = h.cfg.Access.ApprovePairing(
				id, r.PostFormValue("profile"), r.PostFormValue("compared") != "",
				r.PostFormValue("override"), AccessWriteSource{RemoteAddress: remoteIP(r)},
			)
		}
	} else {
		actionErr = h.cfg.Access.RejectPairing(id, AccessWriteSource{RemoteAddress: remoteIP(r)})
	}
	if actionErr != nil {
		data, listErr := h.pairingPageData()
		if listErr != nil {
			h.renderError(w, r, http.StatusInternalServerError, "读取配对请求失败", "配对裁决失败后读取配对请求失败，请稍后重试。")
			return
		}
		data.Title = "配对"
		data.AccessPairingNotice = actionErr.Error()
		h.renderPage(w, r, http.StatusConflict, "pairing.tmpl", data)
		return
	}
	http.Redirect(w, r, accessPairingPath, http.StatusSeeOther)
}

// accessPairingByID 在配对列表里按标识符查找一行。
func (h *handler) accessPairingByID(id string) (*AccessPairingSummary, error) {
	pairings, err := h.cfg.Access.ListPairingRequests()
	if err != nil {
		return nil, err
	}
	for i := range pairings {
		if pairings[i].ID == id {
			return &pairings[i], nil
		}
	}
	return nil, nil
}

// pairingPageData 读取配对请求与 profile，并把 profile 提示换算成表单可选
// profile 的标识符，能对应时用于预选。
func (h *handler) pairingPageData() (pageData, error) {
	pairings, err := h.cfg.Access.ListPairingRequests()
	if err != nil {
		return pageData{}, err
	}
	profiles, err := h.cfg.Access.ListProfiles()
	if err != nil {
		return pageData{}, err
	}
	for i := range pairings {
		hint := strings.TrimSpace(pairings[i].ProfileHint)
		for _, profile := range profiles {
			if hint == profile.ID || hint == profile.Name {
				pairings[i].SuggestedProfileID = profile.ID
				break
			}
		}
	}
	return pageData{
		Title: "配对", AccessPairings: pairings, AccessProfiles: profiles,
	}, nil
}
