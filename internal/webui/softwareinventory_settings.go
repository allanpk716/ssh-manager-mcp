package webui

import (
	"net/http"
	"net/url"
	"strings"
)

// SoftwareInventoryClientInstance 是设备详情高级区里可作为采集执行者的
// 一个客户端实例。
type SoftwareInventoryClientInstance struct {
	Name        string
	ProfileName string
}

// SoftwareInventoryCollectorForm 是设备详情高级区的采集执行者登记表单。
// Collector 为空表示选择权威端；Authority 与之保持一致，交给来源接口时
// 由适配层换算成权威端登记值。
type SoftwareInventoryCollectorForm struct {
	Device      string
	Collector   string
	Authority   bool
	ServerEntry string
	// InvalidCollector 表示提交的客户端实例不在有效列表里；回显时保留
	// 这个值并提示重新选择。
	InvalidCollector bool
	Error            string
	SudoError        string
}

// handleSoftwareInventoryDeviceCollector 处理设备详情高级区的采集执行者
// 登记表单。校验与授权判断在领域层完成，失败时回到设备页并展开高级区。
func (h *handler) handleSoftwareInventoryDeviceCollector(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "登记采集执行者只接受 POST 请求。")
		return
	}
	collector := strings.TrimSpace(r.PostFormValue("collector"))
	form := SoftwareInventoryCollectorForm{
		Device:      strings.TrimSpace(r.PostFormValue("device")),
		Collector:   collector,
		Authority:   collector == "",
		ServerEntry: strings.TrimSpace(r.PostFormValue("server_entry")),
	}
	if form.Device == "" {
		h.renderError(w, r, http.StatusBadRequest, "缺少设备名", "登记采集执行者请求缺少设备名。")
		return
	}
	if err := h.cfg.SoftwareInventory.SetSoftwareInventoryCollector(
		form, SoftwareInventoryWriteSource{RemoteAddress: remoteIP(r)},
	); err != nil {
		if message, ok := softwareInventoryFormMessage(err); ok {
			form.Error = message
			h.renderSoftwareInventoryDevice(w, r, http.StatusBadRequest, form.Device, &form, "", false)
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, "登记采集执行者失败", "登记采集执行者失败，请稍后重试。")
		return
	}
	target := url.Values{"device": {form.Device}, "notice": {"collector-set"}}
	http.Redirect(w, r, "/ui/software-inventory/devices?"+target.Encode(), http.StatusSeeOther)
}

// handleSoftwareInventoryDeviceSudo 处理设备详情高级区的 sudo 采集开关。
// 打开沿用敏感写操作的重新验证要求；重新验证回跳后保留表单状态。
func (h *handler) handleSoftwareInventoryDeviceSudo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "设置 sudo 采集开关只接受 POST 请求。")
		return
	}
	device := strings.TrimSpace(r.PostFormValue("device"))
	if device == "" {
		h.renderError(w, r, http.StatusBadRequest, "缺少设备名", "设置 sudo 采集开关请求缺少设备名。")
		return
	}
	server := strings.TrimSpace(r.PostFormValue("server_entry"))
	enabled := r.PostFormValue("sudo_enabled") == "1"
	if enabled {
		resume := url.Values{
			"device": {device}, "resume": {"1"}, "server_entry": {server}, "sudo_enabled": {"1"},
		}
		if !h.requireRecentReverify(w, r, "/ui/software-inventory/devices?"+resume.Encode()) {
			return
		}
	}
	if err := h.cfg.SoftwareInventory.SetSoftwareInventorySudo(
		server, enabled, SoftwareInventoryWriteSource{RemoteAddress: remoteIP(r)},
	); err != nil {
		if message, ok := softwareInventoryFormMessage(err); ok {
			form := SoftwareInventoryCollectorForm{Device: device, SudoError: message}
			h.renderSoftwareInventoryDevice(w, r, http.StatusBadRequest, device, &form, server, enabled)
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, "设置 sudo 采集开关失败", "设置 sudo 采集开关失败，请稍后重试。")
		return
	}
	state := "sudo-disabled"
	if enabled {
		state = "sudo-enabled"
	}
	target := url.Values{"device": {device}, "notice": {state}}
	http.Redirect(w, r, "/ui/software-inventory/devices?"+target.Encode(), http.StatusSeeOther)
}
