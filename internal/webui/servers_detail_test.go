package webui

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// serverDetailTopologyFixture 返回带多级地点、两个网络成员与同机另一个账
// 号的拓扑数据；地点故意与物理局域网覆盖不一致，供提示断言使用。
func serverDetailTopologyFixture() *TopologyData {
	return &TopologyData{
		Sites: []TopologySite{
			{ID: "site-1", Name: "公司", LocationPath: "公司"},
			{ID: "site-2", ParentID: "site-1", Name: "一厂区", LocationPath: "公司 / 一厂区"},
			{ID: "site-3", ParentID: "site-2", Name: "3 号楼", LocationPath: "公司 / 一厂区 / 3 号楼"},
			{ID: "site-4", Name: "机房", LocationPath: "机房"},
		},
		Networks: []TopologyNetwork{
			{ID: "net-1", Name: "办公网", Type: "physical", SiteID: "site-3", SiteName: "3 号楼",
				LocationPath: "公司 / 一厂区 / 3 号楼"},
			{ID: "net-2", Name: "机房网", Type: "physical", SiteID: "site-4", SiteName: "机房",
				LocationPath: "机房"},
			{ID: "net-3", Name: "虚拟网", Type: "virtual"},
		},
		Devices: []TopologyDevice{
			{
				ID: "device-1", Name: "工作站", Kind: "server",
				SiteID: "site-4", LocationPath: "机房",
				LocationMismatches: []TopologyLocationMismatch{{
					NetworkID: "net-1", NetworkName: "办公网",
					DeviceLocationPath:  "机房",
					NetworkLocationPath: "公司 / 一厂区 / 3 号楼",
				}},
				Servers: []TopologyDeviceServer{
					{ID: "srv-fine", Name: "fine", User: "root", Host: "192.0.2.10", Port: 22},
					{ID: "srv-other", Name: "other-account", User: "backup", Host: "192.0.2.10", Port: 22},
				},
			},
			{ID: "device-2", Name: "NAS", Kind: "unmanaged", SiteID: "site-4", LocationPath: "机房"},
		},
		Memberships: []TopologyMembership{
			{DeviceID: "device-1", NetworkID: "net-1", Address: "192.0.2.10"},
			{DeviceID: "device-1", NetworkID: "net-3", Address: "100.64.0.10"},
		},
	}
}

func newServerDetailTestStore() *stubServerStore {
	store := newStubServerStore()
	detail := store.details["srv-fine"]
	detail.DeviceName = "工作站"
	detail.HasDevice = true
	detail.Description = "内部服务"
	detail.Hardware = "NUC"
	detail.Services = "Docker"
	detail.Role = "构建机"
	detail.Caveats = "只允许 owner 登录"
	detail.ShowHostKeyAnchor = true
	return store
}

type serverDetailHandlerOptions struct {
	role           Role
	servers        *stubServerStore
	writer         *stubServerWriter
	topology       TopologySource
	topologyWriter TopologyWriter
	software       SoftwareInventorySource
	access         *stubAccessStore
	tester         ServerConnectionTester
}

func newServerDetailTestHandler(t *testing.T, options serverDetailHandlerOptions) (http.Handler, *http.Cookie) {
	t.Helper()
	if options.access == nil {
		options.access = &stubAccessStore{}
	}
	handler, startup, err := New(Config{
		Role: options.role, Servers: options.servers, ServerWriter: options.writer,
		Topology: options.topology, TopologyWriter: options.topologyWriter,
		SoftwareInventory:      options.software,
		ServerConnectionTester: options.tester,
		Access:                 options.access,
		VersionHint:            func() string { return "" }, Port: 18080,
	})
	if err != nil {
		t.Fatalf("构造 Web UI Handler 失败：%v", err)
	}
	response := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookies := response.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("应签发会话 cookie，实际 %d", len(cookies))
	}
	return handler, cookies[0]
}

