package webui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestSoftwareInventoryNavFollowsCapability 覆盖软件清单导航入口：只有可写
// 角色配置了软件清单来源时显示；客户端即使误配置来源也不显示。
func TestSoftwareInventoryNavFollowsCapability(t *testing.T) {
	pageBody := func(role Role, enabled bool) string {
		cfg := Config{
			Role:        role,
			Servers:     &stubServerSource{},
			Access:      &stubAccessStore{},
			VersionHint: func() string { return "" },
			Port:        18080,
		}
		if role == RoleClient {
			cfg.Access = nil
		}
		if enabled {
			cfg.SoftwareInventory = &stubSoftwareInventorySource{}
		}
		handler, startup, err := New(cfg)
		if err != nil {
			t.Fatalf("构造 Web UI Handler 失败：%v", err)
		}
		resp := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
		cookies := resp.Cookies()
		if len(cookies) != 1 {
			t.Fatalf("应签发会话 cookie，实际 %d", len(cookies))
		}
		page := doGetWithCookie(t, handler, cookies[0], "/ui/")
		body := readResponseBody(t, page)
		return body
	}

	if body := pageBody(RoleStandalone, true); !strings.Contains(body, `aria-label="一级导航"`) ||
		!strings.Contains(body, `>更多<`) {
		t.Fatalf("单机配置软件清单来源后应显示更多标签：\n%s", body)
	}
	if body := pageBody(RoleStandalone, false); strings.Contains(body, `href="/ui/software-inventory/"`) {
		t.Fatalf("未配置软件清单来源时不应显示导航入口：\n%s", body)
	}
	if body := pageBody(RoleClient, true); strings.Contains(body, `href="/ui/software-inventory/"`) {
		t.Fatalf("客户端 Web UI 不应显示软件清单导航入口：\n%s", body)
	}
}

type stubSoftwareInventorySource struct {
	device             *SoftwareInventoryDevice
	deviceErr          error
	devices            []SoftwareInventoryDeviceSummary
	deviceFilter       SoftwareInventoryFilter
	searchRows         []SoftwareInventorySearchRow
	searchDevices      []SoftwareInventorySearchDevice
	searchQuery        string
	searchFilter       SoftwareInventoryFilter
	collectResult      SoftwareInventoryCollectionResult
	collectAllResult   SoftwareInventoryCollectionRoundResult
	collectErr         error
	collectDevice      string
	collectAddress     string
	collectCalled      bool
	dailyTimeValue     string
	dailyTimeAddress   string
	dailyTimeSetCalled bool
	collectAllAddress  string
	collectAllCalled   bool
	todos              SoftwareInventoryTodos
	declaration        *SoftwareInventoryDeclaration
	addedForm          SoftwareInventoryDeclarationForm
	addAddress         string
	addCalled          bool
	addID              string
	editCalled         bool
	editID             string
	editForm           SoftwareInventoryDeclarationForm
	editAddress        string
	claimCalled        bool
	claimID            string
	claimKey           string
	claimAddress       string
	deleteCalled       bool
	deleteID           string
	deleteAddress      string
	drafts             []SoftwareInventoryDraft
	confirmID          string
	confirmForm        *SoftwareInventoryDeclarationForm
	confirmAddress     string
	confirmCalled      bool
	rejectID           string
	rejectAddress      string
	rejectCalled       bool
	settings           SoftwareInventorySettings
	declarations       []SoftwareInventoryDeclaration
	collectorForm      SoftwareInventoryCollectorForm
	collectorErr       error
	collectorAddr      string
	collectorCall      bool
	sudoServer         string
	sudoEnabled        bool
	sudoErr            error
	sudoAddress        string
	sudoCalled         bool
}

func (s *stubSoftwareInventorySource) SoftwareInventoryDevices(
	filter SoftwareInventoryFilter,
) ([]SoftwareInventoryDeviceSummary, error) {
	s.deviceFilter = filter
	return s.devices, nil
}

func (s *stubSoftwareInventorySource) SoftwareInventoryDevice(deviceName string) (*SoftwareInventoryDevice, error) {
	if s.deviceErr != nil {
		return nil, s.deviceErr
	}
	if s.device == nil || s.device.Name != deviceName {
		return nil, &SoftwareInventoryDeviceNotFoundError{DeviceName: deviceName}
	}
	return s.device, nil
}

