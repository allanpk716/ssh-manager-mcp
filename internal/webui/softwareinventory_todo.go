package webui

import (
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// SoftwareInventoryPendingPort 是一条等待 owner 确认暴露级别的实测端口。
type SoftwareInventoryPendingPort struct {
	Address           string
	Port              int
	Protocol          string
	ExposureLabel     string
	ObservedStableKey string
}

// SoftwareInventoryPendingExposureDevice 是按设备分组的待确认暴露待办。
type SoftwareInventoryPendingExposureDevice struct {
	DeviceName string
	Ports      []SoftwareInventoryPendingPort
}

// SoftwareInventoryUnidentifiedProcess 是等待声明认领的未识别进程。
type SoftwareInventoryUnidentifiedProcess struct {
	DeviceName        string
	Name              string
	ProcessName       string
	ExecutablePath    string
	ObservedStableKey string
	ListeningPorts    []SoftwareInventoryPort
}

// SoftwareInventoryMismatch 是一条没有对应当前实测的生效声明。
type SoftwareInventoryMismatch struct {
	DeviceName    string
	DeclarationID string
	Name          string
	Reason        string
}

// SoftwareInventoryStaleDevice 是一台清单过期或从未成功采集的受管设备。
type SoftwareInventoryStaleDevice struct {
	DeviceName     string
	NeverCollected bool
	LastSuccessAt  *time.Time
}

// SoftwareInventoryTodos 是待办页的三个列表。
type SoftwareInventoryTodos struct {
	PendingExposure       []SoftwareInventoryPendingExposureDevice
	UnidentifiedProcesses []SoftwareInventoryUnidentifiedProcess
	Mismatches            []SoftwareInventoryMismatch
	StaleDevices          []SoftwareInventoryStaleDevice
}

// declarationFormPath 返回从待办进入声明表单的地址，携带设备、端口与实
// 测条目稳定键， owner 提交后即可完成认领。
func declarationFormPath(
	device, observedStableKey, address string, port int, protocol, exposure string,
) string {
	values := url.Values{
		"device":        {device},
		"observed_key":  {observedStableKey},
		"port_address":  {address},
		"port_number":   {strconv.Itoa(port)},
		"port_protocol": {protocol},
		"port_exposure": {exposure},
	}
	return "/ui/software-inventory/declarations/new?" + values.Encode()
}

// handleSoftwareInventoryTodos 渲染三类待办。列表内容来自领域模块，与命
// 令行 `inventory todo` 使用同一个计算结果。
func (h *handler) handleSoftwareInventoryTodos(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "软件清单待办页只接受 GET 请求。")
		return
	}
	todos, err := h.cfg.SoftwareInventory.SoftwareInventoryTodos()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取软件清单待办失败", "读取软件清单待办失败，请稍后重试。")
		return
	}
	data := pageData{Title: "软件清单待办", SoftwareInventoryTodos: todos}
	switch r.URL.Query().Get("notice") {
	case "declaration-added":
		data.SoftwareInventoryNotice = "已登记软件声明。"
	case "declaration-edited":
		data.SoftwareInventoryNotice = "已修改软件声明。"
	case "declaration-claimed":
		data.SoftwareInventoryNotice = "已认领实测条目。"
	}
	h.renderPage(w, r, http.StatusOK, "softwareinventory-todos.tmpl", data)
}