// TestServerDetailConnectionTabShowsCardsAndInlineEdit 校验详情页吸顶分段
// 控件、连接卡片、五个元数据字段与并入连接块的内联编辑表单。
func TestServerDetailConnectionTabShowsCardsAndInlineEdit(t *testing.T) {
	handler, cookie := newServerDetailTestHandler(t, serverDetailHandlerOptions{
		servers: newServerDetailTestStore(), writer: &stubServerWriter{},
	})

	body := serversGet(t, handler, cookie, "/ui/servers/srv-fine")
	for _, want := range []string{
		`class="segmented server-detail-tabs"`,
		`href="/ui/servers/srv-fine?tab=connection"`, `aria-current="page"`,
		`href="/ui/servers/srv-fine?tab=addresses"`,
		`href="/ui/servers/srv-fine?tab=authorization"`,
		`href="/ui/servers/srv-fine?tab=software"`,
		"连接信息", "登录用户", "root", "端口", "认证方式", "密码",
		"sudo 密码", "主机密钥指纹", "shell 类型",
		"说明信息", "说明", "内部服务", "硬件", "NUC", "服务", "Docker",
		"角色", "构建机", "注意事项", "只允许 owner 登录",
		"危险区", `href="/ui/servers/srv-fine/delete"`, "删除服务器",
		`action="/ui/servers/srv-fine/test"`, "测试连接",
		`href="/ui/servers/srv-fine?tab=connection&amp;edit=1"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("服务器详情连接块缺少 %q：\n%s", want, body)
		}
	}
	if strings.Contains(body, `name="description"`) {
		t.Fatalf("未请求编辑时不应展开编辑表单：\n%s", body)
	}

	editing := serversGet(t, handler, cookie, "/ui/servers/srv-fine?tab=connection&edit=1")
	for _, want := range []string{
		`action="/ui/servers/srv-fine/edit"`, `name="description"`,
		`name="key_file"`, `enctype="multipart/form-data"`, "保存连接信息",
	} {
		if !strings.Contains(editing, want) {
			t.Fatalf("服务器详情内联编辑表单缺少 %q：\n%s", want, editing)
		}
	}
}

// TestServerDetailAddressTabShowsLocationAddressAndAccounts 校验地址与地点
// 块：完整地点路径、网络 + 地址行、不一致提示、同机账号与改挂表单。
func TestServerDetailAddressTabShowsLocationAddressAndAccounts(t *testing.T) {
	handler, cookie := newServerDetailTestHandler(t, serverDetailHandlerOptions{
		servers: newServerDetailTestStore(), writer: &stubServerWriter{},
		topology:       &stubTopologySource{data: serverDetailTopologyFixture()},
		topologyWriter: &stubTopologyWriter{},
	})

	body := serversGet(t, handler, cookie, "/ui/servers/srv-fine?tab=addresses")
	for _, want := range []string{
		"地点", "机房", `class="location-value"`,
		`href="/ui/servers/srv-fine?tab=addresses&amp;location=1"`, "更改",
		"地址", "办公网", "物理局域网", "192.0.2.10",
		"虚拟网", "虚拟局域网", "100.64.0.10",
		"其他（公网或未登记网络）",
		"地点不一致", "机房", "公司 / 一厂区 / 3 号楼",
		"同一台机器上的其他账号", "other-account", "backup",
		"共用这台机器的地址与地点",
		"改挂到另一台机器", `name="device"`, `<option value="NAS">`,
		`action="/ui/servers/srv-fine/address/add"`, `name="network"`, `name="address"`,
		"＋ 添加地址", "机房网",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("服务器详情地址与地点块缺少 %q：\n%s", want, body)
		}
	}

	locationPage := serversGet(t, handler, cookie, "/ui/servers/srv-fine?tab=addresses&location=1")
	for _, want := range []string{
		`action="/ui/servers/srv-fine/location"`, `value="公司 / 一厂区 / 3 号楼"`,
		`value="机房"`, "不登记地点", `name="site"`,
		`href="/ui/more/locations/new?parent=%E6%9C%BA%E6%88%BF`, "新建地点",
	} {
		if !strings.Contains(locationPage, want) {
			t.Fatalf("服务器详情地点选择器缺少 %q：\n%s", want, locationPage)
		}
	}

	bare := serversGet(t, handler, cookie, "/ui/servers/srv-bare?tab=addresses")
	if strings.Contains(bare, "同一台机器上的其他账号") {
		t.Fatalf("没有同机账号时不应显示该卡片：\n%s", bare)
	}
	if !strings.Contains(bare, "未关联机器") {
		t.Fatalf("未关联机器时应有明确说明：\n%s", bare)
	}
}

