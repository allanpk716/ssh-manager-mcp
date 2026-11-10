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

	if body := pageBody(RoleStandalone, true); !strings.Contains(body, `href="/ui/software-inventory/"`) ||
		!strings.Contains(body, `aria-label="软件清单区域"`) {
		t.Fatalf("单机配置软件清单来源后应显示导航入口：\n%s", body)
	}
	if body := pageBody(RoleStandalone, false); strings.Contains(body, `href="/ui/software-inventory/"`) {
		t.Fatalf("未配置软件清单来源时不应显示导航入口：\n%s", body)
	}
	if body := pageBody(RoleClient, true); strings.Contains(body, `href="/ui/software-inventory/"`) {
		t.Fatalf("客户端 Web UI 不应显示软件清单导航入口：\n%s", body)
	}
}

type stubSoftwareInventorySource struct {
	device         *SoftwareInventoryDevice
	deviceErr      error
	searchRows     []SoftwareInventorySearchRow
	searchQuery    string
	collectResult  SoftwareInventoryCollectionResult
	collectErr     error
	collectDevice  string
	collectAddress string
	collectCalled  bool
	todos          SoftwareInventoryTodos
	declaration    *SoftwareInventoryDeclaration
	addedForm      SoftwareInventoryDeclarationForm
	addAddress     string
	addCalled      bool
	addID          string
	editCalled     bool
	editID         string
	editForm       SoftwareInventoryDeclarationForm
	editAddress    string
	claimCalled    bool
	claimID        string
	claimKey       string
	claimAddress   string
	deleteCalled   bool
	deleteID       string
	deleteAddress  string
	drafts         []SoftwareInventoryDraft
	confirmID      string
	confirmForm    *SoftwareInventoryDeclarationForm
	confirmAddress string
	confirmCalled  bool
	rejectID       string
	rejectAddress  string
	rejectCalled   bool
	settings       SoftwareInventorySettings
	declarations   []SoftwareInventoryDeclaration
	collectorForm  SoftwareInventoryCollectorForm
	collectorErr   error
	collectorAddr  string
	collectorCall  bool
	sudoServer     string
	sudoEnabled    bool
	sudoErr        error
	sudoAddress    string
	sudoCalled     bool
}

