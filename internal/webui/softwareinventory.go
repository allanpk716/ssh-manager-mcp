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
	CategoryLabel string
	// SearchCategoryLabel 是全局搜索默认视图的类别文案；操作系统条目与
	// 内核条目在同一页合成「操作系统与内核」。
	SearchCategoryLabel string
	Name                string
	Version             string
	PackageURL          string
	CPE                 string
	Source              string
	CollectedAt         *time.Time
	ConfirmedAt         *time.Time
	ListeningPorts      []SoftwareInventoryPort
	BelongsTo           string
	Note                string
	Architecture        string
	ContainerTag        string
	ContainerDigest     string
	ContainerRunning    bool
	// ProcessName、ExecutablePath 与 ProcessID 是未识别进程条目的进
	// 程可得信息；采集时看不到的字段为空。
	ProcessName    string
	ExecutablePath string
	ProcessID      int
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
	Managed bool
	// Collector 是客户端实例执行者的设备码编号；空串表示权威端执行
	// 采集。高级区用它区分当前选择。
	Collector            string
	CollectorName        string
	CollectorLabel       string
	PreferredServerEntry string
	ServerEntry          string
	LastSuccessAt        *time.Time
	LastFailureAt        *time.Time
	LastFailureReason    string
	Incomplete           bool
	NeverCollected       bool
	Stale                bool
	PendingImmediate     bool
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
	Name   string
	Status SoftwareInventoryStatus
	// Summary 与首页设备卡片使用同一套摘要与状态口径。
	Summary SoftwareInventoryDeviceSummary
	Groups  []SoftwareInventoryGroup
	// Advanced 是底部默认折叠的高级采集设置数据；不受管设备为空。
	Advanced SoftwareInventoryDeviceAdvanced
	History  []SoftwareInventoryChange
}

// SoftwareInventoryDeviceServerEntry 是设备详情高级区里一台可用来采集的
// 服务器条目。
type SoftwareInventoryDeviceServerEntry struct {
	Name        string
	SudoEnabled bool
}

// SoftwareInventoryDeviceAdvanced 是设备详情底部高级采集设置的展示数
// 据：可切换的采集执行者与这台设备的采集用服务器条目。
type SoftwareInventoryDeviceAdvanced struct {
	ClientInstances []SoftwareInventoryClientInstance
	ServerEntries   []SoftwareInventoryDeviceServerEntry
}

// SoftwareInventoryDeviceSummary 是软件清单首页的一台设备。
type SoftwareInventoryDeviceSummary struct {
	Name                     string
	SystemName               string
	SystemVersion            string
	KernelName               string
	KernelVersion            string
	ImportantSoftwareCount   int
	AllSoftwareCount         int
	UnidentifiedProcessCount int
	Status                   SoftwareInventoryStatus
	StatusLabels             []string
}

// SoftwareInventoryFilter 是设备清单与全局搜索共享的读取条件。空值表示
// 不限制；地点使用完整路径，网络使用名称。
type SoftwareInventoryFilter struct {
	Name         string
	ExactVersion string
	Site         string
	Network      string
}

// SoftwareInventorySearchDevice 是全局搜索结果里的一台设备。Rows 保持全
// 部匹配条目，模板把系统包放入默认折叠区域。
type SoftwareInventorySearchDevice struct {
	DeviceName         string
	Rows               []SoftwareInventorySearchRow
	SystemPackageCount int
}

// SoftwareInventoryOption 是首页筛选的一个可选项。Value 使用领域查询接
// 受的地点完整路径或网络名称。
type SoftwareInventoryOption struct {
	Value    string
	Label    string
	Selected bool
}

