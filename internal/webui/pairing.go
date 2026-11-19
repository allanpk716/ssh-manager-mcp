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
	PairingID string
	ProfileID string
	Compared  bool
}

// ProfileSelected 判断一个 profile 选项是否应被选中。表单失败回填时优先保
// 留 owner 刚选择的那一项。
func (s AccessPairingSummary) ProfileSelected(profileID string, form AccessPairingForm) bool {
	if form.PairingID == s.ID && form.ProfileID != "" {
		return form.ProfileID == profileID
	}
	return s.SuggestedProfileID == profileID
}

// KeepCompared 只回填对应配对请求的比对勾选。
func (s AccessPairingSummary) KeepCompared(form AccessPairingForm) bool {
	return form.PairingID == s.ID && form.Compared
}

// registerPairingRoutes 只在权威端注册配对路由：配对请求由客户端经主监听
// 写入队列表，owner 在权威端 Web UI 裁决。
func (h *handler) registerPairingRoutes() {
	h.mux.HandleFunc(clientPairingPath, h.requireSession(h.handleAccessPairing))
	h.mux.HandleFunc(clientPairingListPath, h.requireSession(h.handleAccessPairingList))
	h.mux.HandleFunc(clientPairingPrefix, h.requireSession(h.handleAccessPairingAction))
	h.mux.HandleFunc(legacyAccessPairingPath, h.requireSession(h.redirectLegacyPairing))
	h.mux.HandleFunc(legacyAccessPairingListPath, h.requireSession(h.redirectLegacyPairing))
	h.mux.HandleFunc(legacyAccessPairingPrefix, h.requireSession(h.redirectLegacyPairing))
}

// redirectLegacyPairing 把旧访问区域的配对地址搬到客户端区，查询参数保持
// 不变。旧地址只保留兼容读取，写表单一律指向新地址。
func (h *handler) redirectLegacyPairing(w http.ResponseWriter, r *http.Request) {
	target := clientPairingPath + strings.TrimPrefix(r.URL.Path, legacyAccessPairingPath)
	if target == clientPairingPath+"/" {
		target = clientPairingPath
	}
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func applyPairingDoneNotice(data *pageData, r *http.Request) {
	done := r.URL.Query().Get("done")
	if done != "approve" && done != "reject" {
		return
	}
	data.AccessPairingSuccess = true
	if done == "approve" {
		data.AccessPairingNotice = "已批准配对请求。请让客户端在完成窗口内继续操作。"
		return
	}
	data.AccessPairingNotice = "已拒绝配对请求。"
}

func (h *handler) handleAccessPairing(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != clientPairingPath {
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
	applyPairingDoneNotice(&data, r)
	h.renderPage(w, r, http.StatusOK, "pairing.tmpl", data)
}

func (h *handler) handleAccessPairingList(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != clientPairingListPath {
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
	applyPairingDoneNotice(&data, r)
	h.renderPage(w, r, http.StatusOK, "pairing-list.tmpl", data)
}

func (h *handler) handleAccessPairingAction(w http.ResponseWriter, r *http.Request) {
	id, action, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, clientPairingPrefix), "/")
	if id == "" || strings.Contains(action, "/") {
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
		return
	}

	if action == "" {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "配对批准页只接受 GET 与 HEAD 请求。")
			return
		}
		data, err := h.pairingDetailData(id)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "读取配对请求失败", "读取配对请求失败，请稍后重试。")
			return
		}
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
			// Web UI 永远不带命令行显式放行；异常地址由访问层重
			// 新判定并写失败审计。
			actionErr = h.cfg.Access.ApprovePairing(
				id, profile, compared, false, AccessWriteSource{RemoteAddress: remoteIP(r)},
			)
		}
	} else {
		actionErr = h.cfg.Access.RejectPairing(id, AccessWriteSource{RemoteAddress: remoteIP(r)})
	}
	if actionErr != nil {
		data, detailErr := h.pairingPageData()
		if detailErr != nil {
			h.renderError(w, r, http.StatusInternalServerError, "读取配对请求失败", "配对裁决失败后读取配对请求失败，请稍后重试。")
			return
		}
		if action == "approve" {
			data.AccessPairingForm = AccessPairingForm{
				PairingID: id, ProfileID: r.PostFormValue("profile"),
				Compared: r.PostFormValue("compared") != "",
			}
		}
		data.Title = "配对"
		data.AccessPairingNotice = actionErr.Error()
		h.renderPage(w, r, http.StatusConflict, "pairing.tmpl", data)
		return
	}
	done := "reject"
	if action == "approve" {
		done = "approve"
	}
	http.Redirect(w, r, clientPairingPath+"?done="+done, http.StatusSeeOther)
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
	return clientPairingPath + "/" + url.PathEscape(id)
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
		pairings[i].SuggestedProfileID = suggestedPairingProfile(pairings[i], profiles)
	}
	return pageData{
		Title: "配对", AccessPairings: pairings, AccessProfiles: profiles,
	}, nil
}

func suggestedPairingProfile(pairing AccessPairingSummary, profiles []AccessProfileSummary) string {
	hint := strings.TrimSpace(pairing.ProfileHint)
	for _, profile := range profiles {
		if hint == profile.ID || hint == profile.Name {
			return profile.ID
		}
	}
	if len(profiles) == 1 {
		return profiles[0].ID
	}
	return ""
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
		pairings[i].SuggestedProfileID = suggestedPairingProfile(pairings[i], profiles)
		data.AccessPairing = &pairings[i]
		return data, nil
	}
	return data, nil
}