func (s *stubSoftwareInventorySource) SoftwareInventoryDevices() ([]SoftwareInventoryDeviceSummary, error) {
	return nil, nil
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

func (s *stubSoftwareInventorySource) SearchSoftwareInventory(namePart string) ([]SoftwareInventorySearchRow, error) {
	s.searchQuery = namePart
	return s.searchRows, nil
}

func (s *stubSoftwareInventorySource) RequestSoftwareInventoryCollection(
	_ context.Context, deviceName string, source SoftwareInventoryWriteSource,
) (SoftwareInventoryCollectionResult, error) {
	s.collectCalled = true
	s.collectDevice = deviceName
	s.collectAddress = source.RemoteAddress
	return s.collectResult, s.collectErr
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

// TestSoftwareInventorySearchListsDevices 覆盖跨设备搜索：按软件名返回每台
// 设备上的精确版本、来源与暴露级别。
func TestSoftwareInventorySearchListsDevices(t *testing.T) {
	collectedAt := time.Date(2026, 11, 1, 10, 30, 0, 0, time.UTC)
	source := &stubSoftwareInventorySource{searchRows: []SoftwareInventorySearchRow{
		{DeviceName: "工作站", Item: SoftwareInventoryItem{
			Name: "OpenSSH", Version: "1:8.9p1-3ubuntu0.10", Source: "实测", CollectedAt: &collectedAt,
			ListeningPorts: []SoftwareInventoryPort{{Address: "0.0.0.0", Port: 22, Protocol: "tcp", ExposureLabel: "监听所有地址"}},
		}},
		{DeviceName: "网络存储", Item: SoftwareInventoryItem{
			Name: "OpenSSH", Version: "9.6p1-1", Source: "声明",
			ListeningPorts: []SoftwareInventoryPort{{Address: "192.0.2.20", Port: 22, Protocol: "tcp", ExposureLabel: "局域网"}},
		}},
	}}
	handler, cookie := newSoftwareInventoryHandler(t, RoleStandalone, source)

	response := doGetWithCookie(t, handler, cookie, "/ui/software-inventory/search?query=opens")
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("跨设备搜索页状态码 = %d，应为 200：\n%s", response.StatusCode, body)
	}
	for _, wanted := range []string{
		"<h1>软件清单搜索</h1>", `name="query"`, "value=\"opens\"",
		"工作站", "网络存储", "OpenSSH", "1:8.9p1-3ubuntu0.10", "9.6p1-1",
		"实测", "声明", "0.0.0.0:22/tcp", "监听所有地址", "192.0.2.20:22/tcp", "局域网",
	} {
		if !strings.Contains(body, wanted) {
			t.Fatalf("跨设备搜索页缺少 %q：\n%s", wanted, body)
		}
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
		VersionHint:       func() string { return "" },
		Port:              18080,
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

// TestSoftwareInventoryTodoPageListsThreeGroups 验证待办页展示领域模块计
// 算出的三类待办，并给可处理的条目提供进入表单的入口。
func TestSoftwareInventoryTodoPageListsThreeGroups(t *testing.T) {
	lastSuccess := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	source := &stubSoftwareInventorySource{todos: SoftwareInventoryTodos{
		PendingExposure: []SoftwareInventoryPendingExposureDevice{{
			DeviceName: "工作站",
			Ports: []SoftwareInventoryPendingPort{{
				Address: "0.0.0.0", Port: 443, Protocol: "tcp",
				ExposureLabel:     "监听所有地址",
				ObservedStableKey: "service_program\x1fnginx",
			}},
		}},
		UnidentifiedProcesses: []SoftwareInventoryUnidentifiedProcess{{
			DeviceName: "工作站", Name: "8080 监听进程", ProcessName: "gateway",
			ExecutablePath: "/opt/gateway/gateway", ObservedStableKey: "unidentified\x1f8080",
			ListeningPorts: []SoftwareInventoryPort{{
				Address: "0.0.0.0", Port: 8080, Protocol: "tcp", ExposureLabel: "监听所有地址",
			}},
		}},
		Mismatches: []SoftwareInventoryMismatch{{
			DeviceName: "工作站", DeclarationID: "declaration-1",
			Name: "旧网站", Reason: "认领的实测条目已消失",
		}},
		StaleDevices: []SoftwareInventoryStaleDevice{{
			DeviceName: "旧工作站", LastSuccessAt: &lastSuccess, NeverCollected: true,
		}},
	}}
	handler, cookie := newSoftwareInventoryHandler(t, RoleStandalone, source)

	response := doGetWithCookie(t, handler, cookie, "/ui/software-inventory/todos")
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("待办页状态码 = %d，应为 200：\n%s", response.StatusCode, body)
	}
	for _, wanted := range []string{
		"<h1>软件清单待办</h1>", "<h2>待确认暴露</h2>", "工作站", "0.0.0.0:443/tcp",
		"监听所有地址", "进入声明表单", "<h2>未识别进程</h2>", "gateway",
		"/opt/gateway/gateway", "0.0.0.0:8080/tcp", "<h2>对不上与清单过期</h2>",
		"旧网站", "认领的实测条目已消失", "清单过期", "旧工作站", "从未成功采集",
	} {
		if !strings.Contains(body, wanted) {
			t.Fatalf("待办页缺少 %q：\n%s", wanted, body)
		}
	}
	if !strings.Contains(body, `/ui/software-inventory/declarations/new?device=%E5%B7%A5%E4%BD%9C%E7%AB%99&amp;observed_key=unidentified%1F8080&amp;port_address=0.0.0.0&amp;port_exposure=all_addresses&amp;port_number=8080&amp;port_protocol=tcp`) {
		t.Fatalf("未识别进程应跳到带设备、端口与实测稳定键的声明表单：\n%s", body)
	}
}

// TestSoftwareInventoryDeclarationNewClaimsObservedProcess 覆盖从待办进入
// 新增声明表单：设备与端口预填，提交后携带来源地址调用领域写入接口。
func TestSoftwareInventoryDeclarationNewClaimsObservedProcess(t *testing.T) {
	source := &stubSoftwareInventorySource{addID: "declaration-new"}
	handler, cookie := newSoftwareInventoryHandler(t, RoleStandalone, source)
	target := "/ui/software-inventory/declarations/new?device=" + url.QueryEscape("工作站") +
		"&observed_key=unidentified-8443&port_address=0.0.0.0&port_number=8443&port_protocol=tcp&port_exposure=all_addresses"

	page := doGetWithCookie(t, handler, cookie, target)
	pageBody := readResponseBody(t, page)
	if page.StatusCode != http.StatusOK {
		t.Fatalf("新增声明表单状态码 = %d，应为 200：\n%s", page.StatusCode, pageBody)
	}
	for _, wanted := range []string{
		"<h1>新增软件声明</h1>", `value="工作站"`, `value="unidentified-8443"`,
		`0.0.0.0,8443,tcp,all_addresses`, "认领这条实测条目",
	} {
		if !strings.Contains(pageBody, wanted) {
			t.Fatalf("新增声明表单缺少 %q：\n%s", wanted, pageBody)
		}
	}
	csrf := hiddenInputMetaOrHiddenValue(t, pageBody)
	response := postSoftwareInventoryForm(t, handler, cookie, "/ui/software-inventory/declarations/new", url.Values{
		"csrf_token": {csrf}, "device": {"工作站"}, "category": {"service_program"},
		"name": {"业务网关"}, "version": {"2.1.0"},
		"ports":        {"0.0.0.0,8443,tcp,virtual_local_area_network"},
		"observed_key": {"unidentified-8443"},
	})
	responseBody := readResponseBody(t, response)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("新增声明状态码 = %d，应为 303：\n%s", response.StatusCode, responseBody)
	}
	if location := response.Header.Get("Location"); location != "/ui/software-inventory/todos?notice=declaration-added" {
		t.Fatalf("新增声明后的跳转地址不符：%s", location)
	}
	if !source.addCalled || source.addAddress != "198.51.100.9" ||
		source.addedForm.Device != "工作站" || source.addedForm.Category != "service_program" ||
		source.addedForm.Name != "业务网关" || source.addedForm.Version != "2.1.0" ||
		source.addedForm.ObservedStableKey != "unidentified-8443" ||
		len(source.addedForm.ListeningPorts) != 1 ||
		source.addedForm.ListeningPorts[0].Exposure != "virtual_local_area_network" {
		t.Fatalf("新增声明应把表单与来源地址交给来源接口：%+v", source.addedForm)
	}
}

// TestSoftwareInventoryDeclarationListAndPageLinks 验证生效声明有列表入口，
// 软件清单首页能进入待办、草稿审阅、声明与采集设置。
func TestSoftwareInventoryDeclarationListAndPageLinks(t *testing.T) {
	source := &stubSoftwareInventorySource{declarations: []SoftwareInventoryDeclaration{{
		ID: "declaration-1", Device: "工作站", CategoryLabel: "服务程序",
		Name: "业务网关", Version: "2.1.0",
		ListeningPorts: []SoftwareInventoryFormPort{{
			Address: "0.0.0.0", Port: 8443, Protocol: "tcp", Exposure: "public_network",
		}},
	}}}
	handler, cookie := newSoftwareInventoryHandler(t, RoleStandalone, source)
	root := doGetWithCookie(t, handler, cookie, "/ui/software-inventory/")
	rootBody := readResponseBody(t, root)
	for _, link := range []string{
		`href="/ui/software-inventory/todos"`, `href="/ui/software-inventory/drafts"`,
		`href="/ui/software-inventory/declarations"`, `href="/ui/software-inventory/settings"`,
		`href="/ui/software-inventory/declarations/new"`,
	} {
		if !strings.Contains(rootBody, link) {
			t.Fatalf("软件清单首页缺少入口 %s：\n%s", link, rootBody)
		}
	}

	response := doGetWithCookie(t, handler, cookie, "/ui/software-inventory/declarations")
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "<h1>生效软件声明</h1>") ||
		!strings.Contains(body, "工作站") || !strings.Contains(body, "业务网关") ||
		!strings.Contains(body, "2.1.0") || !strings.Contains(body, "8443/tcp") ||
		!strings.Contains(body, `href="/ui/software-inventory/declarations/edit?id=declaration-1"`) ||
		!strings.Contains(body, `href="/ui/software-inventory/declarations/delete?id=declaration-1"`) {
		t.Fatalf("生效声明列表页不符：\n%s", body)
	}
}

