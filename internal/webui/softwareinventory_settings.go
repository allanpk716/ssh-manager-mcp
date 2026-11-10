package webui

import (
	"net/http"
	"net/url"
	"strings"
)

// SoftwareInventorySettingsServer 是采集设置页的一台服务器条目。
type SoftwareInventorySettingsServer struct {
	Name        string
	DeviceName  string
	SudoEnabled bool
}

// SoftwareInventoryClientInstance 是可作为采集执行者的客户端实例。
type SoftwareInventoryClientInstance struct {
	Name        string
	ProfileName string
}

// SoftwareInventorySettings 是采集设置页的展示数据。
type SoftwareInventorySettings struct {
	Devices         []SoftwareInventoryDeviceSummary
	Servers         []SoftwareInventorySettingsServer
	ClientInstances []SoftwareInventoryClientInstance
}

// SoftwareInventoryCollectorForm 是采集执行者登记表单。
type SoftwareInventoryCollectorForm struct {
	Device      string
	Collector   string
	Authority   bool
	ServerEntry string
	Error       string
	SudoError   string
}

func (h *handler) handleSoftwareInventorySettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "软件清单采集设置页只接受 GET 请求。")
		return
	}
	query := r.URL.Query()
	collector := SoftwareInventoryCollectorForm{
		Device: query.Get("device"), Collector: query.Get("collector"),
		ServerEntry: query.Get("server_entry"),
	}
	h.renderSoftwareInventorySettings(w, r, http.StatusOK, collector,
		query.Get("server_entry"), query.Get("sudo_enabled") == "1", query.Get("resume") == "1")
}

func (h *handler) handleSoftwareInventoryCollector(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "登记采集执行者只接受 POST 请求。")
		return
	}
	form := SoftwareInventoryCollectorForm{
		Device:      strings.TrimSpace(r.PostFormValue("device")),
		Collector:   strings.TrimSpace(r.PostFormValue("collector")),
		Authority:   r.PostFormValue("collector_authority") == "1",
		ServerEntry: strings.TrimSpace(r.PostFormValue("server_entry")),
	}
	if form.Authority && form.Collector != "" {
		form.Error = "不能同时选择权威端和客户端实例，请只保留一种采集执行者。"
		h.renderSoftwareInventorySettings(w, r, http.StatusBadRequest, form, "", false, false)
		return
	}
	if !form.Authority && form.Collector == "" {
		form.Error = "请选择权威端，或填写客户端实例的设备码名。"
		h.renderSoftwareInventorySettings(w, r, http.StatusBadRequest, form, "", false, false)
		return
	}
	if err := h.cfg.SoftwareInventory.SetSoftwareInventoryCollector(
		form, SoftwareInventoryWriteSource{RemoteAddress: remoteIP(r)},
	); err != nil {
		if message, ok := softwareInventoryFormMessage(err); ok {
			form.Error = message
			h.renderSoftwareInventorySettings(w, r, http.StatusBadRequest, form, "", false, false)
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, "登记采集执行者失败", "登记采集执行者失败，请稍后重试。")
		return
	}
	http.Redirect(w, r, "/ui/software-inventory/settings?notice=collector-set", http.StatusSeeOther)
}

func (h *handler) handleSoftwareInventorySudo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "设置 sudo 采集开关只接受 POST 请求。")
		return
	}
	server := strings.TrimSpace(r.PostFormValue("server_entry"))
	enabled := r.PostFormValue("sudo_enabled") == "1"
	if enabled {
		resume := url.Values{
			"resume": {"1"}, "server_entry": {server}, "sudo_enabled": {"1"},
		}
		if !h.requireRecentReverify(w, r, "/ui/software-inventory/settings?"+resume.Encode()) {
			return
		}
	}
	if err := h.cfg.SoftwareInventory.SetSoftwareInventorySudo(
		server, enabled, SoftwareInventoryWriteSource{RemoteAddress: remoteIP(r)},
	); err != nil {
		if message, ok := softwareInventoryFormMessage(err); ok {
			form := SoftwareInventoryCollectorForm{SudoError: message}
			h.renderSoftwareInventorySettings(
				w, r, http.StatusBadRequest, form,
				server, enabled, false,
			)
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, "设置 sudo 采集开关失败", "设置 sudo 采集开关失败，请稍后重试。")
		return
	}
	state := "sudo-disabled"
	if enabled {
		state = "sudo-enabled"
	}
	http.Redirect(w, r, "/ui/software-inventory/settings?notice="+state, http.StatusSeeOther)
}

func (h *handler) renderSoftwareInventorySettings(
	w http.ResponseWriter, r *http.Request, status int,
	collector SoftwareInventoryCollectorForm, sudoServer string, sudoEnabled, resumed bool,
) {
	settings, err := h.cfg.SoftwareInventory.SoftwareInventorySettings()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取软件清单采集设置失败", "读取软件清单采集设置失败，请稍后重试。")
		return
	}
	data := pageData{
		Title: "软件清单采集设置", SoftwareInventorySettings: &settings,
		SoftwareInventoryCollectorForm: &collector, SoftwareInventorySudoServer: sudoServer,
		SoftwareInventorySudoEnabled: sudoEnabled,
	}
	switch r.URL.Query().Get("notice") {
	case "collector-set":
		data.SoftwareInventoryNotice = "采集执行者已登记。"
	case "sudo-enabled":
		data.SoftwareInventoryNotice = "sudo 采集开关已打开。"
	case "sudo-disabled":
		data.SoftwareInventoryNotice = "sudo 采集开关已关闭。"
	}
	if resumed {
		data.SoftwareInventoryResumeNotice = "已保留表单状态，请再次提交。"
	}
	h.renderPage(w, r, status, "softwareinventory-settings.tmpl", data)
}