// TestServerDetailAddressLocationAndMoveWrites 校验地点、地址与改挂写操作
// 都走拓扑写来源并回到地址与地点块。
func TestServerDetailAddressLocationAndMoveWrites(t *testing.T) {
	writer := &stubTopologyWriter{}
	handler, cookie := newServerDetailTestHandler(t, serverDetailHandlerOptions{
		servers: newServerDetailTestStore(), writer: &stubServerWriter{},
		topology:       &stubTopologySource{data: serverDetailTopologyFixture()},
		topologyWriter: writer,
	})

	cases := []struct {
		target string
		form   url.Values
		want   string
	}{
		{
			"/ui/servers/srv-fine/location",
			url.Values{"site": {"机房"}},
			"site-edit",
		},
		{
			"/ui/servers/srv-fine/address/add",
			url.Values{"network": {"机房网"}, "address": {"10.0.0.9"}},
			"address-add",
		},
		{
			"/ui/servers/srv-fine/address/edit",
			url.Values{"network": {"办公网"}, "address": {"192.0.2.11"}},
			"address-edit",
		},
		{
			"/ui/servers/srv-fine/address/remove",
			url.Values{"network": {"虚拟网"}},
			"address-remove",
		},
		{
			"/ui/servers/srv-fine/move",
			url.Values{"device": {"NAS"}},
			"move",
		},
	}
	for _, testCase := range cases {
		response := serversPost(t, handler, cookie, testCase.target, testCase.form)
		if response.StatusCode != http.StatusSeeOther {
			t.Fatalf("%s 状态码 = %d，应为 303", testCase.target, response.StatusCode)
		}
		location := response.Header.Get("Location")
		if !strings.HasPrefix(location, "/ui/servers/srv-fine?tab=addresses&notice=") {
			t.Fatalf("%s 跳转地址 = %q，应回到地址与地点块", testCase.target, location)
		}
	}
	if len(writer.siteEdits) != 1 || writer.siteEdits[0] != [2]string{"工作站", "机房"} {
		t.Fatalf("机器地点写入 = %v，应为 工作站 -> 机房", writer.siteEdits)
	}
	if len(writer.addCalls) != 1 || writer.addCalls[0] != [3]string{"工作站", "机房网", "10.0.0.9"} {
		t.Fatalf("地址新增写入 = %v", writer.addCalls)
	}
	if len(writer.editCalls) != 1 || writer.editCalls[0] != [3]string{"工作站", "办公网", "192.0.2.11"} {
		t.Fatalf("地址编辑写入 = %v", writer.editCalls)
	}
	if len(writer.removeCalls) != 1 || writer.removeCalls[0] != [2]string{"工作站", "虚拟网"} {
		t.Fatalf("地址移除写入 = %v", writer.removeCalls)
	}
	if len(writer.serverMoves) != 1 || writer.serverMoves[0] != [2]string{"srv-fine", "NAS"} {
		t.Fatalf("改挂写入 = %v", writer.serverMoves)
	}
}