// TestSoftwareInventoryDeclarationEditAndClaim 覆盖修改声明与把修改后的声
// 明认领到实测条目这两个写入口。
func TestSoftwareInventoryDeclarationEditAndClaim(t *testing.T) {
	source := &stubSoftwareInventorySource{declaration: &SoftwareInventoryDeclaration{
		ID: "declaration-1", Device: "工作站", Category: "service_program",
		CategoryLabel: "服务程序", Name: "业务网关", Version: "2.1.0",
		ListeningPorts: []SoftwareInventoryFormPort{{
			Address: "0.0.0.0", Port: 8443, Protocol: "tcp", Exposure: "local_area_network",
		}},
		ClaimedObservedKey: "observed-1",
	}}
	handler, cookie := newSoftwareInventoryHandler(t, RoleStandalone, source)
	page := doGetWithCookie(t, handler, cookie, "/ui/software-inventory/declarations/edit?id=declaration-1")
	pageBody := readResponseBody(t, page)
	if page.StatusCode != http.StatusOK {
		t.Fatalf("修改声明表单状态码 = %d，应为 200：\n%s", page.StatusCode, pageBody)
	}
	for _, wanted := range []string{
		"<h1>修改软件声明</h1>", `value="declaration-1"`, `value="业务网关"`,
		`0.0.0.0,8443,tcp,local_area_network`, `value="observed-1"`, "保存修改", "保存并认领",
	} {
		if !strings.Contains(pageBody, wanted) {
			t.Fatalf("修改声明表单缺少 %q：\n%s", wanted, pageBody)
		}
	}
	csrf := hiddenInputMetaOrHiddenValue(t, pageBody)
	response := postSoftwareInventoryForm(t, handler, cookie, "/ui/software-inventory/declarations/edit", url.Values{
		"csrf_token": {csrf}, "id": {"declaration-1"}, "device": {"工作站"},
		"category": {"service_program"}, "name": {"业务网关"}, "version": {"2.2.0"},
		"ports":        {"0.0.0.0,8443,tcp,public_network"},
		"observed_key": {"observed-2"}, "action": {"claim"},
	})
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/ui/software-inventory/todos?notice=declaration-claimed" {
		t.Fatalf("修改并认领声明响应不符：%d %s", response.StatusCode, response.Header.Get("Location"))
	}
	if !source.editCalled || source.editID != "declaration-1" || source.editAddress != "198.51.100.9" ||
		source.editForm.Version != "2.2.0" || len(source.editForm.ListeningPorts) != 1 ||
		source.editForm.ListeningPorts[0].Exposure != "public_network" {
		t.Fatalf("修改声明应交给来源接口：%+v", source.editForm)
	}
	if !source.claimCalled || source.claimID != "declaration-1" ||
		source.claimKey != "observed-2" || source.claimAddress != "198.51.100.9" {
		t.Fatalf("认领声明应交给来源接口：%+v", source)
	}
}

