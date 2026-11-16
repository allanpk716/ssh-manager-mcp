package webui

import (
	"errors"
	"net/http"
	"net/url"
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

// AccessPairingForm 保存批准表单提交过的内容。批准失败重新渲染时用它保
// 留 owner 已经选择或输入的值，避免表单被清空。
type AccessPairingForm struct {
	ProfileID string
	Override  string
	Compared  bool
}

// registerPairingRoutes 只在权威端注册配对路由：配对请求由客户端经主监听
// 写入队列表，owner 在权威端 Web UI 裁决。
func (h *handler) registerPairingRoutes() {
	h.mux.HandleFunc(accessPairingPath, h.requireSession(h.handleAccessPairing))
	h.mux.HandleFunc(accessPairingListPath, h.requireSession(h.handleAccessPairingList))
	h.mux.HandleFunc(accessPairingPrefix, h.requireSession(h.handleAccessPairingAction))
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
	id, action, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, accessPairingPrefix), "/")
	if id == "" || strings.Contains(action, "/") {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}

	if action == "" {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "配对批准页只接受 GET 与 HEAD 请求。")
			return
		}
		query := r.URL.Query()
		next := pairingApprovalResumePath(id, query.Get("profile"), query.Get("override"))
		if !h.requireRecentReverify(w, r, next) {
			return
		}
		data, err := h.pairingDetailData(id)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "读取配对请求失败", "读取配对请求失败，请稍后重试。")
			return
		}
		applyPairingApprovalResumeForm(r, &data)
		status := http.StatusOK
		if data.AccessPairing == nil {
			status = http.StatusConflict
		}
		h.renderPage(w, r, status, "pairing-detail.tmpl", data)
		return
	}
	if action != "approve" && action != "reject" {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "配对裁决只接受 POST 请求。")
		return
	}
	next := pairingDetailPath(id)
	if action == "approve" {
		next = pairingApprovalResumePath(id, r.PostFormValue("profile"), r.PostFormValue("override"))
	}
	if !h.requireRecentReverify(w, r, next) {
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
			profile := r.PostFormValue("profile")
			compared := r.PostFormValue("compared") != ""
			override := r.PostFormValue("override")
			actionErr = h.cfg.Access.ApprovePairing(
				id, profile, compared, override, AccessWriteSource{RemoteAddress: remoteIP(r)},
			)
		}
	} else {
		actionErr = h.cfg.Access.RejectPairing(id, AccessWriteSource{RemoteAddress: remoteIP(r)})
	}
	if actionErr != nil {
		var data pageData
		var detailErr error
		if action == "approve" {
			data, detailErr = h.pairingDetailData(id)
			data.AccessPairingForm = AccessPairingForm{
				ProfileID: r.PostFormValue("profile"),
				Override:  r.PostFormValue("override"),
				Compared:  r.PostFormValue("compared") != "",
			}
		} else {
			data, detailErr = h.pairingPageData()
		}
		if detailErr != nil {
			h.renderError(w, r, http.StatusInternalServerError, "读取配对请求失败", "配对裁决失败后读取配对请求失败，请稍后重试。")
			return
		}
		data.Title = "配对"
		data.AccessPairingNotice = actionErr.Error()
		page := "pairing.tmpl"
		if action == "approve" {
			page = "pairing-detail.tmpl"
		}
		h.renderPage(w, r, http.StatusConflict, page, data)
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

func pairingDetailPath(id string) string {
	return accessPairingPath + "/" + url.PathEscape(id)
}

// pairingApprovalResumePath 保留批准表单里的 profile 与覆盖文字，
// 重新验证后回到同一张表单。
func pairingApprovalResumePath(id, profileID, addressOverride string) string {
	query := url.Values{}
	if profileID != "" {
		query.Set("profile", profileID)
	}
	if addressOverride != "" {
		query.Set("override", addressOverride)
	}
	if len(query) == 0 {
		return pairingDetailPath(id)
	}
	return pairingDetailPath(id) + "?" + query.Encode()
}

// applyPairingApprovalResumeForm 只在重新验证回跳后回填表单。「已比对」
// 是 owner 的人工声明，必须每次重新勾选。
func applyPairingApprovalResumeForm(r *http.Request, data *pageData) {
	if data.AccessPairing == nil {
		return
	}
	profileID := r.URL.Query().Get("profile")
	addressOverride := r.URL.Query().Get("override")
	if profileID == "" && addressOverride == "" {
		return
	}
	refilled := false
	for _, profile := range data.AccessProfiles {
		if profile.ID == profileID {
			data.AccessPairingForm.ProfileID = profileID
			refilled = true
			break
		}
	}
	if addressOverride != "" {
		data.AccessPairingForm.Override = addressOverride
		refilled = true
	}
	if refilled {
		data.AccessPairingNotice = "已重新验证管理员口令，请再次核对三项后勾选确认并批准。"
	}
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

// pairingDetailData 读取一个配对请求的批准页数据。请求已过期或已处理时可
// 能不再出现在列表里，此时保留空指针，由模板展示返回列表的指引。
func (h *handler) pairingDetailData(id string) (pageData, error) {
	pairings, err := h.cfg.Access.ListPairingRequests()
	if err != nil {
		return pageData{}, err
	}
	profiles, err := h.cfg.Access.ListProfiles()
	if err != nil {
		return pageData{}, err
	}
	data := pageData{Title: "批准配对", AccessProfiles: profiles}
	for i := range pairings {
		if pairings[i].ID != id {
			continue
		}
		hint := strings.TrimSpace(pairings[i].ProfileHint)
		for _, profile := range profiles {
			if hint == profile.ID || hint == profile.Name {
				pairings[i].SuggestedProfileID = profile.ID
				break
			}
		}
		data.AccessPairing = &pairings[i]
		data.AccessPairingForm = AccessPairingForm{ProfileID: pairings[i].SuggestedProfileID}
		return data, nil
	}
	return data, nil
}