func (s *stubSoftwareInventorySource) SearchSoftwareInventory(
	filter SoftwareInventoryFilter,
) ([]SoftwareInventorySearchDevice, error) {
	s.searchQuery = filter.Name
	s.searchFilter = filter
	return s.searchDevices, nil
}

func (s *stubSoftwareInventorySource) RequestSoftwareInventoryCollection(
	_ context.Context, deviceName string, source SoftwareInventoryWriteSource,
) (SoftwareInventoryCollectionResult, error) {
	s.collectCalled = true
	s.collectDevice = deviceName
	s.collectAddress = source.RemoteAddress
	return s.collectResult, s.collectErr
}

func (s *stubSoftwareInventorySource) SoftwareInventoryDailyTime() (string, error) {
	if s.dailyTimeValue == "" {
		return "01:00", nil
	}
	return s.dailyTimeValue, nil
}

func (s *stubSoftwareInventorySource) SetSoftwareInventoryDailyTime(
	value string, source SoftwareInventoryWriteSource,
) error {
	s.dailyTimeSetCalled = true
	s.dailyTimeValue = value
	s.dailyTimeAddress = source.RemoteAddress
	return nil
}

func (s *stubSoftwareInventorySource) RequestSoftwareInventoryCollectionAll(
	_ context.Context, source SoftwareInventoryWriteSource,
) (SoftwareInventoryCollectionRoundResult, error) {
	s.collectAllCalled = true
	s.collectAllAddress = source.RemoteAddress
	return s.collectAllResult, nil
}

func (s *stubSoftwareInventorySource) SoftwareInventoryTodos() (SoftwareInventoryTodos, error) {
	return s.todos, nil
}

func (s *stubSoftwareInventorySource) SoftwareInventoryDeclaration(id string) (*SoftwareInventoryDeclaration, error) {
	if s.declaration == nil || s.declaration.ID != id {
		return nil, &SoftwareInventoryDeclarationNotFoundError{ID: id}
	}
	return s.declaration, nil
}

func (s *stubSoftwareInventorySource) SoftwareInventoryDeclarations() ([]SoftwareInventoryDeclaration, error) {
	return s.declarations, nil
}

func (s *stubSoftwareInventorySource) AddSoftwareInventoryDeclaration(
	form SoftwareInventoryDeclarationForm, source SoftwareInventoryWriteSource,
) (string, error) {
	s.addCalled = true
	s.addedForm = form
	s.addAddress = source.RemoteAddress
	return s.addID, nil
}

func (s *stubSoftwareInventorySource) EditSoftwareInventoryDeclaration(
	id string, form SoftwareInventoryDeclarationForm, source SoftwareInventoryWriteSource,
) error {
	s.editCalled = true
	s.editID = id
	s.editForm = form
	s.editAddress = source.RemoteAddress
	return nil
}

func (s *stubSoftwareInventorySource) ClaimSoftwareInventoryDeclaration(
	id, observedStableKey string, source SoftwareInventoryWriteSource,
) error {
	s.claimCalled = true
	s.claimID = id
	s.claimKey = observedStableKey
	s.claimAddress = source.RemoteAddress
	return nil
}

func (s *stubSoftwareInventorySource) DeleteSoftwareInventoryDeclaration(
	id string, source SoftwareInventoryWriteSource,
) error {
	s.deleteCalled = true
	s.deleteID = id
	s.deleteAddress = source.RemoteAddress
	return nil
}

func (s *stubSoftwareInventorySource) SoftwareInventoryDrafts() ([]SoftwareInventoryDraft, error) {
	return s.drafts, nil
}

func (s *stubSoftwareInventorySource) ConfirmSoftwareInventoryDraft(
	id string, form *SoftwareInventoryDeclarationForm, source SoftwareInventoryWriteSource,
) error {
	s.confirmCalled = true
	s.confirmID = id
	s.confirmForm = form
	s.confirmAddress = source.RemoteAddress
	return nil
}

func (s *stubSoftwareInventorySource) RejectSoftwareInventoryDraft(
	id string, source SoftwareInventoryWriteSource,
) error {
	s.rejectCalled = true
	s.rejectID = id
	s.rejectAddress = source.RemoteAddress
	return nil
}

func (s *stubSoftwareInventorySource) SoftwareInventorySettings() (SoftwareInventorySettings, error) {
	return s.settings, nil
}

func (s *stubSoftwareInventorySource) SetSoftwareInventoryCollector(
	form SoftwareInventoryCollectorForm, source SoftwareInventoryWriteSource,
) error {
	s.collectorCall = true
	s.collectorForm = form
	s.collectorAddr = source.RemoteAddress
	return s.collectorErr
}

