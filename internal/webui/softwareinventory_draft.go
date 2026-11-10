package webui

import (
	"errors"
	"net/http"
	"strings"
)

// SoftwareInventoryDraft 是一条待 owner 审阅的声明草稿。
type SoftwareInventoryDraft struct {
	ID                 string
	Device             string
	Category           string
	CategoryLabel      string
	Name               string
	Version            string
	PackageURL         string
	CPE                string
	BelongsTo          string
	Note               string
	ClaimedObservedKey string
	ListeningPorts     []SoftwareInventoryPort
	ListeningPortsText string
}

func (h *handler) handleSoftwareInventoryDrafts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "声明草稿审阅页只接受 GET 请求。")
		return
	}
	drafts, err := h.cfg.SoftwareInventory.SoftwareInventoryDrafts()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取声明草稿失败", "读取声明草稿失败，请稍后重试。")
		return
	}
	data := pageData{Title: "声明草稿审阅", SoftwareInventoryDrafts: drafts}
	switch r.URL.Query().Get("notice") {
	case "draft-confirmed":
		data.SoftwareInventoryNotice = "草稿已确认并进入生效清单。"
	case "draft-rejected":
		data.SoftwareInventoryNotice = "草稿已驳回。"
	}
	h.renderPage(w, r, http.StatusOK, "softwareinventory-drafts.tmpl", data)
}

func (h *handler) handleSoftwareInventoryDraftConfirm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "确认声明草稿只接受 POST 请求。")
		return
	}
	id := strings.TrimSpace(r.PostFormValue("draft_id"))
	var modified *SoftwareInventoryDeclarationForm
	if r.PostFormValue("mode") == "edited" {
		form := declarationFormFromPost(r)
		ports, err := parseSoftwareInventoryPorts(form.ListeningPortsText)
		if err != nil {
			h.renderError(w, r, http.StatusBadRequest, "修改后确认失败", err.Error())
			return
		}
		form.ListeningPorts = ports
		modified = &form
	}
	if err := h.cfg.SoftwareInventory.ConfirmSoftwareInventoryDraft(
		id, modified, SoftwareInventoryWriteSource{RemoteAddress: remoteIP(r)},
	); err != nil {
		var missing *SoftwareInventoryDeclarationNotFoundError
		if errors.As(err, &missing) {
			h.renderError(w, r, http.StatusNotFound, "声明草稿不存在", missing.Error())
			return
		}
		if message, ok := softwareInventoryFormMessage(err); ok {
			h.renderError(w, r, http.StatusBadRequest, "确认声明草稿失败", message)
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, "确认声明草稿失败", "确认声明草稿失败，请稍后重试。")
		return
	}
	http.Redirect(w, r, "/ui/software-inventory/drafts?notice=draft-confirmed", http.StatusSeeOther)
}

func (h *handler) handleSoftwareInventoryDraftReject(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "驳回声明草稿只接受 POST 请求。")
		return
	}
	id := strings.TrimSpace(r.PostFormValue("draft_id"))
	if err := h.cfg.SoftwareInventory.RejectSoftwareInventoryDraft(
		id, SoftwareInventoryWriteSource{RemoteAddress: remoteIP(r)},
	); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "驳回声明草稿失败", "驳回声明草稿失败，请稍后重试。")
		return
	}
	http.Redirect(w, r, "/ui/software-inventory/drafts?notice=draft-rejected", http.StatusSeeOther)
}
