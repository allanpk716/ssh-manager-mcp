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
	// CategoryCode 是页面分组与搜索折叠共用的展示类别稳定代号。
	CategoryCode  string
	CategoryLabel string
	// ItemCategoryCode 是原始软件类别的稳定代号（如 container、
	// container_image、unidentified_process）。模板按它区分条目形态，
	// 不用中文文案反推类别。
	ItemCategoryCode string
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
	// ContainerName 是容器条目里的容器名；容器镜像条目与旧版采集结
	// 果没有这个值，模板按空值隐藏。
	ContainerName string
	// ImagePullabilityLabel 是镜像能否从公开仓库拉取的中文显示名；
	// 采集结果没有判定时代号为空，这里也保持空字符串。
	ImagePullabilityLabel string
	ContainerTag          string
	ContainerDigest       string
	ContainerRunning      bool
	// ProcessName、ExecutablePath 与 ProcessID 是未识别进程条目的进
	// 程可得信息；采集时看不到的字段为空。
	ProcessName    string
	ExecutablePath string
	ProcessID      int
	// MissingVersion 表示这条条目拿不到版本号，页面要显示缺版本标记。
	MissingVersion bool
}

// SoftwareInventoryGroup 是设备清单页的一个类别分组。系统包默认只带数
// 量；条目仍由服务器渲染，浏览器展开后即可看到全部。
type SoftwareInventoryGroup struct {
	CategoryCode       string
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
	// UnusedImageCount 是设备上不再被任何容器使用的镜像数量；第 2 版
	// 采集结果没有这个值，数量为零时不显示。
	UnusedImageCount int
	// PendingSecurityUpdatesLabel 是待装安全更新数量的显示文本，带缓存
	// 时间；没有包管理器或缓存不可读时为「未知」；不采集待装更新的系统
	// （Windows、macOS）为空，页面不显示这一行。
	PendingSecurityUpdatesLabel string
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

// SoftwareInventoryFilter 是设备清单、全局搜索与导出共享的读取条件。空
// 值表示不限制；地点使用完整路径，网络使用名称。Device 只用于单台设备
// 导出；设备清单与全局搜索不解析它。
type SoftwareInventoryFilter struct {
	Device       string
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
	Filter              SoftwareInventoryFilter
	DailyCollectionTime string
	ScheduleNotice      string
	ScheduleError       string
	CollectionReport    *SoftwareInventoryCollectionRoundResult
	SiteOptions         []SoftwareInventoryOption
	NetworkOptions      []SoftwareInventoryOption
	Devices             []SoftwareInventoryDeviceSummary
	SearchDevices       []SoftwareInventorySearchDevice
	HasSearch           bool
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

// SoftwareInventoryCollectionDeviceResult 是「立即全部采集」里一台设备的
// 页面结论。Err 同时用于同步采集失败与登记客户端请求失败。
type SoftwareInventoryCollectionDeviceResult struct {
	DeviceName    string
	Requested     bool
	CollectorName string
	Err           string
}

// SoftwareInventoryCollectionRoundResult 是「立即全部采集」的页面报告。
type SoftwareInventoryCollectionRoundResult struct {
	Succeeded int
	Requested int
	Failed    int
	// Interrupted 表示这轮被取消，Devices 只包含已处理设备。
	Interrupted bool
	Devices     []SoftwareInventoryCollectionDeviceResult
}

// SoftwareInventoryFormError 表示软件清单表单提交内容校验失败，Message 可
// 直接展示给 owner。
type SoftwareInventoryFormError struct {
	Message string
}

func (e *SoftwareInventoryFormError) Error() string { return e.Message }

func softwareInventoryFormMessage(err error) (string, bool) {
	var formErr *SoftwareInventoryFormError
	if errors.As(err, &formErr) {
		return formErr.Message, true
	}
	return "", false
}

// SoftwareInventorySource 是软件清单页读取与立即采集的唯一入口。单机与
// 权威端由软件清单领域模块适配；客户端不提供。
type SoftwareInventorySource interface {
	SoftwareInventoryDevices(filter SoftwareInventoryFilter) ([]SoftwareInventoryDeviceSummary, error)
	SoftwareInventoryDevice(deviceName string) (*SoftwareInventoryDevice, error)
	SearchSoftwareInventory(filter SoftwareInventoryFilter) ([]SoftwareInventorySearchDevice, error)
	ExportSoftwareInventoryCSV(filter SoftwareInventoryFilter) ([]byte, error)
	SoftwareInventoryTodos() (SoftwareInventoryTodos, error)
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
	SoftwareInventoryDailyTime() (string, error)
	SetSoftwareInventoryDailyTime(value string, source SoftwareInventoryWriteSource) error
	RequestSoftwareInventoryCollectionAll(
		ctx context.Context,
		source SoftwareInventoryWriteSource,
	) (SoftwareInventoryCollectionRoundResult, error)
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
	case "/ui/software-inventory/export":
		h.handleSoftwareInventoryExport(w, r)
	case "/ui/software-inventory/collect":
		h.handleSoftwareInventoryCollect(w, r)
	case "/ui/software-inventory/schedule":
		h.handleSoftwareInventorySchedule(w, r)
	case "/ui/software-inventory/collect-all":
		h.handleSoftwareInventoryCollectAll(w, r)
	default:
		h.renderError(w, r, http.StatusNotFound, "页面不存在", "请求的页面不存在。")
	}
}

// handleSoftwareInventoryExport 导出平铺 CSV 总表。网址带 device 时只导出
// 这台设备；否则带上首页当前的软件名、精确版本、地点与网络条件。下载
// 链接不改变当前页面，筛选状态由首页网址继续保留。
func (h *handler) handleSoftwareInventoryExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "软件清单导出只接受 GET 请求。")
		return
	}
	filter := softwareInventoryFilterFromQuery(r)
	filename := "software-inventory.csv"
	if deviceName := strings.TrimSpace(r.URL.Query().Get("device")); deviceName != "" {
		filter = SoftwareInventoryFilter{Device: deviceName}
		filename = "software-inventory-device.csv"
	}
	blob, err := h.cfg.SoftwareInventory.ExportSoftwareInventoryCSV(filter)
	if err != nil {
		var missing *SoftwareInventoryDeviceNotFoundError
		if errors.As(err, &missing) {
			h.renderError(w, r, http.StatusNotFound, "设备不存在", missing.Error())
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, "导出软件清单失败", "导出软件清单失败，请稍后重试。")
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(blob)
}