func (s *stubSoftwareInventorySource) SetSoftwareInventorySudo(
	server string, enabled bool, source SoftwareInventoryWriteSource,
) error {
	s.sudoCalled = true
	s.sudoServer = server
	s.sudoEnabled = enabled
	s.sudoAddress = source.RemoteAddress
	return s.sudoErr
}

func TestSoftwareInventoryRootShowsDeviceCardsFiltersAndSearch(t *testing.T) {
	collectedAt := time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)
	source := &stubSoftwareInventorySource{
		devices: []SoftwareInventoryDeviceSummary{{
			Name: "工作站", SystemName: "Ubuntu", SystemVersion: "24.04.1 LTS",
			KernelName: "Linux 内核", KernelVersion: "6.8.0-49-generic",
			ImportantSoftwareCount: 4, AllSoftwareCount: 6, UnidentifiedProcessCount: 1,
			Status: SoftwareInventoryStatus{
				Managed: true, CollectorLabel: "权威端", LastSuccessAt: &collectedAt,
			},
			StatusLabels: []string{"正常"},
		}},
		searchDevices: []SoftwareInventorySearchDevice{{
			DeviceName: "工作站", SystemPackageCount: 1,
			Rows: []SoftwareInventorySearchRow{
				{DeviceName: "工作站", Item: SoftwareInventoryItem{
					CategoryLabel: "服务程序", Name: "openssl-service", Version: "3.0.2-0ubuntu1.18",
					Source: "实测", CollectedAt: &collectedAt,
				}},
				{DeviceName: "工作站", Item: SoftwareInventoryItem{
					CategoryLabel: "系统包", Name: "openssl", Version: "3.0.2-0ubuntu1.18",
					Source: "实测", CollectedAt: &collectedAt,
				}},
			},
		}},
	}
	handler, cookie := newSoftwareInventoryHandler(t, RoleStandalone, source)
	response := doGetWithCookie(t, handler, cookie,
		"/ui/software-inventory/?query=openssl&exact_version=3.0.2-0ubuntu1.18&site=%E5%85%AC%E5%8F%B8&network=%E6%80%BB%E9%83%A8%E7%89%A9%E7%90%86%E5%B1%80%E5%9F%9F%E7%BD%91")
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("软件清单首页状态码 = %d，应为 200：\n%s", response.StatusCode, body)
	}
	wantedFilter := SoftwareInventoryFilter{
		Name: "openssl", ExactVersion: "3.0.2-0ubuntu1.18",
		Site: "公司", Network: "总部物理局域网",
	}
	if source.deviceFilter != wantedFilter {
		t.Fatalf("设备列表条件不符：%+v", source.deviceFilter)
	}
	if source.searchFilter != wantedFilter {
		t.Fatalf("搜索条件应与设备列表一致：%+v", source.searchFilter)
	}
	for _, wanted := range []string{
		"<h1>软件清单</h1>", "先看设备，再用全局搜索跨设备找软件。",
		`action="/ui/software-inventory/"`, `name="query"`, `value="openssl"`,
		`name="exact_version"`, `value="3.0.2-0ubuntu1.18"`,
		`name="site"`, `value="公司"`, `selected`, `name="network"`, `value="总部物理局域网"`,
		"物理局域网", "虚拟局域网", "设备清单", "工作站", "Ubuntu 24.04.1 LTS",
		"Linux 内核 6.8.0-49-generic", "重点软件", "<dd>4</dd>", "全部软件", "<dd>6</dd>",
		"未识别进程", "<dd>1</dd>", "采集时间", "2026-11-20 09:30:00", "正常", "立即采集",
		"跨设备搜索结果", "openssl-service", "全部软件包（1 条，默认折叠）", "展开全部软件包",
	} {
		if !strings.Contains(body, wanted) {
			t.Fatalf("软件清单首页缺少 %q：\n%s", wanted, body)
		}
	}
	if strings.Contains(body, "<details open") {
		t.Fatalf("全部软件包应默认折叠：\n%s", body)
	}
}