// TestSoftwareInventoryDeclarationDeleteConfirmation 覆盖删除声明的确认页
// 与确认提交。
func TestSoftwareInventoryDeclarationDeleteConfirmation(t *testing.T) {
	source := &stubSoftwareInventorySource{declaration: &SoftwareInventoryDeclaration{
		ID: "declaration-1", Device: "工作站", CategoryLabel: "服务程序",
		Name: "业务网关", Version: "2.1.0",
	}}
	handler, cookie := newSoftwareInventoryHandler(t, RoleStandalone, source)
	page := doGetWithCookie(t, handler, cookie, "/ui/software-inventory/declarations/delete?id=declaration-1")
	pageBody := readResponseBody(t, page)
	if page.StatusCode != http.StatusOK || !strings.Contains(pageBody, "<h1>删除软件声明</h1>") ||
		!strings.Contains(pageBody, "工作站") || !strings.Contains(pageBody, "业务网关") ||
		!strings.Contains(pageBody, "确认删除") {
		t.Fatalf("删除声明确认页不符：\n%s", pageBody)
	}
	csrf := hiddenInputMetaOrHiddenValue(t, pageBody)
	response := postSoftwareInventoryForm(t, handler, cookie, "/ui/software-inventory/declarations/delete", url.Values{
		"csrf_token": {csrf}, "id": {"declaration-1"},
	})
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/ui/software-inventory/declarations?notice=declaration-deleted" {
		t.Fatalf("删除声明响应不符：%d %s", response.StatusCode, response.Header.Get("Location"))
	}
	if !source.deleteCalled || source.deleteID != "declaration-1" || source.deleteAddress != "198.51.100.9" {
		t.Fatalf("删除声明应交给来源接口：%+v", source)
	}
}

