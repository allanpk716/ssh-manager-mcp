package webui

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SoftwareInventoryPort 是页面上展示的一个监听端口。
type SoftwareInventoryPort struct {
	Address       string
	Port          int
	Protocol      string
	ExposureLabel string
}

// SoftwareInventoryItem 是软件清单页的一条软件条目。版本与标识原样保留；
// 实测条目带采集时间，声明条目带确认时间。
type SoftwareInventoryItem struct {
	CategoryLabel    string
	Name             string
	Version          string
	PackageURL       string
	CPE              string
	Source           string
	CollectedAt      *time.Time
	ConfirmedAt      *time.Time
	ListeningPorts   []SoftwareInventoryPort
	BelongsTo        string
	Note             string
	Architecture     string
	ContainerTag     string
	ContainerDigest  string
	ContainerRunning bool
}

// SoftwareInventoryGroup 是设备清单页的一个类别分组。系统包默认只带数
// 量；条目仍由服务器渲染，浏览器展开后即可看到全部。
type SoftwareInventoryGroup struct {
	CategoryLabel      string
	Items              []SoftwareInventoryItem
	SystemPackageCount int
	SystemPackages     bool
}

// SoftwareInventoryStatus 是一台设备的采集状态。
type SoftwareInventoryStatus struct {
	CollectorLabel    string
	ServerEntry       string
	LastSuccessAt     *time.Time
	LastFailureAt     *time.Time
	LastFailureReason string
	Incomplete        bool
	NeverCollected    bool
	Stale             bool
	PendingImmediate  bool
}

// SoftwareInventoryChange 是一条变化历史。
type SoftwareInventoryChange struct {
	OccurredAt    time.Time
	KindLabel     string
	CategoryLabel string
	Name          string
	OldValue      string
	NewValue      string
}

// SoftwareInventoryDevice 是一台设备的完整页面数据。
type SoftwareInventoryDevice struct {
	Name    string
	Status  SoftwareInventoryStatus
	Groups  []SoftwareInventoryGroup
	History []SoftwareInventoryChange
}

// SoftwareInventoryDeviceSummary 是软件清单首页的一台设备。
type SoftwareInventoryDeviceSummary struct {
	Name   string
	Status SoftwareInventoryStatus
}

// SoftwareInventorySearchRow 是跨设备搜索结果中的一行。
type SoftwareInventorySearchRow struct {
	DeviceName string
	Item       SoftwareInventoryItem
}

// SoftwareInventoryWriteSource 描述软件清单写请求来源，适配层写审计时使用。
type SoftwareInventoryWriteSource struct {
	RemoteAddress string
}

// SoftwareInventoryDeviceNotFoundError 表示请求的设备不存在或不可见。
type SoftwareInventoryDeviceNotFoundError struct {
	DeviceName string
}

func (e *SoftwareInventoryDeviceNotFoundError) Error() string {
	return "设备 " + e.DeviceName + " 不存在或不可见"
}

// SoftwareInventoryCollectionResult 表示立即采集请求的结果。Requested 为
// true 表示只登记了请求，等待客户端实例执行。
type SoftwareInventoryCollectionResult struct {
	Requested bool
}

// SoftwareInventorySource 是软件清单页读取与立即采集的唯一入口。单机与
// 权威端由软件清单领域模块适配；客户端不提供。
type SoftwareInventorySource interface {
	SoftwareInventoryDevices() ([]SoftwareInventoryDeviceSummary, error)
	SoftwareInventoryDevice(deviceName string) (*SoftwareInventoryDevice, error)
	SearchSoftwareInventory(namePart string) ([]SoftwareInventorySearchRow, error)
	RequestSoftwareInventoryCollection(
		ctx context.Context,
		deviceName string,
		source SoftwareInventoryWriteSource,
	) (SoftwareInventoryCollectionResult, error)
}

// registerSoftwareInventoryRoutes 注册软件清单页路由。客户端不注册任何
// 软件清单地址。
func (h *handler) registerSoftwareInventoryRoutes() {
	if h.cfg.Role == RoleClient || h.cfg.SoftwareInventory == nil {
		return
	}
	h.mux.HandleFunc("/ui/software-inventory/", h.requireSession(h.handleSoftwareInventory))
}