// TestSoftwareInventorySearchRedirectsToRoot 钉住搜索入口合并后的地址口
// 径：旧搜索地址只负责跳转首页，并保留全部查询条件。
func TestSoftwareInventorySearchRedirectsToRoot(t *testing.T) {
	source := &stubSoftwareInventorySource{}
	handler, cookie := newSoftwareInventoryHandler(t, RoleStandalone, source)
	response := doGetWithCookie(t, handler, cookie,
		"/ui/software-inventory/search?query=openssl&exact_version=3.0.2&site=%E5%85%AC%E5%8F%B8")
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("搜索地址应跳转首页，状态码=%d：\n%s", response.StatusCode, body)
	}
	if location := response.Header.Get("Location"); location !=
		"/ui/software-inventory/?exact_version=3.0.2&query=openssl&site=%E5%85%AC%E5%8F%B8" {
		t.Fatalf("搜索跳转应保留条件：%s", location)
	}
}

// TestSoftwareInventoryWithdrawsOldReadWritePages 钉住旧页面地址的撤下语
// 义：读取地址不再渲染旧流程，写入地址也不再调用来源接口。
func TestSoftwareInventoryWithdrawsOldReadWritePages(t *testing.T) {
	source := &stubSoftwareInventorySource{
		devices: []SoftwareInventoryDeviceSummary{{
			Name: "工作站", StatusLabels: []string{"正常"},
		}},
	}
	handler, cookie := newSoftwareInventoryHandler(t, RoleStandalone, source)
	home := doGetWithCookie(t, handler, cookie, "/ui/software-inventory/")
	csrf := hiddenInputMetaOrHiddenValue(t, readResponseBody(t, home))
	for _, path := range []string{
		"/ui/software-inventory/todos",
		"/ui/software-inventory/declarations",
		"/ui/software-inventory/declarations/new",
		"/ui/software-inventory/declarations/edit",
		"/ui/software-inventory/declarations/delete",
		"/ui/software-inventory/drafts",
		"/ui/software-inventory/settings",
	} {
		response := doGetWithCookie(t, handler, cookie, path)
		body := readResponseBody(t, response)
		if response.StatusCode != http.StatusNotFound || !strings.Contains(body, "页面不存在") {
			t.Fatalf("旧读取地址 %s 应返回 404，实际 %d：\n%s", path, response.StatusCode, body)
		}
	}
	for _, path := range []string{
		"/ui/software-inventory/declarations/new",
		"/ui/software-inventory/declarations/edit",
		"/ui/software-inventory/declarations/delete",
		"/ui/software-inventory/drafts/confirm",
		"/ui/software-inventory/drafts/reject",
		"/ui/software-inventory/settings/collector",
		"/ui/software-inventory/settings/sudo",
	} {
		response := postSoftwareInventoryForm(t, handler, cookie, path, url.Values{
			"csrf_token": {csrf},
		})
		body := readResponseBody(t, response)
		if response.StatusCode != http.StatusNotFound || !strings.Contains(body, "页面不存在") {
			t.Fatalf("旧写入地址 %s 应返回 404，实际 %d：\n%s", path, response.StatusCode, body)
		}
	}
	if source.addCalled || source.editCalled || source.claimCalled || source.deleteCalled ||
		source.confirmCalled || source.rejectCalled || source.collectorCall || source.sudoCalled {
		t.Fatalf("旧写入地址不应调用来源接口：%+v", source)
	}
}

// TestSoftwareInventoryNavigationUsesMeasuredDeviceSummary 钉住首页、更多
// 菜单与软件清单区域的入口文案：不再暴露旧流程词，摘要改为设备口径。
func TestSoftwareInventoryNavigationUsesMeasuredDeviceSummary(t *testing.T) {
	source := &stubSoftwareInventorySource{
		devices: []SoftwareInventoryDeviceSummary{
			{Name: "工作站", Status: SoftwareInventoryStatus{Managed: true}, StatusLabels: []string{"正常"}},
			{Name: "不受管存储", StatusLabels: []string{"不采集（不受管设备）"}},
		},
	}
	handler, cookie := newSoftwareInventoryHandler(t, RoleStandalone, source)
	for _, path := range []string{"/ui/", "/ui/more/", "/ui/software-inventory/"} {
		response := doGetWithCookie(t, handler, cookie, path)
		body := readResponseBody(t, response)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("页面 %s 状态码 = %d，应为 200：\n%s", path, response.StatusCode, body)
		}
		for _, forbidden := range []string{
			"声明", "认领", "草稿", "待办", "对不上", "待确认暴露", "采集设置",
			`/ui/software-inventory/todos`, `/ui/software-inventory/settings`,
		} {
			if strings.Contains(body, forbidden) {
				t.Fatalf("页面 %s 不应出现 %q：\n%s", path, forbidden, body)
			}
		}
	}
	more := doGetWithCookie(t, handler, cookie, "/ui/more/")
	moreBody := readResponseBody(t, more)
	if !strings.Contains(moreBody, "2 台设备 · 1 台受管") {
		t.Fatalf("更多菜单应显示实测设备摘要：\n%s", moreBody)
	}
}