// TestSoftwareInventoryDraftReviewConfirmsWithoutEdit 覆盖草稿审阅页：先按
// 原样确认，页面跳回草稿列表。
func TestSoftwareInventoryDraftReviewConfirmsWithoutEdit(t *testing.T) {
	source := &stubSoftwareInventorySource{drafts: []SoftwareInventoryDraft{{
		ID: "draft-1", Device: "工作站", Category: "service_program",
		CategoryLabel: "服务程序", Name: "业务网关", Version: "2.1.0",
		ClaimedObservedKey: "unidentified-8443",
		ListeningPorts: []SoftwareInventoryPort{{
			Address: "0.0.0.0", Port: 8443, Protocol: "tcp", ExposureLabel: "虚拟局域网",
		}},
	}}}
	handler, cookie := newSoftwareInventoryHandler(t, RoleStandalone, source)
	page := doGetWithCookie(t, handler, cookie, "/ui/software-inventory/drafts")
	pageBody := readResponseBody(t, page)
	if page.StatusCode != http.StatusOK {
		t.Fatalf("草稿审阅页状态码 = %d，应为 200：\n%s", page.StatusCode, pageBody)
	}
	for _, wanted := range []string{
		"<h1>声明草稿审阅</h1>", "工作站", "业务网关", "2.1.0", "服务程序",
		"0.0.0.0:8443/tcp · 虚拟局域网", "unidentified-8443",
		"按草稿确认", "修改后确认", "驳回草稿",
	} {
		if !strings.Contains(pageBody, wanted) {
			t.Fatalf("草稿审阅页缺少 %q：\n%s", wanted, pageBody)
		}
	}
	csrf := hiddenInputMetaOrHiddenValue(t, pageBody)
	response := postSoftwareInventoryForm(t, handler, cookie, "/ui/software-inventory/drafts/confirm", url.Values{
		"csrf_token": {csrf}, "draft_id": {"draft-1"}, "mode": {"original"},
	})
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/ui/software-inventory/drafts?notice=draft-confirmed" {
		t.Fatalf("按草稿确认响应不符：%d %s", response.StatusCode, response.Header.Get("Location"))
	}
	if !source.confirmCalled || source.confirmID != "draft-1" || source.confirmForm != nil ||
		source.confirmAddress != "198.51.100.9" {
		t.Fatalf("按草稿确认应把空修改表单与来源地址交给来源接口：%+v", source)
	}
}

// TestSoftwareInventoryDraftModifiedConfirmAndReject 分别覆盖修改后确认与
// 驳回两个写入口。
func TestSoftwareInventoryDraftModifiedConfirmAndReject(t *testing.T) {
	source := &stubSoftwareInventorySource{drafts: []SoftwareInventoryDraft{{
		ID: "draft-1", Device: "工作站", Category: "service_program",
		Name: "业务网关", Version: "2.1.0", ListeningPortsText: "0.0.0.0,8443,tcp,virtual_local_area_network",
	}}}
	handler, cookie := newSoftwareInventoryHandler(t, RoleStandalone, source)
	page := doGetWithCookie(t, handler, cookie, "/ui/software-inventory/drafts")
	csrf := hiddenInputMetaOrHiddenValue(t, readResponseBody(t, page))

	confirm := postSoftwareInventoryForm(t, handler, cookie, "/ui/software-inventory/drafts/confirm", url.Values{
		"csrf_token": {csrf}, "draft_id": {"draft-1"}, "mode": {"edited"},
		"device": {"工作站"}, "category": {"service_program"},
		"name": {"业务网关"}, "version": {"2.2.0"},
		"ports": {"0.0.0.0,8443,tcp,public_network"},
		"purl":  {"pkg:generic/gateway@2.2.0"}, "cpe": {"cpe:2.3:a:example:gateway:*:*:*:*:*:*:*:*"},
		"belongs_to": {"业务系统"}, "note": {"修改后确认"},
	})
	if confirm.StatusCode != http.StatusSeeOther || confirm.Header.Get("Location") != "/ui/software-inventory/drafts?notice=draft-confirmed" {
		t.Fatalf("修改后确认响应不符：%d %s", confirm.StatusCode, confirm.Header.Get("Location"))
	}
	if source.confirmForm == nil || source.confirmForm.Version != "2.2.0" ||
		source.confirmForm.PackageURL != "pkg:generic/gateway@2.2.0" ||
		len(source.confirmForm.ListeningPorts) != 1 ||
		source.confirmForm.ListeningPorts[0].Exposure != "public_network" ||
		source.confirmAddress != "198.51.100.9" {
		t.Fatalf("修改后确认应传递修改后的表单：%+v", source.confirmForm)
	}

	reject := postSoftwareInventoryForm(t, handler, cookie, "/ui/software-inventory/drafts/reject", url.Values{
		"csrf_token": {csrf}, "draft_id": {"draft-1"},
	})
	if reject.StatusCode != http.StatusSeeOther || reject.Header.Get("Location") != "/ui/software-inventory/drafts?notice=draft-rejected" {
		t.Fatalf("驳回草稿响应不符：%d %s", reject.StatusCode, reject.Header.Get("Location"))
	}
	if !source.rejectCalled || source.rejectID != "draft-1" || source.rejectAddress != "198.51.100.9" {
		t.Fatalf("驳回草稿应交给来源接口：%+v", source)
	}
}