// handleSoftwareInventory 按路径分发软件清单页面与立即采集表单。
func (h *handler) handleSoftwareInventory(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/ui/software-inventory/":
		h.handleSoftwareInventoryRoot(w, r)
	case "/ui/software-inventory/devices":
		h.handleSoftwareInventoryDevice(w, r)
	case "/ui/software-inventory/search":
		h.handleSoftwareInventorySearch(w, r)
	case "/ui/software-inventory/collect":
		h.handleSoftwareInventoryCollect(w, r)
	default:
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
	}
}

// handleSoftwareInventoryCollect 处理立即采集表单。本实例采集时同步执行；
// 客户端实例作为采集执行者时，来源接口只登记请求并返回等待提示。
func (h *handler) handleSoftwareInventoryCollect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "立即采集只接受 POST 请求。")
		return
	}
	deviceName := strings.TrimSpace(r.URL.Query().Get("device"))
	if deviceName == "" {
		h.renderError(w, r, http.StatusBadRequest, "缺少设备名", "立即采集请求缺少设备名。")
		return
	}
	result, err := h.cfg.SoftwareInventory.RequestSoftwareInventoryCollection(
		r.Context(), deviceName, SoftwareInventoryWriteSource{RemoteAddress: remoteIP(r)},
	)
	if err != nil {
		var missing *SoftwareInventoryDeviceNotFoundError
		if errors.As(err, &missing) {
			h.renderError(w, r, http.StatusNotFound, "设备不存在", missing.Error())
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, "立即采集失败", "立即采集失败，请稍后重试。")
		return
	}
	state := "done"
	if result.Requested {
		state = "requested"
	}
	target := url.Values{"device": {deviceName}, "collect": {state}}
	http.Redirect(w, r, "/ui/software-inventory/devices?"+target.Encode(), http.StatusSeeOther)
}

// handleSoftwareInventorySearch 渲染跨设备搜索结果。空关键字只显示表单，
// 不读取全部清单。
func (h *handler) handleSoftwareInventorySearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "软件清单搜索页只接受 GET 请求。")
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("query"))
	data := pageData{Title: "软件清单搜索", SoftwareInventoryQuery: query}
	if query != "" {
		rows, err := h.cfg.SoftwareInventory.SearchSoftwareInventory(query)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "搜索软件清单失败", "搜索软件清单失败，请稍后重试。")
			return
		}
		data.SoftwareInventorySearchRows = rows
	}
	h.renderPage(w, r, http.StatusOK, "softwareinventory-search.tmpl", data)
}

// handleSoftwareInventoryRoot 渲染设备列表。
func (h *handler) handleSoftwareInventoryRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "软件清单页只接受 GET 请求。")
		return
	}
	devices, err := h.cfg.SoftwareInventory.SoftwareInventoryDevices()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取软件清单失败", "读取软件清单失败，请稍后重试。")
		return
	}
	h.renderPage(w, r, http.StatusOK, "softwareinventory.tmpl", pageData{
		Title: "软件清单", SoftwareInventoryDevices: devices,
	})
}

// handleSoftwareInventoryDevice 渲染一台设备的软件清单、采集状态与变化
// 历史。
func (h *handler) handleSoftwareInventoryDevice(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "设备软件清单页只接受 GET 请求。")
		return
	}
	deviceName := strings.TrimSpace(r.URL.Query().Get("device"))
	if deviceName == "" {
		http.Redirect(w, r, "/ui/software-inventory/", http.StatusSeeOther)
		return
	}
	device, err := h.cfg.SoftwareInventory.SoftwareInventoryDevice(deviceName)
	if err != nil {
		var missing *SoftwareInventoryDeviceNotFoundError
		if errors.As(err, &missing) {
			h.renderError(w, r, http.StatusNotFound, "设备不存在", missing.Error())
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, "读取设备软件清单失败", "读取设备软件清单失败，请稍后重试。")
		return
	}
	data := pageData{Title: device.Name + " · 软件清单", SoftwareInventoryDevice: device}
	switch r.URL.Query().Get("collect") {
	case "done":
		data.SoftwareInventoryNotice = "已采集完成。"
	case "requested":
		data.SoftwareInventoryNotice = "已请求，等待客户端执行。"
	}
	h.renderPage(w, r, http.StatusOK, "softwareinventory-device.tmpl", data)
}