// TestServerDetailAuthorizationTabShowsThreeSwitchGroups 校验授权块三组开
// 关：名称与说明两行、机密项环境变量与最近写入、profile 客户端、共享存
// 储提供方与挂载路径，以及各自的确认表单入口。
func TestServerDetailAuthorizationTabShowsThreeSwitchGroups(t *testing.T) {
	store := newServerDetailTestStore()
	detail := store.details["srv-fine"]
	detail.SecretChoices = []ServerDetailSecretChoice{{
		ServerDetailSecret: ServerDetailSecret{
			ID: "secret-1", Name: "API_TOKEN", Note: "接口令牌第一行\n第二行", EnvironmentName: "API_TOKEN",
		},
		Checked: false,
	}}
	detail.ShowDeliveryRecords = true
	detail.DeliveryRecords = []SecretDeliveryRecordRow{{
		ServerName: "fine", Path: "/srv/app/.env", WrittenAt: time.Unix(1760000000, 0),
		Secrets: []SecretDeliverySecretRow{{Name: "API_TOKEN", ValueVersion: 2}},
	}}
	detail.SharedStorageChoices = []ServerDetailSharedStorageChoice{{
		PasswordSecretID: "secret-share", PasswordSecretName: "DATASET_PASSWORD",
		SharedStorages: []ServerDetailSharedStorage{{
			ID: "share-1", Name: "datasets", ProviderDeviceName: "NAS",
			Note:             "数据集备份第一行\n第二行",
			PasswordSecretID: "secret-share", PasswordSecretName: "DATASET_PASSWORD",
			MountPoint: "/mnt/sshmgr/datasets",
		}},
		Checked: true,
	}}
	detail.ProfileChoices = []ServerDetailProfileChoice{{
		ServerDetailProfile: ServerDetailProfile{ID: "profile-1", Name: "全部"},
		Checked:             false,
	}}
	handler, cookie := newServerDetailTestHandler(t, serverDetailHandlerOptions{
		servers: store, writer: &stubServerWriter{},
		access: &stubAccessStore{
			clients: []ClientSummary{{
				ID: "client-1", Name: "laptop-client", ProfileID: "profile-1", ProfileName: "全部",
			}},
		},
	})

	body := serversGet(t, handler, cookie, "/ui/servers/srv-fine?tab=authorization")
	for _, want := range []string{
		"Profile", "机密项", "共享存储",
		`class="resource-name"`, `API_TOKEN`, `class="resource-note"`, "接口令牌第一行",
		"环境变量 $API_TOKEN", "最近写入 /srv/app/.env",
		`name="cell__secret-1__srv-fine"`,
		`action="/ui/secrets/authorization/preview"`, `name="return_to" value="/ui/servers/srv-fine"`,
		"能看到这台服务器的客户端", "laptop-client",
		`action="/ui/servers/profile-authorization/preview"`, `name="profile" value="profile-1"`,
		"由 NAS 提供", "/mnt/sshmgr/datasets",
		"数据集备份第一行",
		`name="authorization_view" value="server-shares"`,
		`name="cell__secret-share__srv-fine"`,
		"下发记录", "/srv/app/.env",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("服务器详情授权块缺少 %q：\n%s", want, body)
		}
	}

	clientHandler, clientCookie := newServerDetailTestHandler(t, serverDetailHandlerOptions{
		role: RoleClient, servers: store,
		topology: &stubTopologySource{data: serverDetailTopologyFixture()},
	})
	clientPage := serversGet(t, clientHandler, clientCookie, "/ui/servers/srv-fine?tab=authorization")
	for _, forbidden := range []string{
		`action="/ui/secrets/authorization/preview"`,
		`action="/ui/servers/profile-authorization/preview"`,
	} {
		if strings.Contains(clientPage, forbidden) {
			t.Fatalf("客户端授权块不得包含写表单 %q：\n%s", forbidden, clientPage)
		}
	}
}