func newSoftwareInventoryHandler(
	t *testing.T,
	role Role,
	source SoftwareInventorySource,
) (http.Handler, *http.Cookie) {
	t.Helper()
	cfg := Config{
		Role:              role,
		Servers:           &stubServerSource{},
		Access:            &stubAccessStore{},
		SoftwareInventory: source,
		Topology: &stubTopologySource{data: &TopologyData{
			Sites: []TopologySite{
				{ID: "site-company", Name: "公司", LocationPath: "公司"},
				{ID: "site-hq", ParentID: "site-company", Name: "总部机房", LocationPath: "公司 / 总部机房"},
			},
			Networks: []TopologyNetwork{
				{ID: "network-hq", Name: "总部物理局域网", Type: "physical"},
				{ID: "network-virtual", Name: "跨地点虚拟局域网", Type: "virtual"},
			},
		}},
		VersionHint: func() string { return "" },
		Port:        18080,
	}
	if role == RoleClient {
		cfg.Access = nil
	}
	handler, startup, err := New(cfg)
	if err != nil {
		t.Fatalf("构造 Web UI Handler 失败：%v", err)
	}
	resp := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookies := resp.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("应签发会话 cookie，实际 %d", len(cookies))
	}
	return handler, cookies[0]
}

func TestSoftwareInventoryDevicePageShowsRequiredColumns(t *testing.T) {
	collectedAt := time.Date(2026, 11, 1, 10, 30, 0, 0, time.UTC)
	confirmedAt := time.Date(2026, 11, 2, 11, 0, 0, 0, time.UTC)
	source := &stubSoftwareInventorySource{device: &SoftwareInventoryDevice{
		Name: "工作站",
		Status: SoftwareInventoryStatus{
			Managed: true, CollectorLabel: "权威端", ServerEntry: "web-1",
			LastSuccessAt:     &collectedAt,
			LastFailureAt:     &confirmedAt,
			LastFailureReason: "连接超时",
			Stale:             true,
		},
		Groups: []SoftwareInventoryGroup{
			{CategoryLabel: "操作系统", Items: []SoftwareInventoryItem{{
				CategoryLabel: "操作系统", Name: "Ubuntu", Version: "22.04.5 LTS",
				Source: "实测", CollectedAt: &collectedAt,
				ListeningPorts: []SoftwareInventoryPort{{
					Address: "0.0.0.0", Port: 22, Protocol: "tcp", ExposureLabel: "监听所有地址",
				}},
			}}},
			{CategoryLabel: "容器镜像", Items: []SoftwareInventoryItem{{
				CategoryLabel: "容器镜像", Name: "postgres", Version: "16.4",
				Source: "实测", CollectedAt: &collectedAt, BelongsTo: "Docker",
				ContainerTag: "16-alpine", ContainerDigest: "sha256:abcdef", ContainerRunning: true,
			}}},
			{CategoryLabel: "系统包", SystemPackages: true, SystemPackageCount: 2, Items: []SoftwareInventoryItem{
				{CategoryLabel: "系统包", Name: "curl", Version: "8.9.1-1ubuntu2", Source: "实测", CollectedAt: &collectedAt},
				{CategoryLabel: "系统包", Name: "openssh-client", Version: "1:8.9p1-3ubuntu0.10", Source: "实测", CollectedAt: &collectedAt},
			}},
			{CategoryLabel: "服务程序", Items: []SoftwareInventoryItem{{
				CategoryLabel: "服务程序", Name: "自建网关", Version: "1.2.3+build.7",
				Source: "声明", ConfirmedAt: &confirmedAt,
			}}},
		},
		History: []SoftwareInventoryChange{{
			OccurredAt: collectedAt, KindLabel: "版本变化", CategoryLabel: "系统包",
			Name: "curl", OldValue: "8.8.0-1", NewValue: "8.9.1-1ubuntu2",
		}},
	}}
	handler, cookie := newSoftwareInventoryHandler(t, RoleStandalone, source)

	response := doGetWithCookie(t, handler, cookie, "/ui/software-inventory/devices?device="+url.QueryEscape("工作站"))
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("设备清单页状态码 = %d，应为 200：\n%s", response.StatusCode, body)
	}
	for _, wanted := range []string{
		"<h1>工作站 · 软件清单</h1>",
		"Ubuntu", "22.04.5 LTS", "实测", "2026-11-01 10:30:00",
		"0.0.0.0:22/tcp", "监听所有地址",
		"postgres", "16.4", "Docker", "16-alpine", "sha256:abcdef", "运行中",
		"权威端", "web-1", "连接超时", "清单已过期",
		"自建网关", "1.2.3", "build.7", "声明", "2026-11-02 11:00:00",
		"系统包（2 条，默认折叠）", "curl", "openssh-client",
		"版本变化", "8.8.0-1", "8.9.1-1ubuntu2",
		"立即采集",
	} {
		if !strings.Contains(body, wanted) {
			t.Fatalf("设备清单页缺少 %q：\n%s", wanted, body)
		}
	}
}

