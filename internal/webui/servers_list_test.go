package webui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
)

type serverListTopologySource struct{}

func (serverListTopologySource) Topology(string, string) (*TopologyData, error) {
	return &TopologyData{
		Sites:    []TopologySite{{ID: "site-1", Name: "家A", LocationPath: "家A"}},
		Networks: []TopologyNetwork{{ID: "net-physical", Name: "家A局域网", Type: "physical"}},
		Devices:  []TopologyDevice{{ID: "device-1", Name: "工作站", SiteID: "site-1", Servers: []TopologyDeviceServer{{ID: "srv-fine"}, {ID: "srv-bare"}}}},
		Memberships: []TopologyMembership{
			{DeviceID: "device-1", NetworkID: "net-physical", Address: "192.0.2.10"},
		},
	}, nil
}

type serverListHierarchicalTopologySource struct{}

func (serverListHierarchicalTopologySource) Topology(string, string) (*TopologyData, error) {
	return &TopologyData{
		Sites: []TopologySite{
			{ID: "site-company", ParentID: "", Name: "公司", LocationPath: "公司"},
			{ID: "site-factory", ParentID: "site-company", Name: "一厂区", LocationPath: "公司 / 一厂区"},
			{ID: "site-building", ParentID: "site-factory", Name: "3 号楼", LocationPath: "公司 / 一厂区 / 3 号楼"},
			{ID: "site-home", ParentID: "", Name: "家A", LocationPath: "家A"},
		},
		Networks: []TopologyNetwork{
			{ID: "net-office", Name: "公司办公局域网", Type: "physical", SiteID: "site-company", LocationPath: "公司", NetworkRange: "10.20.0.0/16"},
			{ID: "net-virtual", Name: "跨地点虚拟局域网", Type: "virtual"},
		},
		Devices: []TopologyDevice{
			{ID: "device-root", Name: "总部机器", SiteID: "site-company", LocationPath: "公司", Servers: []TopologyDeviceServer{{ID: "srv-root"}}},
			{ID: "device-factory", Name: "厂区机器", SiteID: "site-factory", LocationPath: "公司 / 一厂区", Servers: []TopologyDeviceServer{{ID: "srv-factory"}}},
			{ID: "device-building", Name: "楼内机器", SiteID: "site-building", LocationPath: "公司 / 一厂区 / 3 号楼", Servers: []TopologyDeviceServer{{ID: "srv-building"}}},
			{ID: "device-home", Name: "家里机器", SiteID: "site-home", LocationPath: "家A", Servers: []TopologyDeviceServer{{ID: "srv-home"}}},
			{ID: "device-missing-site", Name: "未登记地点机器", SiteID: "", Servers: []TopologyDeviceServer{{ID: "srv-missing-site"}}},
		},
		Memberships: []TopologyMembership{
			{DeviceID: "device-root", NetworkID: "net-office"},
			{DeviceID: "device-factory", NetworkID: "net-office"},
			{DeviceID: "device-building", NetworkID: "net-virtual"},
			{DeviceID: "device-missing-site", NetworkID: "net-office"},
		},
	}, nil
}