// TestServerDetailSoftwareTabReusesInventory 校验软件块复用软件清单来源：
// 采集状态、立即采集、实测与声明两份清单和待办入口。
func TestServerDetailSoftwareTabReusesInventory(t *testing.T) {
	lastSuccess := time.Unix(1760000100, 0)
	software := &stubSoftwareInventorySource{device: &SoftwareInventoryDevice{
		Name: "工作站",
		Status: SoftwareInventoryStatus{
			Managed: true, CollectorLabel: "权威端", ServerEntry: "fine", LastSuccessAt: &lastSuccess,
		},
		Groups: []SoftwareInventoryGroup{{
			CategoryLabel: "系统包",
			Items: []SoftwareInventoryItem{
				{Name: "nginx", Version: "1.25", Source: "实测", CollectedAt: &lastSuccess},
				{Name: "内部工具", Version: "2.0", Source: "声明", ConfirmedAt: &lastSuccess},
			},
		}},
	}, todos: SoftwareInventoryTodos{
		PendingExposure: []SoftwareInventoryPendingExposureDevice{{
			DeviceName: "工作站",
			Ports: []SoftwareInventoryPendingPort{
				{Address: "192.0.2.10", Port: 8080, Protocol: "tcp", ExposureLabel: "未确认", ObservedStableKey: "stable-web"},
				{Address: "192.0.2.10", Port: 9090, Protocol: "tcp", ExposureLabel: "未确认", ObservedStableKey: "stable-metrics"},
			},
		}, {
			DeviceName: "NAS",
			Ports: []SoftwareInventoryPendingPort{{
				Address: "192.0.2.30", Port: 445, Protocol: "tcp", ExposureLabel: "未确认", ObservedStableKey: "stable-nas",
			}},
		}},
		UnidentifiedProcesses: []SoftwareInventoryUnidentifiedProcess{{
			DeviceName: "工作站", ProcessName: "legacy-agent", ExecutablePath: "/opt/legacy/agent",
			ObservedStableKey: "stable-legacy",
			ListeningPorts:    []SoftwareInventoryPort{{Address: "192.0.2.10", Port: 9100, Protocol: "tcp", ExposureLabel: "未确认"}},
		}, {
			DeviceName: "NAS", ProcessName: "nas-agent", ExecutablePath: "/opt/nas/agent",
			ObservedStableKey: "stable-nas-agent",
		}},
	}}
	handler, cookie := newServerDetailTestHandler(t, serverDetailHandlerOptions{
		servers: newServerDetailTestStore(), writer: &stubServerWriter{},
		software: software,
	})

	body := serversGet(t, handler, cookie, "/ui/servers/srv-fine?tab=software")
	for _, want := range []string{
		"采集状态", "采集执行者", "权威端", "上次成功",
		`action="/ui/software-inventory/collect?device=%E5%B7%A5%E4%BD%9C%E7%AB%99"`,
		`name="return_to" value="/ui/servers/srv-fine?tab=software"`, "立即采集",
		"实测清单", "nginx", "声明清单", "内部工具",
		"本机待确认暴露", "192.0.2.10:8080/tcp", "192.0.2.10:9090/tcp",
		"observed_key=stable-web", "observed_key=stable-metrics", "进入声明表单",
		"本机未识别进程", "legacy-agent", "/opt/legacy/agent", "认领",
		"查看全部待确认暴露", "查看全部未识别进程",
		`href="/ui/software-inventory/todos"`, "待确认暴露",
		`href="/ui/software-inventory/drafts"`, "未识别进程与认领",
		`href="/ui/software-inventory/devices?device=%E5%B7%A5%E4%BD%9C%E7%AB%99"`, "完整软件清单",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("服务器详情软件块缺少 %q：\n%s", want, body)
		}
	}
	if strings.Contains(body, "192.0.2.30:445") || strings.Contains(body, "nas-agent") {
		t.Fatalf("服务器详情软件块只应显示本机前几条待办，不应显示其他机器：%s", body)
	}

	bare := serversGet(t, handler, cookie, "/ui/servers/srv-bare?tab=software")
	if !strings.Contains(bare, "未关联机器") {
		t.Fatalf("未关联机器的软件块应有说明：\n%s", bare)
	}
}