// TestSoftwareInventoryImmediateCollectionRequestedForClientCollector 覆盖
// 客户端实例作为采集执行者时的立即采集行为：只登记请求，页面明确显示等待。
func TestSoftwareInventoryImmediateCollectionRequestedForClientCollector(t *testing.T) {
	source := &stubSoftwareInventorySource{
		device: &SoftwareInventoryDevice{
			Name: "工作站",
			Status: SoftwareInventoryStatus{
				Managed: true, CollectorLabel: "笔记本客户端", PendingImmediate: false,
			},
		},
		collectResult: SoftwareInventoryCollectionResult{Requested: true},
	}
	handler, cookie := newSoftwareInventoryHandler(t, RoleStandalone, source)
	page := doGetWithCookie(t, handler, cookie, "/ui/software-inventory/devices?device="+url.QueryEscape("工作站"))
	csrf := hiddenInputMetaOrHiddenValue(t, readResponseBody(t, page))

	response := postSoftwareInventoryForm(t, handler, cookie,
		"/ui/software-inventory/collect?device="+url.QueryEscape("工作站"),
		url.Values{"csrf_token": {csrf}},
	)
	responseBody := readResponseBody(t, response)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("登记立即采集请求应返回 303，实际 %d：\n%s", response.StatusCode, responseBody)
	}
	if location := response.Header.Get("Location"); location != "/ui/software-inventory/devices?collect=requested&device="+url.QueryEscape("工作站") {
		t.Fatalf("登记立即采集请求后的跳转地址不符：%s", location)
	}
	if !source.collectCalled || source.collectDevice != "工作站" || source.collectAddress != "198.51.100.9" {
		t.Fatalf("立即采集请求应把设备与来源地址交给来源接口：%+v", source)
	}

	resultPage := doGetWithCookie(t, handler, cookie, response.Header.Get("Location"))
	resultBody := readResponseBody(t, resultPage)
	if !strings.Contains(resultBody, "已请求，等待客户端执行。") {
		t.Fatalf("客户端采集执行者登记请求后应显示等待提示：\n%s", resultBody)
	}
}

func postSoftwareInventoryForm(
	t *testing.T,
	handler http.Handler,
	cookie *http.Cookie,
	target string,
	form url.Values,
) *http.Response {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	request.Host = "127.0.0.1:18080"
	request.RemoteAddr = "198.51.100.9:45678"
	request.AddCookie(cookie)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "http://127.0.0.1:18080")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder.Result()
}

// TestSoftwareInventoryTemplatesUseMobileCards 固定手机可读性要求：各清
// 单页不使用裸表格，记录一律是带字段名的卡片。
func TestSoftwareInventoryTemplatesUseMobileCards(t *testing.T) {
	contents, err := templateContents()
	if err != nil {
		t.Fatalf("读取模板失败：%v", err)
	}
	for _, name := range []string{
		"softwareinventory.tmpl", "softwareinventory-device.tmpl",
		"softwareinventory-search.tmpl", "softwareinventory-declarations.tmpl",
		"softwareinventory-settings.tmpl", "softwareinventory-todos.tmpl",
	} {
		body, ok := contents[name]
		if !ok {
			t.Fatalf("模板 %s 不存在", name)
		}
		if strings.Contains(body, "<table") {
			t.Fatalf("软件清单模板 %s 不得使用表格：%s", name, body)
		}
		if !strings.Contains(body, "field-list") {
			t.Fatalf("软件清单模板 %s 应使用带字段名的卡片：%s", name, body)
		}
	}
}
