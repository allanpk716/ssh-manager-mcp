package webui

import (
	"bytes"
	"context"
	"encoding/csv"
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
	exportCSV          []byte
	exportErr          error
	exportFilter       SoftwareInventoryFilter
	exportCalled       bool
	searchRows         []SoftwareInventorySearchRow
	searchDevices      []SoftwareInventorySearchDevice
	searchQuery        string
	searchFilter       SoftwareInventoryFilter
	collectResult      SoftwareInventoryCollectionResult
	collectAllResult   SoftwareInventoryCollectionRoundResult
	collectAllErr      error
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

func (s *stubSoftwareInventorySource) ExportSoftwareInventoryCSV(
	filter SoftwareInventoryFilter,
) ([]byte, error) {
	s.exportCalled = true
	s.exportFilter = filter
	return s.exportCSV, s.exportErr
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
	return s.collectAllResult, s.collectAllErr
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
		`href="/ui/software-inventory/export?query=openssl&amp;exact_version=3.0.2-0ubuntu1.18&amp;site=%E5%85%AC%E5%8F%B8&amp;network=%E6%80%BB%E9%83%A8%E7%89%A9%E7%90%86%E5%B1%80%E5%9F%9F%E7%BD%91"`,
		"导出当前筛选",
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

func readSoftwareInventoryExportCSV(t *testing.T, response *http.Response) [][]string {
	t.Helper()
	body := readResponseBody(t, response)
	blob := []byte(body)
	if !bytes.HasPrefix(blob, []byte("\uFEFF")) {
		t.Fatalf("导出缺少 UTF-8 字节顺序标记：%q", blob)
	}
	reader := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(blob, []byte("\uFEFF"))))
	rows, err := reader.ReadAll()
	if err != nil {
		t.Fatalf("导出必须能用标准 CSV 解析器读回：%v\n%q", err, blob)
	}
	return rows
}

func TestSoftwareInventoryExportUsesCurrentFilters(t *testing.T) {
	source := &stubSoftwareInventorySource{exportCSV: []byte(
		"\uFEFF设备,软件名,版本,类别,监听端口,暴露级别,采集时间\r\n" +
			"工作站,nginx,1.25.3,服务程序,0.0.0.0:80/tcp,监听所有地址,2026-11-01T08:00:00Z\r\n")}
	handler, cookie := newSoftwareInventoryHandler(t, RoleStandalone, source)
	response := doGetWithCookie(t, handler, cookie,
		"/ui/software-inventory/export?query=nginx&exact_version=1.25.3&site=%E5%85%AC%E5%8F%B8&network=%E6%80%BB%E9%83%A8%E7%89%A9%E7%90%86%E5%B1%80%E5%9F%9F%E7%BD%91")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("导出状态码 = %d，应为 200：\n%s", response.StatusCode, readResponseBody(t, response))
	}
	if contentType := response.Header.Get("Content-Type"); contentType != "text/csv; charset=utf-8" {
		t.Fatalf("导出 Content-Type = %q", contentType)
	}
	if disposition := response.Header.Get("Content-Disposition"); disposition !=
		`attachment; filename="software-inventory.csv"` {
		t.Fatalf("导出 Content-Disposition = %q", disposition)
	}
	wantedFilter := SoftwareInventoryFilter{
		Name: "nginx", ExactVersion: "1.25.3", Site: "公司", Network: "总部物理局域网",
	}
	if !source.exportCalled || source.exportFilter != wantedFilter {
		t.Fatalf("导出条件 = %+v（called=%v），想要 %+v",
			source.exportFilter, source.exportCalled, wantedFilter)
	}
	rows := readSoftwareInventoryExportCSV(t, response)
	if len(rows) != 2 || rows[1][0] != "工作站" || rows[1][1] != "nginx" {
		t.Fatalf("当前筛选导出行 = %#v", rows)
	}
}