// TestSoftwareInventorySettingsShowsCollectorValidationError 覆盖采集执行者
// 登记表单：校验失败时页面显示领域层给出的原因并保留输入。
func TestSoftwareInventorySettingsShowsCollectorValidationError(t *testing.T) {
	source := &stubSoftwareInventorySource{settings: SoftwareInventorySettings{
		Devices: []SoftwareInventoryDeviceSummary{{
			Name: "工作站", Status: SoftwareInventoryStatus{Managed: true, CollectorLabel: "权威端"},
		}},
		Servers: []SoftwareInventorySettingsServer{{
			Name: "web-1", DeviceName: "工作站", SudoEnabled: false,
		}},
		ClientInstances: []SoftwareInventoryClientInstance{{Name: "笔记本客户端", ProfileName: "运维"}},
	}, collectorErr: &SoftwareInventoryFormError{
		Message: "客户端实例 \"missing\" 不是有效的活动设备码",
	}}
	handler, cookie := newSoftwareInventoryHandler(t, RoleStandalone, source)
	page := doGetWithCookie(t, handler, cookie, "/ui/software-inventory/settings")
	pageBody := readResponseBody(t, page)
	if page.StatusCode != http.StatusOK {
		t.Fatalf("采集设置页状态码 = %d，应为 200：\n%s", page.StatusCode, pageBody)
	}
	csrf := hiddenInputMetaOrHiddenValue(t, pageBody)
	if !strings.Contains(pageBody, "<h1>软件清单采集设置</h1>") ||
		!strings.Contains(pageBody, "工作站") || !strings.Contains(pageBody, "web-1") ||
		!strings.Contains(pageBody, "笔记本客户端") || !strings.Contains(pageBody, "登记采集执行者") {
		t.Fatalf("采集设置页缺少基础内容：\n%s", pageBody)
	}

	response := postSoftwareInventoryForm(t, handler, cookie, "/ui/software-inventory/settings/collector", url.Values{
		"csrf_token": {csrf}, "device": {"工作站"}, "collector": {"missing"},
		"server_entry": {"web-1"},
	})
	responseBody := readResponseBody(t, response)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("采集执行者登记校验失败应返回 400，实际 %d：\n%s", response.StatusCode, responseBody)
	}
	for _, wanted := range []string{
		`value="missing"`, `value="web-1"`,
		"missing",
		"不是有效的活动设备码",
	} {
		if !strings.Contains(responseBody, wanted) {
			t.Fatalf("采集执行者错误页缺少 %q：\n%s", wanted, responseBody)
		}
	}
	if !source.collectorCall || source.collectorAddr != "198.51.100.9" {
		t.Fatalf("采集执行者登记应交给来源接口：%+v", source)
	}
}