// TestServerDetailTestConnectionStartsTask 校验详情页测试连接启动任务并进
// 入独立结果页。
func TestServerDetailTestConnectionStartsTask(t *testing.T) {
	tester := &stubServerConnectionTester{}
	handler, cookie := newServerDetailTestHandler(t, serverDetailHandlerOptions{
		servers: newServerDetailTestStore(), writer: &stubServerWriter{}, tester: tester,
		topology: &stubTopologySource{data: serverDetailTopologyFixture()},
	})
	response := serversPost(t, handler, cookie, "/ui/servers/srv-fine/test", url.Values{})
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("测试连接状态码 = %d，应为 303", response.StatusCode)
	}
	location := response.Header.Get("Location")
	if !strings.HasPrefix(location, "/ui/servers/new/result?server=srv-fine") ||
		!strings.Contains(location, "source=detail") {
		t.Fatalf("测试连接跳转地址 = %q", location)
	}
	if len(tester.starts) != 1 || tester.starts[0].serverID != "srv-fine" {
		t.Fatalf("测试连接应启动 srv-fine 任务，实际 %v", tester.starts)
	}

	tester.status = &ServerConnectionTestStatus{
		ID: "task-1", State: ServerConnectionTestSucceeded, ServerID: "srv-fine",
		HandshakeAddress: "192.0.2.10:22",
	}
	page := serversGet(t, handler, cookie, location)
	for _, want := range []string{
		"连接测试完成", "走的网络", "办公网", "地址", "192.0.2.10:22",
		`href="/ui/servers/srv-fine?tab=connection"`, "返回服务器详情",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("详情页连接测试结果缺少 %q：\n%s", want, page)
		}
	}
	if strings.Contains(page, "已保存，") || strings.Contains(page, "服务器条目已保存") {
		t.Fatalf("详情页连接测试结果不得沿用新增结果页的已保存文案：\n%s", page)
	}
}

// TestSecretPreviewFromServerDetailShowsClientImpact 校验从服务器详情页提交
// 的机密项开关预览复用 #301 的客户端实际取得途径计算。
func TestSecretPreviewFromServerDetailShowsClientImpact(t *testing.T) {
	access := &stubAccessStore{
		profiles: []AccessProfileSummary{{
			ID: "profile-1", Name: "运维",
			Servers: []AccessServerRef{{ID: "srv-db", Name: "db-1"}},
		}},
		clients: []ClientSummary{{
			ID: "client-1", Name: "笔记本", ProfileID: "profile-1", ProfileName: "运维",
		}},
	}
	servers := &stubServerSource{rows: []ServerRow{
		{ID: "srv-db", Name: "db-1"}, {ID: "srv-web", Name: "web-1"},
	}}
	writer := &authorizationSecretWriter{preview: SecretAuthorizationPreview{
		Changes: []SecretAuthorizationChange{{
			SecretID: "secret-1", SecretName: "MATRIX_TOKEN",
			Added: []SecretAuthorizationServer{{ID: "srv-db", Name: "db-1"}},
		}},
		TotalAdded: 1, Summary: "授权变更摘要",
	}}
	handler, startup, err := New(Config{
		Role: RoleStandalone, Servers: servers, Access: access,
		Secrets: authorizationSecretSource{rows: []SecretRow{{
			ID: "secret-1", Name: "MATRIX_TOKEN", Note: "浏览器测试机密项",
			AllowedServers: []string{"web-1"},
		}}},
		SecretsWriter: writer, Port: 18080,
	})
	if err != nil {
		t.Fatalf("构造 Web UI Handler 失败：%v", err)
	}
	session := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookie := session.Cookies()[0]
	page := doGetWithCookie(t, handler, cookie, "/ui/secrets/")
	csrf := hiddenInputMetaOrHiddenValue(t, readResponseBody(t, page))
	response := postSecretForm(t, handler, cookie, "/ui/secrets/authorization/preview", url.Values{
		"csrf_token": {csrf}, "return_to": {"/ui/servers/srv-db"},
		"secret_id": {"secret-1"}, "server_id": {"srv-db"},
		secretCellName("secret-1", "srv-db"): {"1"},
	})
	body := readResponseBody(t, response)
	for _, want := range []string{
		"实际能拿到它的客户端如何变化", "笔记本", "现在无法拿到", "提交后经 db-1",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("服务器详情来源的机密项预览缺少 %q：\n%s", want, body)
		}
	}
}