func TestSoftwareInventoryExportSingleDevice(t *testing.T) {
	source := &stubSoftwareInventorySource{
		device: &SoftwareInventoryDevice{Name: "工作站"},
		exportCSV: []byte(
			"\uFEFF设备,软件名,版本,类别,监听端口,暴露级别,采集时间\r\n" +
				"工作站,nginx,1.25.3,服务程序,0.0.0.0:80/tcp,监听所有地址,2026-11-01T08:00:00Z\r\n"),
	}
	handler, cookie := newSoftwareInventoryHandler(t, RoleStandalone, source)
	devicePage := doGetWithCookie(t, handler, cookie,
		"/ui/software-inventory/devices?device=%E5%B7%A5%E4%BD%9C%E7%AB%99")
	deviceBody := readResponseBody(t, devicePage)
	if !strings.Contains(deviceBody,
		`href="/ui/software-inventory/export?device=%E5%B7%A5%E4%BD%9C%E7%AB%99"`) ||
		!strings.Contains(deviceBody, "导出这台设备") {
		t.Fatalf("设备详情缺少单台设备导出入口：\n%s", deviceBody)
	}

	response := doGetWithCookie(t, handler, cookie,
		"/ui/software-inventory/export?device=%E5%B7%A5%E4%BD%9C%E7%AB%99")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("单台设备导出状态码 = %d，应为 200：\n%s",
			response.StatusCode, readResponseBody(t, response))
	}
	if disposition := response.Header.Get("Content-Disposition"); disposition !=
		`attachment; filename="software-inventory-device.csv"` {
		t.Fatalf("单台设备导出 Content-Disposition = %q", disposition)
	}
	wantedFilter := SoftwareInventoryFilter{Device: "工作站"}
	if source.exportFilter != wantedFilter {
		t.Fatalf("单台设备导出条件 = %+v，想要 %+v", source.exportFilter, wantedFilter)
	}
	rows := readSoftwareInventoryExportCSV(t, response)
	if len(rows) != 2 || rows[1][0] != "工作站" {
		t.Fatalf("单台设备导出行 = %#v", rows)
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

func TestSoftwareInventoryDevicePageShowsSummaryGroupsAndAdvanced(t *testing.T) {
	collectedAt := time.Date(2026, 11, 1, 10, 30, 0, 0, time.UTC)
	failedAt := time.Date(2026, 11, 2, 11, 0, 0, 0, time.UTC)
	source := &stubSoftwareInventorySource{device: &SoftwareInventoryDevice{
		Name: "工作站",
		Status: SoftwareInventoryStatus{
			Managed: true, Collector: "", CollectorLabel: "权威端",
			PreferredServerEntry: "web-1", ServerEntry: "web-1",
			LastSuccessAt:     &collectedAt,
			LastFailureAt:     &failedAt,
			LastFailureReason: "连接超时",
			Stale:             true,
		},
		Summary: SoftwareInventoryDeviceSummary{
			Name: "工作站", SystemName: "Ubuntu", SystemVersion: "24.04.1 LTS",
			KernelName: "Linux 内核", KernelVersion: "6.8.0-49-generic",
			ImportantSoftwareCount: 5, AllSoftwareCount: 8, UnidentifiedProcessCount: 1,
			StatusLabels: []string{"采集失败", "清单过期"},
		},
		Groups: []SoftwareInventoryGroup{
			{CategoryLabel: "操作系统与内核", Items: []SoftwareInventoryItem{
				{
					CategoryLabel: "操作系统", Name: "Ubuntu", Version: "24.04.1 LTS",
					Source: "实测", CollectedAt: &collectedAt,
				},
				{
					CategoryLabel: "驱动与固件", Name: "Linux 内核", Version: "6.8.0-49-generic",
					Source: "实测", CollectedAt: &collectedAt,
				},
			}},
			{CategoryLabel: "服务程序", Items: []SoftwareInventoryItem{{
				CategoryLabel: "服务程序", Name: "nginx", Version: "1.24.0",
				Source: "实测", CollectedAt: &collectedAt,
				ListeningPorts: []SoftwareInventoryPort{{
					Address: "0.0.0.0", Port: 80, Protocol: "tcp", ExposureLabel: "监听所有地址",
				}},
			}}},
			{CategoryLabel: "容器镜像", Items: []SoftwareInventoryItem{{
				CategoryLabel: "容器镜像", Name: "postgres", Version: "16.4",
				Source: "实测", CollectedAt: &collectedAt, BelongsTo: "Docker",
				ContainerTag: "16-alpine", ContainerDigest: "sha256:abcdef", ContainerRunning: true,
			}}},
			{CategoryLabel: "驱动与固件", Items: []SoftwareInventoryItem{{
				CategoryLabel: "驱动与固件", Name: "NVIDIA 显卡驱动", Version: "550.120",
				Source: "实测", CollectedAt: &collectedAt,
			}}},
			{
				CategoryLabel: "系统包", SystemPackages: true, SystemPackageCount: 2,
				Items: []SoftwareInventoryItem{
					{CategoryLabel: "系统包", Name: "curl", Version: "8.9.1-1ubuntu2", Source: "实测", CollectedAt: &collectedAt},
					{CategoryLabel: "系统包", Name: "openssh-client", Version: "1:8.9p1-3ubuntu0.10", Source: "实测", CollectedAt: &collectedAt},
				},
			},
			{CategoryLabel: "未识别进程", Items: []SoftwareInventoryItem{{
				CategoryLabel: "未识别进程", Name: "unknown-gateway",
				ProcessName: "gateway", ExecutablePath: "/opt/gateway/gateway", ProcessID: 8123,
				Source: "实测", CollectedAt: &collectedAt,
				ListeningPorts: []SoftwareInventoryPort{{
					Address: "127.0.0.1", Port: 8443, Protocol: "tcp", ExposureLabel: "仅本机",
				}},
			}}},
		},
		Advanced: SoftwareInventoryDeviceAdvanced{
			ClientInstances: []SoftwareInventoryClientInstance{
				{Name: "笔记本客户端", ProfileName: "运维"},
			},
			ServerEntries: []SoftwareInventoryDeviceServerEntry{
				{Name: "web-1", SudoEnabled: false},
				{Name: "db-1", SudoEnabled: true},
			},
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
		"系统与内核", "Ubuntu 24.04.1 LTS；Linux 内核 6.8.0-49-generic",
		"重点软件", "全部软件", "未识别进程",
		"采集时间", "2026-11-01 10:30:00", "采集失败", "清单过期", "连接超时",
		"0.0.0.0:80/tcp", "监听所有地址",
		"postgres", "16.4", "Docker", "16-alpine", "sha256:abcdef", "运行中",
		"NVIDIA 显卡驱动", "550.120",
		"系统包（2 条，默认折叠）", "curl", "openssh-client",
		"未识别", "gateway", "/opt/gateway/gateway", "8123", "127.0.0.1:8443/tcp", "仅本机",
		"版本变化", "8.8.0-1", "8.9.1-1ubuntu2",
		"立即采集",
		`id="software-inventory-advanced"`, "高级采集设置", "权威端", "笔记本客户端",
		"web-1", "db-1", "允许用 sudo 采集", "最近 5 分钟内重新验证管理员口令",
	} {
		if !strings.Contains(body, wanted) {
			t.Fatalf("设备清单页缺少 %q：\n%s", wanted, body)
		}
	}
	previous := -1
	for _, label := range []string{
		"操作系统与内核", "服务程序", "容器镜像", "驱动与固件", "系统包", "未识别进程",
	} {
		position := strings.Index(body, "<h2>"+label)
		if position < 0 || position < previous {
			t.Fatalf("设备详情分组 %s 应按固定顺序出现：\n%s", label, body)
		}
		previous = position
	}
	if !strings.Contains(body, `<details id="software-inventory-advanced">`) {
		t.Fatalf("高级采集设置应默认折叠（不带 open 属性）：\n%s", body)
	}
	if strings.Contains(body, "<details open") {
		t.Fatalf("默认视图不应有展开的折叠区：\n%s", body)
	}
	for _, forbidden := range []string{"声明", "认领", "草稿", "待办", "对不上", "待确认暴露"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("设备清单页不应出现 %q：\n%s", forbidden, body)
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

// TestSoftwareInventoryDeviceCollectorSettings 覆盖设备页高级区的采集
// 执行者表单：校验错误回显并展开高级区，成功后回到设备页显示提示。
func TestSoftwareInventoryDeviceCollectorSettings(t *testing.T) {
	source := &stubSoftwareInventorySource{device: &SoftwareInventoryDevice{
		Name: "工作站",
		Status: SoftwareInventoryStatus{
			Managed: true, CollectorLabel: "权威端", PreferredServerEntry: "web-1",
		},
		Advanced: SoftwareInventoryDeviceAdvanced{
			ClientInstances: []SoftwareInventoryClientInstance{{Name: "laptop-client", ProfileName: "运维"}},
			ServerEntries:   []SoftwareInventoryDeviceServerEntry{{Name: "web-1"}},
		},
	}, collectorErr: &SoftwareInventoryFormError{
		Message: "客户端实例 \"missing\" 不是有效的活动设备码",
	}}
	handler, cookie := newSoftwareInventoryHandler(t, RoleStandalone, source)
	page := doGetWithCookie(t, handler, cookie, "/ui/software-inventory/devices?device="+url.QueryEscape("工作站"))
	pageBody := readResponseBody(t, page)
	csrf := hiddenInputMetaOrHiddenValue(t, pageBody)

	rejected := postSoftwareInventoryForm(t, handler, cookie,
		"/ui/software-inventory/devices/collector", url.Values{
			"csrf_token": {csrf}, "device": {"工作站"}, "collector": {"missing"},
			"server_entry": {"web-1"},
		})
	rejectedBody := readResponseBody(t, rejected)
	if rejected.StatusCode != http.StatusBadRequest {
		t.Fatalf("采集执行者校验失败应返回 400，实际 %d：\n%s", rejected.StatusCode, rejectedBody)
	}
	for _, wanted := range []string{
		`<details id="software-inventory-advanced" open>`,
		`value="missing" selected`, `value="web-1" selected`,
		"不是有效的活动设备码",
	} {
		if !strings.Contains(rejectedBody, wanted) {
			t.Fatalf("采集执行者错误页缺少 %q：\n%s", wanted, rejectedBody)
		}
	}
	if !source.collectorCall || source.collectorAddr != "198.51.100.9" {
		t.Fatalf("采集执行者登记应交给来源接口：%+v", source)
	}

	source.collectorErr = nil
	applied := postSoftwareInventoryForm(t, handler, cookie,
		"/ui/software-inventory/devices/collector", url.Values{
			"csrf_token": {csrf}, "device": {"工作站"}, "collector": {""},
			"server_entry": {"web-1"},
		})
	if applied.StatusCode != http.StatusSeeOther || applied.Header.Get("Location") !=
		"/ui/software-inventory/devices?device="+url.QueryEscape("工作站")+"&notice=collector-set" {
		t.Fatalf("采集执行者登记成功响应不符：%d %s", applied.StatusCode, applied.Header.Get("Location"))
	}
	resultPage := doGetWithCookie(t, handler, cookie, applied.Header.Get("Location"))
	resultBody := readResponseBody(t, resultPage)
	if !strings.Contains(resultBody, "采集执行者已登记。") {
		t.Fatalf("设备页应显示采集执行者登记提示：\n%s", resultBody)
	}
	if source.collectorForm.Device != "工作站" || source.collectorForm.Collector != "" ||
		source.collectorForm.ServerEntry != "web-1" || !source.collectorForm.Authority {
		t.Fatalf("采集执行者登记表单应传回权威端与服务器条目：%+v", source.collectorForm)
	}
}

// TestSoftwareInventoryDeviceSudoSettings 覆盖设备页 sudo 采集开关：校验
// 错误回显，成功后给出打开与关闭提示。
func TestSoftwareInventoryDeviceSudoSettings(t *testing.T) {
	source := &stubSoftwareInventorySource{device: &SoftwareInventoryDevice{
		Name:   "工作站",
		Status: SoftwareInventoryStatus{Managed: true, CollectorLabel: "权威端"},
		Advanced: SoftwareInventoryDeviceAdvanced{
			ServerEntries: []SoftwareInventoryDeviceServerEntry{{Name: "web-1"}},
		},
	}, sudoErr: &SoftwareInventoryFormError{Message: "服务器条目 \"missing\" 不存在"}}
	handler, cookie := newSoftwareInventoryHandler(t, RoleStandalone, source)
	page := doGetWithCookie(t, handler, cookie, "/ui/software-inventory/devices?device="+url.QueryEscape("工作站"))
	csrf := hiddenInputMetaOrHiddenValue(t, readResponseBody(t, page))

	rejected := postSoftwareInventoryForm(t, handler, cookie,
		"/ui/software-inventory/devices/sudo", url.Values{
			"csrf_token": {csrf}, "device": {"工作站"}, "server_entry": {"missing"},
		})
	rejectedBody := readResponseBody(t, rejected)
	if rejected.StatusCode != http.StatusBadRequest {
		t.Fatalf("sudo 设置校验失败应返回 400，实际 %d：\n%s", rejected.StatusCode, rejectedBody)
	}
	if !strings.Contains(rejectedBody, `id="software-inventory-advanced" open`) ||
		!strings.Contains(rejectedBody, "服务器条目 &#34;missing&#34; 不存在") {
		t.Fatalf("sudo 错误页应展开高级区并显示原因：\n%s", rejectedBody)
	}
	if !source.sudoCalled || source.sudoServer != "missing" {
		t.Fatalf("sudo 设置应交给来源接口：%+v", source)
	}

	source.sudoErr = nil
	enabled := postSoftwareInventoryForm(t, handler, cookie,
		"/ui/software-inventory/devices/sudo", url.Values{
			"csrf_token": {csrf}, "device": {"工作站"}, "server_entry": {"web-1"},
			"sudo_enabled": {"1"},
		})
	if enabled.StatusCode != http.StatusSeeOther || enabled.Header.Get("Location") !=
		"/ui/software-inventory/devices?device="+url.QueryEscape("工作站")+"&notice=sudo-enabled" {
		t.Fatalf("sudo 打开成功响应不符：%d %s", enabled.StatusCode, enabled.Header.Get("Location"))
	}
	if !source.sudoEnabled || source.sudoAddress != "198.51.100.9" {
		t.Fatalf("sudo 打开应交给来源接口：%+v", source)
	}
	disabled := postSoftwareInventoryForm(t, handler, cookie,
		"/ui/software-inventory/devices/sudo", url.Values{
			"csrf_token": {csrf}, "device": {"工作站"}, "server_entry": {"web-1"},
		})
	if disabled.StatusCode != http.StatusSeeOther || disabled.Header.Get("Location") !=
		"/ui/software-inventory/devices?device="+url.QueryEscape("工作站")+"&notice=sudo-disabled" {
		t.Fatalf("sudo 关闭成功响应不符：%d %s", disabled.StatusCode, disabled.Header.Get("Location"))
	}
}

// TestSoftwareInventoryDeviceSudoEnableRequiresReverify 验证设备页打开
// sudo 采集开关沿用重新验证要求，回跳后保留服务器条目与开关状态。
func TestSoftwareInventoryDeviceSudoEnableRequiresReverify(t *testing.T) {
	auth := &fakeAuthorityAuth{passwordSet: true, password: "correct-password"}
	now := time.Unix(1800000000, 0)
	source := &stubSoftwareInventorySource{device: &SoftwareInventoryDevice{
		Name:   "工作站",
		Status: SoftwareInventoryStatus{Managed: true, CollectorLabel: "权威端"},
		Advanced: SoftwareInventoryDeviceAdvanced{
			ServerEntries: []SoftwareInventoryDeviceServerEntry{{Name: "web-1"}},
		},
	}}
	handler, _, err := NewAuthority(Config{
		Role: RoleServer, Servers: &stubServerSource{}, Access: &stubAccessStore{},
		Auth: auth, AllowedHosts: []string{"127.0.0.1"}, Port: 7879,
		SoftwareInventory: source, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie := authoritySuccessfulLogin(t, handler, "127.0.0.1:7879", "correct-password")
	now = now.Add(6 * time.Minute)
	devicePath := "/ui/software-inventory/devices?device=" + url.QueryEscape("工作站")
	page := authorityGet(t, handler, "127.0.0.1:7879", devicePath, cookie)
	pageBody := readResponseBody(t, page)
	if page.StatusCode != http.StatusOK {
		t.Fatalf("设备页状态码 = %d，应为 200：\n%s", page.StatusCode, pageBody)
	}
	csrf := hiddenInputMetaOrHiddenValue(t, pageBody)

	rejected := authorityPostForm(t, handler, "127.0.0.1:7879",
		"/ui/software-inventory/devices/sudo", url.Values{
			"csrf_token": {csrf}, "device": {"工作站"}, "server_entry": {"web-1"},
			"sudo_enabled": {"1"},
		}, cookie)
	resume := devicePath + "&resume=1&server_entry=web-1&sudo_enabled=1"
	wantNext := `/ui/reverify?next=` + url.QueryEscape(resume)
	if rejected.StatusCode != http.StatusSeeOther || rejected.Header.Get("Location") != wantNext {
		t.Fatalf("未重新验证时打开 sudo 开关应跳转重新验证：%d %s", rejected.StatusCode, rejected.Header.Get("Location"))
	}
	reverifyPage := authorityGet(t, handler, "127.0.0.1:7879", wantNext, cookie)
	reverifyCSRF := hiddenInputValue(t, readResponseBody(t, reverifyPage), "csrf_token")
	verified := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/reverify", url.Values{
		"csrf_token": {reverifyCSRF}, "password": {"correct-password"}, "next": {resume},
	}, cookie)
	if verified.StatusCode != http.StatusSeeOther {
		t.Fatalf("重新验证失败：%d", verified.StatusCode)
	}
	resumed := authorityGet(t, handler, "127.0.0.1:7879", verified.Header.Get("Location"), cookie)
	resumedBody := readResponseBody(t, resumed)
	if !strings.Contains(resumedBody, `value="web-1" selected`) ||
		!strings.Contains(resumedBody, `value="1" checked`) ||
		!strings.Contains(resumedBody, "已保留表单状态，请再次提交") {
		t.Fatalf("重新验证回跳后应保留 sudo 表单状态：\n%s", resumedBody)
	}
	resumedCSRF := hiddenInputMetaOrHiddenValue(t, resumedBody)
	applied := authorityPostForm(t, handler, "127.0.0.1:7879",
		"/ui/software-inventory/devices/sudo", url.Values{
			"csrf_token": {resumedCSRF}, "device": {"工作站"}, "server_entry": {"web-1"},
			"sudo_enabled": {"1"},
		}, cookie)
	if applied.StatusCode != http.StatusSeeOther || applied.Header.Get("Location") !=
		devicePath+"&notice=sudo-enabled" {
		t.Fatalf("重新验证后的 sudo 提交应成功：%d %s", applied.StatusCode, applied.Header.Get("Location"))
	}
	if !source.sudoCalled || source.sudoServer != "web-1" || !source.sudoEnabled {
		t.Fatalf("sudo 开关应交给来源接口：%+v", source)
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
		"softwareinventory-todos.tmpl",
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