func TestServerListCardsShowFiltersAndBatchEntrances(t *testing.T) {
	store := newStubServerStore()
	store.rows = append(store.rows, ServerListEntry{
		ID: "srv-work", Name: "work", User: "root", Host: "192.0.2.20", Port: 22,
	})
	handler, startup, err := New(Config{
		Role: RoleStandalone, Servers: store, ServerWriter: &stubServerWriter{},
		Topology: serverListTopologySource{}, Access: &stubAccessStore{},
		VersionHint: func() string { return "" }, Port: 18080,
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie := exchangeStartupTokenForCookie(t, handler, startup)
	h := handler
	page := serversGet(t, h, cookie, "/ui/servers/")

	for _, want := range []string{
		`aria-label="搜索服务器"`, `aria-label="按地点筛选"`, `aria-label="按网络筛选"`,
		"root@192.0.2.10:22", "root@192.0.2.11:22",
		"家A", "家A局域网", "能看见", "个客户端", "个机密项", "个共享存储", "同机 2 个账号",
		"server-attention-tags", "未挂凭据", "未设角色", "需要注意",
		"加入 profile", "授权机密项", "授权共享存储", "导入 SSH 配置",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("服务器列表缺少 %q：\n%s", want, page)
		}
	}
	if strings.Index(page, "bare") > strings.Index(page, "fine") {
		t.Fatalf("需要注意的卡片应排在最前：\n%s", page)
	}
	filtered := serversGet(t, h, cookie, "/ui/servers/?"+url.Values{
		"query": {"fine"}, "site": {"site-1"}, "network": {"net-physical"},
	}.Encode())
	if strings.Contains(filtered, `value="srv-bare"`) || !strings.Contains(filtered, `value="srv-fine"`) {
		t.Fatalf("搜索与筛选应只留下匹配卡片：\n%s", filtered)
	}
}

// TestServerListCardsShowConnectionTestStatus 校验列表卡片底部状态行：成
// 功、失败、从未测试三种视觉状态可区分，成功带实际地址、网络与耗时，列
// 表不出现候选地址展开与批量测试连接入口。
func TestServerListCardsShowConnectionTestStatus(t *testing.T) {
	store := newStubServerStore()
	store.rows = []ServerListEntry{
		{ID: "srv-ok", Name: "ok", User: "root", Host: "192.0.2.10", Port: 22,
			ConnectionTest: &ServerConnectionTestResultView{
				State: ServerConnectionTestSucceeded, Succeeded: true,
				CompletedAt:    time.Unix(1_782_000_000, 0),
				SuccessAddress: "192.0.2.10", SuccessNetworkName: "办公网",
				SuccessSource: models.DialAddressSourcePhysical, Duration: 125 * time.Millisecond,
			}},
		{ID: "srv-down", Name: "down", User: "root", Host: "192.0.2.11", Port: 22,
			ConnectionTest: &ServerConnectionTestResultView{
				State: ServerConnectionTestFailed, CompletedAt: time.Unix(1_782_000_100, 0),
				FailureLabel: "端口不通", Duration: 40 * time.Millisecond,
			}},
		{ID: "srv-new", Name: "new", User: "root", Host: "192.0.2.12", Port: 22},
		{ID: "srv-no-net", Name: "no-net", User: "root", Host: "192.0.2.13", Port: 22,
			ConnectionTest: &ServerConnectionTestResultView{
				State: ServerConnectionTestSucceeded, Succeeded: true,
				CompletedAt:    time.Unix(1_782_000_200, 0),
				SuccessAddress: "192.0.2.13", SuccessNetworkName: "",
				SuccessSource: models.DialAddressSourcePhysical, Duration: 80 * time.Millisecond,
			}},
	}
	handler, startup, err := New(Config{
		Role: RoleStandalone, Servers: store, ServerWriter: &stubServerWriter{},
		Topology: serverListTopologySource{}, Access: &stubAccessStore{},
		VersionHint: func() string { return "" }, Port: 18080,
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie := exchangeStartupTokenForCookie(t, handler, startup)
	page := serversGet(t, handler, cookie, "/ui/servers/")

	success := regexp.MustCompile(`server-test-line is-succeeded[^\v]*?aria-hidden="true"[^\v]*?已连上`).FindString(page)
	if success == "" {
		t.Fatalf("成功卡片应有带小圆点的已连上状态行：\n%s", page)
	}
	for _, want := range []string{
		"192.0.2.10 · 办公网 · 耗时 125 毫秒",
		`server-test-line is-failed`, "连不上：端口不通",
		`server-test-line is-untested`, "从未测试",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("服务器列表状态行缺少 %q：\n%s", want, page)
		}
	}
	if regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} (&#43;|\+|-)\d{2}:\d{2} 已连上`).MatchString(page) {
		t.Fatalf("服务器列表卡片应使用短时间格式：\n%s", page)
	}
	if !regexp.MustCompile(`((今天|昨天) \d{2}:\d{2}|\d{1,2}月\d{1,2}日 \d{2}:\d{2}|\d{4}年\d{1,2}月\d{1,2}日) 已连上`).MatchString(page) {
		t.Fatalf("成功状态行应显示短时间：\n%s", page)
	}
	if regexp.MustCompile(`·\s*·`).MatchString(page) {
		t.Fatalf("网络名为空时不得输出空段与连续分隔点：\n%s", page)
	}
	if !strings.Contains(page, "192.0.2.13 · 耗时 80 毫秒") {
		t.Fatalf("网络名为空时应保留地址与耗时：\n%s", page)
	}
	if strings.Contains(page, "connection-test-candidate") || strings.Contains(page, "批量测试连接") {
		t.Fatalf("列表不应出现候选地址展开或批量测试连接：\n%s", page)
	}
}

func TestServerListFiltersUseLocationSubtreeUnregisteredAndNetworkMembership(t *testing.T) {
	store := newStubServerStore()
	store.rows = []ServerListEntry{
		{ID: "srv-root", Name: "root", User: "owner", Host: "10.20.0.10", Port: 22},
		{ID: "srv-factory", Name: "factory", User: "owner", Host: "10.20.1.10", Port: 22},
		{ID: "srv-building", Name: "building", User: "owner", Host: "10.20.2.10", Port: 22,
			AttentionReasons: []string{"未挂凭据"}},
		{ID: "srv-home", Name: "home", User: "owner", Host: "192.168.1.10", Port: 22},
		{ID: "srv-missing-site", Name: "missing-site", User: "owner", Host: "10.20.3.10", Port: 22},
		{ID: "srv-no-device", Name: "no-device", User: "owner", Host: "203.0.113.10", Port: 22},
	}
	handler, startup, err := New(Config{
		Role: RoleStandalone, Servers: store, ServerWriter: &stubServerWriter{},
		Topology: serverListHierarchicalTopologySource{}, Access: &stubAccessStore{},
		VersionHint: func() string { return "" }, Port: 18080,
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie := exchangeStartupTokenForCookie(t, handler, startup)

	page := serversGet(t, handler, cookie, "/ui/servers/")
	for _, want := range []string{
		"公司 / 一厂区 / 3 号楼", "包含全部下级地点", "未登记地点",
		"没有关联机器，或机器尚未选择地点",
		"公司办公局域网", "物理局域网 · 网段 10.20.0.0/16 · 覆盖 公司",
		"跨地点虚拟局域网", "虚拟局域网 · 横跨多个地点", "6 台",
		"公司 · 包含全部下级地点 · 3 台", "未登记地点 · 没有关联机器，或机器尚未选择地点 · 2 台",
		"公司办公局域网 · 物理局域网 · 网段 10.20.0.0/16 · 覆盖 公司 · 3 台",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("筛选选项缺少 %q：\n%s", want, page)
		}
	}

	company := serversGet(t, handler, cookie, "/ui/servers/?site=site-company")
	for _, want := range []string{"srv-root", "srv-factory", "srv-building"} {
		if !strings.Contains(company, `value="`+want+`"`) {
			t.Fatalf("选择上级地点应包含下级服务器 %q：\n%s", want, company)
		}
	}
	for _, unwanted := range []string{"srv-home", "srv-missing-site", "srv-no-device"} {
		if strings.Contains(company, `value="`+unwanted+`"`) {
			t.Fatalf("选择上级地点不应包含范围外服务器 %q：\n%s", unwanted, company)
		}
	}

	building := serversGet(t, handler, cookie, "/ui/servers/?site=site-building")
	if !strings.Contains(building, `value="srv-building"`) || strings.Contains(building, `value="srv-root"`) ||
		strings.Contains(building, `value="srv-factory"`) {
		t.Fatalf("选择下级地点不应包含上级或其他分支：\n%s", building)
	}

	unregistered := serversGet(t, handler, cookie, "/ui/servers/?site=__unregistered__")
	if !strings.Contains(unregistered, `value="srv-missing-site"`) || !strings.Contains(unregistered, `value="srv-no-device"`) ||
		strings.Contains(unregistered, `value="srv-root"`) {
		t.Fatalf("未登记地点应只包含没有关联机器或机器未登记地点的服务器：\n%s", unregistered)
	}

	network := serversGet(t, handler, cookie, "/ui/servers/?network=net-office")
	if !strings.Contains(network, `value="srv-root"`) || !strings.Contains(network, `value="srv-missing-site"`) ||
		strings.Contains(network, `value="srv-building"`) || strings.Contains(network, `value="srv-no-device"`) {
		t.Fatalf("网络筛选应按设备是否为网络成员匹配：\n%s", network)
	}

	combined := serversGet(t, handler, cookie, "/ui/servers/?site=site-company&network=net-virtual&query=building&attention=1")
	if !strings.Contains(combined, `value="srv-building"`) || strings.Contains(combined, `value="srv-root"`) ||
		strings.Contains(combined, `value="srv-factory"`) {
		t.Fatalf("地点、网络与需要注意筛选应取交集：\n%s", combined)
	}
}

func TestServerListFilterPillsNoScriptFallbackAndEmptyState(t *testing.T) {
	store := newStubServerStore()
	store.rows = []ServerListEntry{
		{ID: "srv-building", Name: "building", User: "owner", Host: "10.20.2.10", Port: 22},
		{ID: "srv-home", Name: "home", User: "owner", Host: "192.168.1.10", Port: 22},
	}
	handler, startup, err := New(Config{
		Role: RoleStandalone, Servers: store, ServerWriter: &stubServerWriter{},
		Topology: serverListHierarchicalTopologySource{}, Access: &stubAccessStore{},
		VersionHint: func() string { return "" }, Port: 18080,
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie := exchangeStartupTokenForCookie(t, handler, startup)

	filtered := serversGet(t, handler, cookie, "/ui/servers/?site=site-company&network=net-virtual&query=building")
	for _, want := range []string{
		`data-server-filter-form`,
		`data-server-filter-live="polite"`,
		"地点：公司", `/ui/servers/?network=net-virtual&amp;query=building`,
		"网络：跨地点虚拟局域网", `/ui/servers/?query=building&amp;site=site-company`,
		"搜索：building", `/ui/servers/?network=net-virtual&amp;site=site-company`,
		"清除全部", `href="/ui/servers/"`,
		`type="submit"`, "应用筛选",
	} {
		if !strings.Contains(filtered, want) {
			t.Fatalf("筛选视图缺少 %q：\n%s", want, filtered)
		}
	}

	// 定稿原型的一键清除只清地点、网络与搜索；「只看需要注意」不在胶囊
	// 里，应保持原状，避免用户看不见的条件被一并重置。
	empty := serversGet(t, handler, cookie, "/ui/servers/?site=site-home&query=not-found&attention=1")
	for _, want := range []string{"没有符合条件的服务器", "清除筛选", `/ui/servers/`} {
		if !strings.Contains(empty, want) {
			t.Fatalf("无结果空态缺少 %q：\n%s", want, empty)
		}
	}
	for _, want := range []string{
		`href="/ui/servers/?attention=1">清除全部`,
		`href="/ui/servers/?attention=1">清除筛选`,
	} {
		if !strings.Contains(empty, want) {
			t.Fatalf("一键清除应保留「只看需要注意」，缺少 %q：\n%s", want, empty)
		}
	}
}

func TestServerListClientRoleCanFilterWithoutBatchEntrances(t *testing.T) {
	store := newStubServerStore()
	store.rows = []ServerListEntry{{ID: "srv-building", Name: "building", User: "owner", Host: "10.20.2.10", Port: 22}}
	handler, startup, err := New(Config{
		Role: RoleClient, Servers: store, Topology: serverListHierarchicalTopologySource{},
		Access: &stubAccessStore{}, VersionHint: func() string { return "" }, Port: 18080,
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie := exchangeStartupTokenForCookie(t, handler, startup)

	page := serversGet(t, handler, cookie, "/ui/servers/?site=site-company&network=net-virtual")
	for _, want := range []string{"按地点筛选", "按网络筛选", "地点：公司", "网络：跨地点虚拟局域网"} {
		if !strings.Contains(page, want) {
			t.Fatalf("客户端应能使用全部筛选，缺少 %q：\n%s", want, page)
		}
	}
	for _, forbidden := range []string{"server-select-control", "data-server-selection-bar", "data-server-batch-drawer", "导入 SSH 配置"} {
		if strings.Contains(page, forbidden) {
			t.Fatalf("客户端筛选页不应出现批量写入口 %q：\n%s", forbidden, page)
		}
	}
}

func TestOldServerAddTypePageRedirectsToWizard(t *testing.T) {
	h, cookie := newServersTestHandlerWithTopology(t, RoleStandalone, newStubServerStore(), &stubServerWriter{})
	request := httptest.NewRequest(http.MethodGet, "/ui/add?network="+url.QueryEscape("家A局域网"), nil)
	request.Host = "127.0.0.1:18080"
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	response := recorder.Result()
	if response.StatusCode != 303 {
		t.Fatalf("旧类型选择页应跳转，状态码=%d", response.StatusCode)
	}
	if location := response.Header.Get("Location"); !strings.Contains(location, "/ui/servers/new") ||
		!strings.Contains(location, url.QueryEscape("家A局域网")) {
		t.Fatalf("跳转应保留网络预选参数：%s", location)
	}
}

func exchangeStartupTokenForCookie(t *testing.T, handler http.Handler, startup Startup) *http.Cookie {
	t.Helper()
	response := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookies := response.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("应签发会话 cookie，实际 %d 个", len(cookies))
	}
	return cookies[0]
}

// serverListPreferredAddressTopologyData 构造空主机条目与首选网络地址场
// 景用的拓扑数据：一台带物理与虚拟局域网候选的工作站、一台只有成员地址
// 没有候选的裸机。
func serverListPreferredAddressTopologyData() *TopologyData {
	return &TopologyData{
		Networks: []TopologyNetwork{
			{ID: "net-lan", Name: "家B局域网", Type: "physical"},
			{ID: "net-vpn", Name: "家B虚拟网", Type: "virtual"},
		},
		Devices: []TopologyDevice{
			{
				ID: "device-lan", Name: "工作站B",
				Servers: []TopologyDeviceServer{{ID: "srv-empty"}, {ID: "srv-full"}},
				Candidates: []TopologyCandidate{
					{Address: "192.0.2.50", Source: "physical"},
					{Address: "100.64.0.50", Source: "virtual"},
				},
			},
			{
				ID: "device-bare", Name: "裸机B",
				Servers: []TopologyDeviceServer{{ID: "srv-bare-host"}},
			},
		},
		Memberships: []TopologyMembership{
			{DeviceID: "device-lan", NetworkID: "net-lan", Address: "192.0.2.50"},
			{DeviceID: "device-lan", NetworkID: "net-vpn", Address: "100.64.0.50"},
			{DeviceID: "device-bare", NetworkID: "net-lan", Address: "10.9.9.9"},
		},
	}
}

// newServerListPreferredAddressStore 构造空主机与无候选场景用的服务器列
// 表数据；标识符与地址独立成套，避免与既有测试桩串扰。
func newServerListPreferredAddressStore() *stubServerStore {
	store := newStubServerStore()
	store.rows = append(store.rows, []ServerListEntry{
		{ID: "srv-empty", Name: "empty", User: "root", Host: "", Port: 22},
		{ID: "srv-full", Name: "full", User: "owner", Host: "203.0.113.9", Port: 22},
		{ID: "srv-bare-host", Name: "bare-host", User: "backup", Host: "", Port: 22022},
	}...)
	store.details["srv-empty"] = &ServerDetailView{
		ID: "srv-empty", Name: "empty", User: "root", Host: "", Port: 22,
		Shell: "posix", DeviceName: "工作站B", HasDevice: true,
	}
	return store
}

// TestServerListCardsShowPreferredNetworkAddressForEmptyHost 校验列表卡片
// 地址行：条目主机为空时显示设备连接候选里的首选网络地址，没有候选时退化
// 为用户与端口，主机非空的条目显示不变。
func TestServerListCardsShowPreferredNetworkAddressForEmptyHost(t *testing.T) {
	store := newServerListPreferredAddressStore()
	handler, startup, err := New(Config{
		Role: RoleStandalone, Servers: store, ServerWriter: &stubServerWriter{},
		Topology: &stubTopologySource{data: serverListPreferredAddressTopologyData()},
		Access:   &stubAccessStore{}, VersionHint: func() string { return "" }, Port: 18080,
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie := exchangeStartupTokenForCookie(t, handler, startup)
	page := serversGet(t, handler, cookie, "/ui/servers/")
	for _, want := range []string{
		"root@192.0.2.50:22",
		"backup · 端口 22022",
		"owner@203.0.113.9:22",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("服务器列表地址行缺少 %q：\n%s", want, page)
		}
	}
	if strings.Contains(page, "@:") {
		t.Fatalf("主机为空的条目不得显示空主机地址 root@:22：\n%s", page)
	}
}

// TestServerListSearchMatchesMembershipAddresses 校验列表搜索收录设备全
// 部网络成员地址：候选地址与不在候选里的成员地址都能命中对应条目，命中后
// 无候选条目的地址行仍退化为用户与端口。
func TestServerListSearchMatchesMembershipAddresses(t *testing.T) {
	store := newServerListPreferredAddressStore()
	handler, startup, err := New(Config{
		Role: RoleStandalone, Servers: store, ServerWriter: &stubServerWriter{},
		Topology: &stubTopologySource{data: serverListPreferredAddressTopologyData()},
		Access:   &stubAccessStore{}, VersionHint: func() string { return "" }, Port: 18080,
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie := exchangeStartupTokenForCookie(t, handler, startup)
	for _, query := range []string{"192.0.2.50", "100.64.0.50"} {
		filtered := serversGet(t, handler, cookie, "/ui/servers/?"+url.Values{"query": {query}}.Encode())
		if !strings.Contains(filtered, `value="srv-empty"`) {
			t.Fatalf("按成员地址 %s 搜索应命中空主机条目 srv-empty：\n%s", query, filtered)
		}
	}
	bare := serversGet(t, handler, cookie, "/ui/servers/?"+url.Values{"query": {"10.9.9.9"}}.Encode())
	if !strings.Contains(bare, `value="srv-bare-host"`) {
		t.Fatalf("按成员地址 10.9.9.9 搜索应命中裸机条目 srv-bare-host：\n%s", bare)
	}
	if !strings.Contains(bare, "backup · 端口 22022") {
		t.Fatalf("搜索命中的无候选条目地址行仍应退化为用户与端口：\n%s", bare)
	}
}

// TestServerListCardAndDetailHeaderSharePreferredNetworkAddress 校验列表卡
// 片与详情页页头对同一个空主机条目算出同一个首选网络地址，且两处读取拓扑
// 都以本机设备为起点。
func TestServerListCardAndDetailHeaderSharePreferredNetworkAddress(t *testing.T) {
	store := newServerListPreferredAddressStore()
	source := &stubTopologySource{data: serverListPreferredAddressTopologyData()}
	handler, startup, err := New(Config{
		Role: RoleStandalone, Servers: store, ServerWriter: &stubServerWriter{},
		Topology: source, Access: &stubAccessStore{},
		VersionHint: func() string { return "" }, Port: 18080,
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie := exchangeStartupTokenForCookie(t, handler, startup)
	list := serversGet(t, handler, cookie, "/ui/servers/")
	if !strings.Contains(list, "root@192.0.2.50:22") {
		t.Fatalf("列表卡片应显示首选网络地址：\n%s", list)
	}
	detail := serversGet(t, handler, cookie, "/ui/servers/srv-empty")
	if !strings.Contains(detail, "root@192.0.2.50:22") {
		t.Fatalf("详情页页头应显示与列表卡片相同的首选网络地址：\n%s", detail)
	}
	if source.from != TopologyProbeOriginSelf {
		t.Fatalf("列表与详情页读取拓扑应都以本机设备为起点，实际 %q", source.from)
	}
}

// TestServerListClientRoleShowsPreferredNetworkAddress 校验客户端只读的服
// 务器列表同样显示首选网络地址，不出现空主机地址。
func TestServerListClientRoleShowsPreferredNetworkAddress(t *testing.T) {
	store := newServerListPreferredAddressStore()
	handler, startup, err := New(Config{
		Role: RoleClient, Servers: store,
		Topology: &stubTopologySource{data: serverListPreferredAddressTopologyData()},
		Access:   &stubAccessStore{}, VersionHint: func() string { return "" }, Port: 18080,
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie := exchangeStartupTokenForCookie(t, handler, startup)
	page := serversGet(t, handler, cookie, "/ui/servers/")
	for _, want := range []string{"root@192.0.2.50:22", "backup · 端口 22022"} {
		if !strings.Contains(page, want) {
			t.Fatalf("客户端列表地址行缺少 %q：\n%s", want, page)
		}
	}
	if strings.Contains(page, "@:") {
		t.Fatalf("客户端列表主机为空的条目不得显示空主机地址：\n%s", page)
	}
}
