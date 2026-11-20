package webui

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// moreMenuAuthority 构造带各区域数据的权威端处理器，供“更多”菜单测试使用。
func moreMenuAuthority(t *testing.T) http.Handler {
	t.Helper()
	handler, _, err := NewAuthority(Config{
		Role:    RoleServer,
		Servers: &stubServerSource{},
		Access:  &stubAccessStore{},
		SoftwareInventory: &stubSoftwareInventorySource{devices: []SoftwareInventoryDeviceSummary{
			{Name: "工作站", Status: SoftwareInventoryStatus{Managed: true}, StatusLabels: []string{"正常"}},
			{Name: "软路由", StatusLabels: []string{"不采集（不受管设备）"}},
		}, todos: SoftwareInventoryTodos{
			UnidentifiedProcesses: []SoftwareInventoryUnidentifiedProcess{{DeviceName: "工作站"}},
			StaleDevices:          []SoftwareInventoryStaleDevice{{DeviceName: "软路由"}},
		}},
		Ops: &stubOpsStore{backupStatus: OpsBackupStatus{
			HasLatest: true, LatestTime: time.Unix(1800000000, 0),
		}},
		Topology: &stubTopologySource{data: &TopologyData{
			Sites: []TopologySite{
				{ID: "site-1", Name: "公司"},
				{ID: "site-2", Name: "一厂区", ParentID: "site-1"},
				{ID: "site-3", Name: "机房"},
			},
			Networks: []TopologyNetwork{
				{ID: "net-1", Name: "办公网", Type: "physical"},
				{ID: "net-2", Name: "虚拟网", Type: "virtual"},
			},
		}},
		Auth:         &fakeAuthorityAuth{passwordSet: true, password: "correct-password"},
		AllowedHosts: []string{"127.0.0.1"},
		Port:         7879,
	})
	if err != nil {
		t.Fatalf("构造权威端 Web UI 失败：%v", err)
	}
	return handler
}

func TestMoreMenuShowsGroupsSummariesAndAccountActions(t *testing.T) {
	handler := moreMenuAuthority(t)
	cookie := authoritySuccessfulLogin(t, handler, "127.0.0.1:7879", "correct-password")

	home := authorityGet(t, handler, "127.0.0.1:7879", "/ui/", cookie)
	homeBody := readResponseBody(t, home)
	if strings.Contains(homeBody, "header-actions") {
		t.Fatalf("顶部条不应再放账号操作，应移入“更多”菜单：\n%s", homeBody)
	}
	if !strings.Contains(homeBody, `href="/ui/more/"`) {
		t.Fatalf("一级导航的“更多”标签应指向新菜单页：\n%s", homeBody)
	}

	response := authorityGet(t, handler, "127.0.0.1:7879", "/ui/more/", cookie)
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "<h1>更多</h1>") {
		t.Fatalf("权威端应打开“更多”菜单，状态码=%d 页面：\n%s", response.StatusCode, body)
	}
	for _, want := range []string{
		"资产", "软件清单", "2 项需要处理", `/ui/software-inventory/`,
		"地点与网络", "3 个地点 · 2 个网络", `/ui/more/locations-networks/`,
		"网络拓扑图", `/ui/topology/`,
		"运维", "备份", "最近备份：2027-01-15", `/ui/ops/backup`,
		"升级", `/ui/ops/update`, "设置", `/ui/ops/settings`, "审计日志", `/ui/ops/audit`,
		"高级", "MCP 访问令牌", `/ui/ops/access-tokens`, "手动添加客户端", `/ui/ops/clients/new`,
		"账号", "修改管理员口令", `/ui/password`,
		`action="/ui/logout"`, "注销",
		`action="/ui/logout-all"`, "注销全部会话",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("“更多”菜单缺少 %q：\n%s", want, body)
		}
	}
	for _, forbidden := range []string{"设备码", "项目令牌", "站点", "网络成员", "登录条目", "兜底主机", "cache token"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("“更多”菜单不应出现 %q：\n%s", forbidden, body)
		}
	}
}

func TestMoreMenuHidesUnavailableRowsByRole(t *testing.T) {
	standalone := func(role Role) string {
		t.Helper()
		cfg := Config{
			Role: role, Servers: &stubServerSource{}, Access: &stubAccessStore{},
			Ops: &stubOpsStore{}, Topology: &stubTopologySource{data: &TopologyData{}},
			VersionHint: func() string { return "" }, Port: 18080,
		}
		if role == RoleClient {
			cfg.Access = nil
		}
		handler, startup, err := New(cfg)
		if err != nil {
			t.Fatalf("构造 Web UI Handler 失败：%v", err)
		}
		session := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
		cookie := session.Cookies()[0]
		session.Body.Close()
		return readResponseBody(t, doGetWithCookie(t, handler, cookie, "/ui/more/"))
	}

	standaloneBody := standalone(RoleStandalone)
	for _, want := range []string{"资产", "地点与网络", "运维", "备份", "设置"} {
		if !strings.Contains(standaloneBody, want) {
			t.Fatalf("单机“更多”菜单应显示 %q：\n%s", want, standaloneBody)
		}
	}
	for _, hidden := range []string{"手动添加客户端", "账号", "修改管理员口令", "注销全部会话"} {
		if strings.Contains(standaloneBody, hidden) {
			t.Fatalf("单机“更多”菜单不应显示 %q：\n%s", hidden, standaloneBody)
		}
	}

	clientBody := standalone(RoleClient)
	for _, want := range []string{"网络拓扑图", "仅查看", "客户端本机审计"} {
		if !strings.Contains(clientBody, want) {
			t.Fatalf("客户端“更多”菜单应显示 %q：\n%s", want, clientBody)
		}
	}
	for _, hidden := range []string{"软件清单", "备份", "设置", "MCP 访问令牌", "修改管理员口令"} {
		if strings.Contains(clientBody, hidden) {
			t.Fatalf("客户端“更多”菜单不应显示 %q：\n%s", hidden, clientBody)
		}
	}
}