// SoftwareInventoryPageView 是设备优先首页的全部页面数据。设备列表与搜索
// 结果共享同一组条件，保证两组结果的地点与网络口径一致。
type SoftwareInventoryPageView struct {
	Filter         SoftwareInventoryFilter
	SiteOptions    []SoftwareInventoryOption
	NetworkOptions []SoftwareInventoryOption
	Devices        []SoftwareInventoryDeviceSummary
	SearchDevices  []SoftwareInventorySearchDevice
	HasSearch      bool
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

// SoftwareInventoryFormError 表示软件清单表单提交内容校验失败，Message 可
// 直接展示给 owner。
type SoftwareInventoryFormError struct {
	Message string
}

func (e *SoftwareInventoryFormError) Error() string { return e.Message }

// SoftwareInventorySource 是软件清单页读取与立即采集的唯一入口。单机与
// 权威端由软件清单领域模块适配；客户端不提供。
type SoftwareInventorySource interface {
	SoftwareInventoryDevices(filter SoftwareInventoryFilter) ([]SoftwareInventoryDeviceSummary, error)
	SoftwareInventoryDevice(deviceName string) (*SoftwareInventoryDevice, error)
	SearchSoftwareInventory(filter SoftwareInventoryFilter) ([]SoftwareInventorySearchDevice, error)
	SoftwareInventoryTodos() (SoftwareInventoryTodos, error)
	SoftwareInventoryDeclaration(id string) (*SoftwareInventoryDeclaration, error)
	SoftwareInventoryDeclarations() ([]SoftwareInventoryDeclaration, error)
	AddSoftwareInventoryDeclaration(
		form SoftwareInventoryDeclarationForm,
		source SoftwareInventoryWriteSource,
	) (string, error)
	EditSoftwareInventoryDeclaration(
		id string,
		form SoftwareInventoryDeclarationForm,
		source SoftwareInventoryWriteSource,
	) error
	ClaimSoftwareInventoryDeclaration(
		id, observedStableKey string,
		source SoftwareInventoryWriteSource,
	) error
	DeleteSoftwareInventoryDeclaration(id string, source SoftwareInventoryWriteSource) error
	SoftwareInventoryDrafts() ([]SoftwareInventoryDraft, error)
	ConfirmSoftwareInventoryDraft(
		id string,
		form *SoftwareInventoryDeclarationForm,
		source SoftwareInventoryWriteSource,
	) error
	RejectSoftwareInventoryDraft(id string, source SoftwareInventoryWriteSource) error
	SetSoftwareInventoryCollector(
		form SoftwareInventoryCollectorForm,
		source SoftwareInventoryWriteSource,
	) error
	SetSoftwareInventorySudo(
		server string, enabled bool, source SoftwareInventoryWriteSource,
	) error
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
	case "/ui/software-inventory/devices/collector":
		h.handleSoftwareInventoryDeviceCollector(w, r)
	case "/ui/software-inventory/devices/sudo":
		h.handleSoftwareInventoryDeviceSudo(w, r)
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
	if returnTo := safeReturnPath(r.PostFormValue("return_to"), "", "/ui/servers/"); returnTo != "" {
		separator := "&"
		if !strings.Contains(returnTo, "?") {
			separator = "?"
		}
		http.Redirect(w, r, returnTo+separator+"collect="+state, http.StatusSeeOther)
		return
	}
	target := url.Values{"device": {deviceName}, "collect": {state}}
	http.Redirect(w, r, "/ui/software-inventory/devices?"+target.Encode(), http.StatusSeeOther)
}

// handleSoftwareInventorySearch 把旧搜索地址跳转回设备优先首页，并保留
// 全部查询条件。
func (h *handler) handleSoftwareInventorySearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "软件清单搜索页只接受 GET 请求。")
		return
	}
	http.Redirect(w, r, "/ui/software-inventory/?"+r.URL.Query().Encode(), http.StatusSeeOther)
}

// handleSoftwareInventoryRoot 渲染设备列表。
func (h *handler) handleSoftwareInventoryRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "软件清单页只接受 GET 请求。")
		return
	}
	filter := softwareInventoryFilterFromQuery(r)
	view, err := h.softwareInventoryPageView(r, filter)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取软件清单失败", "读取软件清单失败，请稍后重试。")
		return
	}
	h.renderPage(w, r, http.StatusOK, "softwareinventory.tmpl", pageData{
		Title: "软件清单", SoftwareInventory: view,
	})
}

func softwareInventoryFilterFromQuery(r *http.Request) SoftwareInventoryFilter {
	return SoftwareInventoryFilter{
		Name:         strings.TrimSpace(r.URL.Query().Get("query")),
		ExactVersion: strings.TrimSpace(r.URL.Query().Get("exact_version")),
		Site:         strings.TrimSpace(r.URL.Query().Get("site")),
		Network:      strings.TrimSpace(r.URL.Query().Get("network")),
	}
}

