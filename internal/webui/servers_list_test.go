package webui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
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