// handleSoftwareInventorySchedule 保存全局每天采集时间。调度服务与客户端
// 分派都在下一次读取设置时使用新时间，不需要重启权威端。
func (h *handler) handleSoftwareInventorySchedule(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "保存每天采集时间只接受 POST 请求。")
		return
	}
	value := strings.TrimSpace(r.PostFormValue("daily_time"))
	if err := h.cfg.SoftwareInventory.SetSoftwareInventoryDailyTime(
		value, SoftwareInventoryWriteSource{RemoteAddress: remoteIP(r)},
	); err != nil {
		var formErr *SoftwareInventoryFormError
		if errors.As(err, &formErr) {
			http.Redirect(w, r, "/ui/software-inventory/?schedule=error", http.StatusSeeOther)
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, "保存每天采集时间失败", "保存每天采集时间失败，请稍后重试。")
		return
	}
	http.Redirect(w, r, "/ui/software-inventory/?schedule=saved", http.StatusSeeOther)
}

// handleSoftwareInventoryCollectAll 处理「立即全部采集」。同步结果与等待
// 客户端实例的登记结果先放在会话里的一次性报告，跳回首页后展示。
func (h *handler) handleSoftwareInventoryCollectAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "立即全部采集只接受 POST 请求。")
		return
	}
	report, err := h.cfg.SoftwareInventory.RequestSoftwareInventoryCollectionAll(
		r.Context(), SoftwareInventoryWriteSource{RemoteAddress: remoteIP(r)},
	)
	if err != nil && !(errors.Is(err, context.Canceled) && report.Interrupted) {
		h.renderError(w, r, http.StatusInternalServerError, "立即全部采集失败", "立即全部采集失败，请稍后重试。")
		return
	}
	if sess := h.currentSession(r); sess != nil {
		h.mu.Lock()
		sess.softwareInventoryCollection = &report
		h.mu.Unlock()
	}
	http.Redirect(w, r, "/ui/software-inventory/?collect_all=done", http.StatusSeeOther)
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

func (h *handler) takeSoftwareInventoryCollectionReport(r *http.Request) *SoftwareInventoryCollectionRoundResult {
	if r.URL.Query().Get("collect_all") != "done" {
		return nil
	}
	sess := h.currentSession(r)
	if sess == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	report := sess.softwareInventoryCollection
	sess.softwareInventoryCollection = nil
	return report
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
	dailyTime, err := h.cfg.SoftwareInventory.SoftwareInventoryDailyTime()
	if err != nil {
		return nil, err
	}
	view.DailyCollectionTime = dailyTime
	switch r.URL.Query().Get("schedule") {
	case "saved":
		view.ScheduleNotice = "每天采集时间已保存。"
	case "error":
		view.ScheduleError = "每天采集时间保存失败，请核对 24 小时制的「小时:分钟」写法。"
	}
	view.CollectionReport = h.takeSoftwareInventoryCollectionReport(r)
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