func (h *handler) softwareInventoryPageView(
	r *http.Request, filter SoftwareInventoryFilter,
) (*SoftwareInventoryPageView, error) {
	view := &SoftwareInventoryPageView{Filter: filter}
	devices, err := h.cfg.SoftwareInventory.SoftwareInventoryDevices(filter)
	if err != nil {
		return nil, err
	}
	view.Devices = devices
	if filter.Name != "" || filter.ExactVersion != "" {
		view.HasSearch = true
		rows, err := h.cfg.SoftwareInventory.SearchSoftwareInventory(filter)
		if err != nil {
			return nil, err
		}
		view.SearchDevices = rows
	}
	if h.cfg.Topology == nil {
		return view, nil
	}
	topology, err := h.cfg.Topology.Topology(h.requestClientInstance(r), "")
	if err != nil {
		return nil, err
	}
	for _, site := range topology.Sites {
		if site.LocationPath == "" {
			continue
		}
		view.SiteOptions = append(view.SiteOptions, SoftwareInventoryOption{
			Value: site.LocationPath, Label: site.LocationPath, Selected: site.LocationPath == filter.Site,
		})
	}
	for _, network := range topology.Networks {
		if network.Name == "" {
			continue
		}
		view.NetworkOptions = append(view.NetworkOptions, SoftwareInventoryOption{
			Value:    network.Name,
			Label:    network.Name + "（" + networkTypeLabel(network.Type) + "）",
			Selected: network.Name == filter.Network,
		})
	}
	return view, nil
}

// handleSoftwareInventoryDevice 渲染一台设备的软件清单摘要、类别分组、
// 采集状态与高级采集设置。
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
	h.renderSoftwareInventoryDevice(w, r, http.StatusOK, deviceName, nil, "", false)
}

// renderSoftwareInventoryDevice 读取设备页面数据并渲染。form 非空时表示
// 高级设置提交失败后回显；否则用当前登记状态填充默认选择。
func (h *handler) renderSoftwareInventoryDevice(
	w http.ResponseWriter, r *http.Request, status int,
	deviceName string, form *SoftwareInventoryCollectorForm,
	sudoServer string, sudoEnabled bool,
) {
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
	if form == nil {
		form = &SoftwareInventoryCollectorForm{
			Device: device.Name, Collector: device.Status.CollectorName,
			ServerEntry: device.Status.PreferredServerEntry,
		}
	}
	form.InvalidCollector = form.Collector != "" &&
		!softwareInventoryClientKnown(device.Advanced.ClientInstances, form.Collector)
	data := pageData{Title: device.Name + " · 软件清单", SoftwareInventoryDevice: device}
	query := r.URL.Query()
	switch r.URL.Query().Get("collect") {
	case "done":
		data.SoftwareInventoryNotice = "已采集完成。"
	case "requested":
		data.SoftwareInventoryNotice = "已请求，等待客户端执行。"
	}
	switch query.Get("notice") {
	case "collector-set":
		data.SoftwareInventoryNotice = "采集执行者已登记。"
	case "sudo-enabled":
		data.SoftwareInventoryNotice = "sudo 采集开关已打开。"
	case "sudo-disabled":
		data.SoftwareInventoryNotice = "sudo 采集开关已关闭。"
	}
	if query.Get("resume") == "1" {
		data.SoftwareInventoryResumeNotice = "已保留表单状态，请再次提交。"
		sudoServer = query.Get("server_entry")
		sudoEnabled = query.Get("sudo_enabled") == "1"
	}
	data.SoftwareInventoryCollectorForm = form
	data.SoftwareInventoryAdvancedOpen = form.Error != "" || form.SudoError != ""
	data.SoftwareInventorySudoServer = sudoServer
	data.SoftwareInventorySudoEnabled = sudoEnabled
	h.renderPage(w, r, status, "softwareinventory-device.tmpl", data)
}

// softwareInventoryClientKnown 判断提交的客户端实例是否仍在有效列表里。
func softwareInventoryClientKnown(
	instances []SoftwareInventoryClientInstance, name string,
) bool {
	for _, instance := range instances {
		if instance.Name == name {
			return true
		}
	}
	return false
}
