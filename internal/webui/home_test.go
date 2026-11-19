package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAuthorityHomePageShowsOverviewAndFiveTabs(t *testing.T) {
	auth := &fakeAuthorityAuth{passwordSet: true, password: "correct-password"}
	access := &stubAccessStore{
		profiles: []AccessProfileSummary{{ID: "profile-1", Name: "全部"}, {ID: "profile-2", Name: "训练机"}},
		deviceCodes: []AccessDeviceCodeSummary{
			{ID: "device-1", Name: "mini6-claude", Status: "active"},
			{ID: "device-2", Name: "old-client", Status: "revoked"},
		},
		pairings: []AccessPairingSummary{
			{ID: "pair-1", Name: "macbook-air", State: "pending"},
			{ID: "pair-2", Name: "old-request", State: "completed"},
		},
	}
	software := &stubSoftwareInventorySource{todos: SoftwareInventoryTodos{
		UnidentifiedProcesses: []SoftwareInventoryUnidentifiedProcess{
			{DeviceName: "工作站", Name: "业务网关"},
			{DeviceName: "工作站", Name: "数据网关"},
		},
		StaleDevices: []SoftwareInventoryStaleDevice{{DeviceName: "软路由"}},
	}}
	ops := &stubOpsStore{rows: []OpsAuditRow{
		{ID: 12, TS: time.Unix(1800000120, 0), Action: "cache.pull", Command: "客户端 mini6-claude 完成同步", Status: "ok"},
		{ID: 11, TS: time.Unix(1800000060, 0), Action: "secret.grant", Command: "机密项 HF_TOKEN 授权给 3090x2", Status: "ok"},
	}}
	handler, _, err := NewAuthority(Config{
		Role: RoleServer,
		Servers: &stubServerSource{rows: []ServerRow{
			{ID: "srv-web", Name: "web-1", User: "deploy", Host: "192.0.2.10", Port: 22},
			{ID: "srv-db", Name: "db-1", User: "root", Host: "192.0.2.11", Port: 22, NeedsAttention: true},
			{ID: "srv-gpu", Name: "gpu-1", User: "root", Host: "192.0.2.12", Port: 22, NeedsAttention: true},
		}},
		Access:            access,
		SoftwareInventory: software,
		Ops:               ops,
		Auth:              auth,
		AllowedHosts:      []string{"127.0.0.1"},
		Port:              7879,
		Now:               func() time.Time { return time.Unix(1800000120, 0) },
	})
	if err != nil {
		t.Fatalf("构造权威端 Web UI 失败：%v", err)
	}
	cookie := authoritySuccessfulLogin(t, handler, "127.0.0.1:7879", "correct-password")
	response := authorityGet(t, handler, "127.0.0.1:7879", "/ui/", cookie)
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "<h1>状态总览</h1>") {
		t.Fatalf("权威端主页应是状态总览，状态码=%d 页面：\n%s", response.StatusCode, body)
	}
	for _, want := range []string{
		`aria-label="一级导航"`,
		`href="/ui/"`,
		`href="/ui/servers/"`,
		`href="/ui/clients/"`,
		`href="/ui/authorization/"`,
		`href="/ui/ops/"`,
		"主页", "服务器", "客户端", "授权", "更多",
		`href="/ui/add"`,
		"1 个配对请求待审批",
		"macbook-air 请求接入",
		"2 台服务器需要注意",
		"2 个未识别进程待认领、1 台设备清单已过期",
		"3", "服务器", "1", "客户端", "2", "profile",
		"客户端 mini6-claude 完成同步",
		"机密项 HF_TOKEN 授权给 3090x2",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("权威端主页缺少 %q：\n%s", want, body)
		}
	}
	if strings.Contains(body, "缺备注") {
		t.Fatalf("这个替身数据来源没有实现缺备注查询，主页不得显示缺备注待办：\n%s", body)
	}
	for _, oldNavigation := range []string{
		`href="/ui/access/"`, `href="/ui/access/profiles">Profile`, `href="/ui/access/projects"`,
		`href="/ui/access/device-codes"`, `href="/ui/secrets/"`, `href="/ui/shares/"`,
		`href="/ui/software-inventory/"`, `href="/ui/topology/"`, `href="/ui/ops/audit"`,
	} {
		if strings.Contains(body, oldNavigation) {
			t.Fatalf("一级导航不得保留旧入口 %q：\n%s", oldNavigation, body)
		}
	}
}