// TestSoftwareInventorySudoValidationErrorRendersForm 验证不存在的服务器条
// 目返回表单错误，而不是内部错误页。
func TestSoftwareInventorySudoValidationErrorRendersForm(t *testing.T) {
	source := &stubSoftwareInventorySource{settings: SoftwareInventorySettings{
		Servers: []SoftwareInventorySettingsServer{{Name: "web-1", DeviceName: "工作站"}},
	}, sudoErr: &SoftwareInventoryFormError{Message: "服务器条目 \"missing\" 不存在"}}
	handler, cookie := newSoftwareInventoryHandler(t, RoleStandalone, source)
	page := doGetWithCookie(t, handler, cookie, "/ui/software-inventory/settings")
	pageBody := readResponseBody(t, page)
	csrf := hiddenInputMetaOrHiddenValue(t, pageBody)

	response := postSoftwareInventoryForm(t, handler, cookie, "/ui/software-inventory/settings/sudo", url.Values{
		"csrf_token": {csrf}, "server_entry": {"missing"}, "sudo_enabled": {"0"},
	})
	responseBody := readResponseBody(t, response)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("sudo 表单校验失败应返回 400，实际 %d：\n%s", response.StatusCode, responseBody)
	}
	if !strings.Contains(responseBody, "软件清单采集设置") ||
		!strings.Contains(responseBody, "服务器条目 &#34;missing&#34; 不存在") {
		t.Fatalf("sudo 表单错误页缺少设置页与原因：\n%s", responseBody)
	}
	if !source.sudoCalled || source.sudoServer != "missing" {
		t.Fatalf("sudo 设置应交给来源接口：%+v", source)
	}
}

// TestSoftwareInventorySudoEnableRequiresReverify 验证打开 sudo 采集开关
// 要求最近 5 分钟内重新验证，回跳后保留服务器条目与开关状态。
func TestSoftwareInventorySudoEnableRequiresReverify(t *testing.T) {
	auth := &fakeAuthorityAuth{passwordSet: true, password: "correct-password"}
	now := time.Unix(1800000000, 0)
	source := &stubSoftwareInventorySource{settings: SoftwareInventorySettings{
		Servers: []SoftwareInventorySettingsServer{{Name: "web-1", DeviceName: "工作站"}},
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
	page := authorityGet(t, handler, "127.0.0.1:7879", "/ui/software-inventory/settings", cookie)
	pageBody := readResponseBody(t, page)
	if page.StatusCode != http.StatusOK {
		t.Fatalf("采集设置页状态码 = %d，应为 200：\n%s", page.StatusCode, pageBody)
	}
	csrf := hiddenInputMetaOrHiddenValue(t, pageBody)

	rejected := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/software-inventory/settings/sudo", url.Values{
		"csrf_token": {csrf}, "server_entry": {"web-1"}, "sudo_enabled": {"1"},
	}, cookie)
	wantNext := `/ui/reverify?next=` + url.QueryEscape("/ui/software-inventory/settings?resume=1&server_entry=web-1&sudo_enabled=1")
	if rejected.StatusCode != http.StatusSeeOther || rejected.Header.Get("Location") != wantNext {
		t.Fatalf("未重新验证时打开 sudo 开关应跳转重新验证：%d %s", rejected.StatusCode, rejected.Header.Get("Location"))
	}
	reverifyPage := authorityGet(t, handler, "127.0.0.1:7879", wantNext, cookie)
	reverifyCSRF := hiddenInputValue(t, readResponseBody(t, reverifyPage), "csrf_token")
	verified := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/reverify", url.Values{
		"csrf_token": {reverifyCSRF}, "password": {"correct-password"}, "next": {"/ui/software-inventory/settings?resume=1&server_entry=web-1&sudo_enabled=1"},
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
	applied := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/software-inventory/settings/sudo", url.Values{
		"csrf_token": {resumedCSRF}, "server_entry": {"web-1"}, "sudo_enabled": {"1"},
	}, cookie)
	if applied.StatusCode != http.StatusSeeOther ||
		applied.Header.Get("Location") != "/ui/software-inventory/settings?notice=sudo-enabled" {
		t.Fatalf("重新验证后的 sudo 提交应成功：%d %s", applied.StatusCode, applied.Header.Get("Location"))
	}
	if !source.sudoCalled || source.sudoServer != "web-1" || !source.sudoEnabled || source.sudoAddress != "198.51.100.9" {
		t.Fatalf("sudo 开关应交给来源接口：%+v", source)
	}
}

// TestSoftwareInventoryDevicePageShowsRequiredColumns 覆盖设备清单页的展示
// 要求：类别分组、精确字段、采集状态、立即采集与变化历史。
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
