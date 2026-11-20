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