func TestNavigationHidesUnavailableClientTabForStandaloneRole(t *testing.T) {
	handler, startup, err := New(Config{
		Role:    RoleStandalone,
		Servers: &stubServerSource{rows: []ServerRow{{Name: "web-1"}}},
		Access:  &stubAccessStore{},
		Ops:     &stubOpsStore{},
		Port:    18080,
	})
	if err != nil {
		t.Fatalf("构造单机 Web UI 失败：%v", err)
	}
	session := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookie := session.Cookies()[0]
	session.Body.Close()

	request := httptest.NewRequest(http.MethodGet, "/ui/", nil)
	request.Host = "127.0.0.1:18080"
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	body := recorder.Body.String()
	for _, want := range []string{"主页", "服务器", "授权", "更多"} {
		if !strings.Contains(body, want) {
			t.Fatalf("单机一级导航应保留 %q：\n%s", want, body)
		}
	}
	if strings.Contains(body, `href="/ui/access/pairing"`) || strings.Contains(body, ">客户端<") {
		t.Fatalf("单机没有客户端实例数据，应隐藏客户端标签：\n%s", body)
	}
}

func TestClientHomePageIsReadOnlyAndUsesClientAudit(t *testing.T) {
	handler, startup, err := New(Config{
		Role:    RoleClient,
		Servers: &stubServerSource{rows: []ServerRow{{ID: "srv-1", Name: "web-1"}}},
		Ops: &stubOpsStore{clientRows: []OpsAuditRow{
			{ID: 9, TS: time.Unix(1800000090, 0), Action: "client.sync", ServerName: "web-1"},
		}},
		Port: 18080,
	})
	if err != nil {
		t.Fatalf("构造客户端 Web UI 失败：%v", err)
	}
	session := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookie := session.Cookies()[0]
	session.Body.Close()

	request := httptest.NewRequest(http.MethodGet, "/ui/", nil)
	request.Host = "127.0.0.1:18080"
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	body := recorder.Body.String()
	for _, want := range []string{
		"<h1>状态总览</h1>",
		`href="/ui/ops/client"`,
		"client.sync · web-1",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("客户端主页应包含 %q：\n%s", want, body)
		}
	}
	if strings.Contains(body, "添加服务器") || strings.Contains(body, `class="owner-badge"`) {
		t.Fatalf("客户端主页是只读视图，不应出现添加按钮或管理员标记：\n%s", body)
	}
	if strings.Contains(body, "按已启用的设备码统计") || strings.Contains(body, "进入授权管理") {
		t.Fatalf("客户端看不到权威端的客户端实例与 profile 数据，不应显示这两个计数磁贴：\n%s", body)
	}
}

func TestServerListMovesToServersPathAndOldAttentionPathRedirects(t *testing.T) {
	handler, startup, err := New(Config{
		Role:    RoleStandalone,
		Servers: &stubServerSource{rows: []ServerRow{{ID: "srv-1", Name: "web-1"}, {ID: "srv-2", Name: "db-1", NeedsAttention: true}}},
		Access:  &stubAccessStore{},
		Port:    18080,
	})
	if err != nil {
		t.Fatalf("构造单机 Web UI 失败：%v", err)
	}
	session := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookie := session.Cookies()[0]
	session.Body.Close()

	newList := doGetWithCookie(t, handler, cookie, "/ui/servers/")
	newListBody := readResponseBody(t, newList)
	if newList.StatusCode != http.StatusOK || !strings.Contains(newListBody, "<h1>服务器</h1>") ||
		!strings.Contains(newListBody, "web-1") || !strings.Contains(newListBody, "db-1") {
		t.Fatalf("服务器页应在 /ui/servers/ 保持可用，状态码=%d 页面：\n%s", newList.StatusCode, newListBody)
	}
	if !strings.Contains(newListBody, `href="/ui/servers/" aria-current="page"`) {
		t.Fatalf("服务器页的一级导航应标记当前标签：\n%s", newListBody)
	}

	oldAttention := doGetWithCookie(t, handler, cookie, "/ui/?attention=1")
	oldAttention.Body.Close()
	if oldAttention.StatusCode != http.StatusSeeOther ||
		oldAttention.Header.Get("Location") != "/ui/servers/?attention=1" {
		t.Fatalf("旧服务器过滤地址应重定向到新服务器页，状态码=%d 跳转=%q",
			oldAttention.StatusCode, oldAttention.Header.Get("Location"))
	}
}
