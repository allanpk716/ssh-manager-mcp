package webui

// 网络拓扑页与内部只读 JSON 接口的处理器测试。断言只看外部行为：页面可
// 见内容、JSON 响应、会话与防跨站请求伪造要求，不关心页面脚本的内部函数。

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubTopologySource 是测试用的拓扑来源，记录收到的客户端实例名并返回固
// 定数据。
type stubTopologySource struct {
	instance string
	data     *TopologyData
	err      error
}

func (s *stubTopologySource) Topology(instance string) (*TopologyData, error) {
	s.instance = instance
	return s.data, s.err
}

func topologyFixture() *TopologyData {
	return &TopologyData{
		Sites: []TopologySite{{ID: "site-1", Name: "家A", Note: "物理场所"}},
		Networks: []TopologyNetwork{
			{ID: "net-1", Name: "家A局域网", Type: "physical", SiteID: "site-1", SiteName: "家A",
				NetworkRange: "192.168.1.0/24", BandwidthMbps: 1000, Metered: true, Note: "家庭网络"},
			{ID: "net-2", Name: "虚拟网", Type: "virtual", Note: "跨站点"},
		},
		Devices: []TopologyDevice{
			{ID: "device-1", Name: "工作站", Note: "主力机器", Kind: "server", CandidateNotice: "",
				Servers: []TopologyDeviceServer{
					{ID: "server-1", Name: "web-1", User: "owner", Port: 22, Pinned: true},
				},
				SharedStorages: []TopologySharedStorage{{ID: "share-1", Name: "家NAS共享"}},
				Candidates: []TopologyCandidate{
					{Address: "192.168.1.20", NetworkName: "家A局域网", Source: "physical"},
					{Address: "192.0.2.10", Source: "fallback", ServerName: "web-1"},
				}},
			{ID: "device-2", Name: "NAS", Kind: "unmanaged"},
		},
		Memberships: []TopologyMembership{
			{DeviceID: "device-1", NetworkID: "net-1", Address: "192.168.1.20", RegistrationOrder: 1},
		},
		LayoutCoordinates: []TopologyLayoutCoordinate{{ViewKey: "physical", DeviceID: "device-1", X: 12.5, Y: -3}},
		LocalDevice:       TopologyLocalDevice{Known: true, DeviceID: "device-1", DeviceName: "工作站"},
	}
}

func newTopologyTestHandler(t *testing.T, role Role, source TopologySource, clientInstance string) (http.Handler, Startup) {
	t.Helper()
	if role == RoleServer {
		handler, _, err := NewAuthority(Config{
			Role: RoleServer, Servers: &stubServerSource{}, Access: &stubAccessStore{},
			Auth:     &fakeAuthorityAuth{passwordSet: true, password: "correct-password"},
			Topology: source, AllowedHosts: []string{"127.0.0.1"}, Port: 7879,
		})
		if err != nil {
			t.Fatalf("构造权威端 Web UI 失败：%v", err)
		}
		return handler, Startup{}
	}
	handler, startup, err := New(Config{
		Role: role, Servers: &stubServerSource{}, Access: &stubAccessStore{},
		Topology: source, ClientInstance: clientInstance,
		VersionHint: func() string { return "" }, Port: 18080,
	})
	if err != nil {
		t.Fatalf("构造 Web UI 失败：%v", err)
	}
	return handler, startup
}

// newTopologyWritableTestHandler 在可写角色上额外注入布局写来源。
func newTopologyWritableTestHandler(t *testing.T, role Role, source TopologySource, writer TopologyWriter) (http.Handler, Startup) {
	t.Helper()
	if role == RoleServer {
		handler, _, err := NewAuthority(Config{
			Role: RoleServer, Servers: &stubServerSource{}, Access: &stubAccessStore{},
			Auth:           &fakeAuthorityAuth{passwordSet: true, password: "correct-password"},
			Topology:       source,
			TopologyWriter: writer,
			AllowedHosts:   []string{"127.0.0.1"},
			Port:           7879,
		})
		if err != nil {
			t.Fatalf("构造权威端 Web UI 失败：%v", err)
		}
		return handler, Startup{}
	}
	handler, startup, err := New(Config{
		Role: role, Servers: &stubServerSource{}, Access: &stubAccessStore{},
		Topology: source, TopologyWriter: writer,
		VersionHint: func() string { return "" }, Port: 18080,
	})
	if err != nil {
		t.Fatalf("构造 Web UI 失败：%v", err)
	}
	return handler, startup
}

func TestTopologyNavigationAndPageAvailableForEveryRole(t *testing.T) {
	cases := []struct {
		name      string
		role      Role
		authority bool
	}{
		{name: "standalone", role: RoleStandalone},
		{name: "client", role: RoleClient},
		{name: "server", role: RoleServer, authority: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler, startup := newTopologyTestHandler(t, tc.role, &stubTopologySource{data: topologyFixture()}, "")
			var cookie *http.Cookie
			if tc.authority {
				cookie = authoritySuccessfulLogin(t, handler, "127.0.0.1:7879", "correct-password")
			} else {
				resp := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
				cookies := resp.Cookies()
				resp.Body.Close()
				if len(cookies) != 1 {
					t.Fatalf("应签发会话 cookie，实际 %d", len(cookies))
				}
				cookie = cookies[0]
			}
			host := "127.0.0.1:18080"
			if tc.authority {
				host = "127.0.0.1:7879"
			}

			home := authorityGet(t, handler, host, "/ui/", cookie)
			homeBody, _ := io.ReadAll(home.Body)
			home.Body.Close()
			if !strings.Contains(string(homeBody), `<a href="/ui/topology/">网络拓扑</a>`) {
				t.Fatalf("首页导航应出现网络拓扑入口：\n%s", homeBody)
			}

			page := authorityGet(t, handler, host, "/ui/topology/", cookie)
			body, _ := io.ReadAll(page.Body)
			page.Body.Close()
			if page.StatusCode != http.StatusOK {
				t.Fatalf("拓扑页状态码 = %d，应为 200", page.StatusCode)
			}
			content := string(body)
			for _, want := range []string{
				"网络拓扑", `id="topology-canvas"`, "物理视图", "虚拟视图", "叠加视图", "未归类设备",
			} {
				if !strings.Contains(content, want) {
					t.Fatalf("拓扑页应包含 %q：\n%s", want, content)
				}
			}
			if tc.role == RoleClient && !strings.Contains(content, "修改需到权威端") {
				t.Fatalf("客户端拓扑页应提示修改到权威端：\n%s", content)
			}
			for _, banned := range []string{" style=", " onclick=", " hx-on"} {
				if strings.Contains(content, banned) {
					t.Fatalf("拓扑页不得包含内联样式或事件属性 %q", banned)
				}
			}
		})
	}
}

func TestTopologyWithoutSourceHidesNavigationAndRoutes(t *testing.T) {
	handler, startup := newTestHandler(t, RoleStandalone, nil)
	resp := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookies := resp.Cookies()
	resp.Body.Close()
	if len(cookies) != 1 {
		t.Fatalf("应签发会话 cookie，实际 %d", len(cookies))
	}
	page := authorityGet(t, handler, "127.0.0.1:18080", "/ui/", cookies[0])
	body, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if strings.Contains(string(body), "网络拓扑") {
		t.Fatalf("没有拓扑来源时导航不应出现网络拓扑入口：\n%s", body)
	}
	missing := authorityGet(t, handler, "127.0.0.1:18080", "/ui/topology/", cookies[0])
	missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("没有拓扑来源时拓扑页状态码 = %d，应为 404", missing.StatusCode)
	}
}

func TestTopologyDataEndpointReturnsSourceJSON(t *testing.T) {
	source := &stubTopologySource{data: topologyFixture()}
	handler, startup := newTopologyTestHandler(t, RoleStandalone, source, "")
	resp := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookies := resp.Cookies()
	resp.Body.Close()

	data := authorityGet(t, handler, "127.0.0.1:18080", "/ui/api/topology", cookies[0])
	body, err := io.ReadAll(data.Body)
	data.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if data.StatusCode != http.StatusOK {
		t.Fatalf("拓扑数据接口状态码 = %d，响应：%s", data.StatusCode, body)
	}
	if got := data.Header.Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("拓扑数据接口内容类型 = %q，应为 JSON", got)
	}
	var got TopologyData
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("拓扑数据不是合法 JSON：%v\n%s", err, body)
	}
	if len(got.Sites) != 1 || got.Sites[0].Name != "家A" || got.Sites[0].Note != "物理场所" {
		t.Fatalf("站点数据错误：%+v", got.Sites)
	}
	if len(got.Networks) != 2 {
		t.Fatalf("网络数量 = %d，应为 2：%+v", len(got.Networks), got.Networks)
	}
	if got.Networks[0].Type != "physical" || got.Networks[0].SiteName != "家A" ||
		got.Networks[0].NetworkRange != "192.168.1.0/24" || got.Networks[0].BandwidthMbps != 1000 || !got.Networks[0].Metered {
		t.Fatalf("物理局域网字段错误：%+v", got.Networks[0])
	}
	if got.Networks[1].Type != "virtual" || got.Networks[1].SiteName != "" {
		t.Fatalf("虚拟局域网字段错误：%+v", got.Networks[1])
	}
	if len(got.Devices) != 2 || got.Devices[0].Name != "工作站" || got.Devices[0].Kind != "server" {
		t.Fatalf("设备数据错误：%+v", got.Devices)
	}
	device := got.Devices[0]
	if len(device.Servers) != 1 || device.Servers[0].Name != "web-1" || !device.Servers[0].Pinned {
		t.Fatalf("设备服务器摘要错误：%+v", device.Servers)
	}
	if len(device.SharedStorages) != 1 || device.SharedStorages[0].Name != "家NAS共享" {
		t.Fatalf("设备共享存储错误：%+v", device.SharedStorages)
	}
	if len(device.Candidates) != 2 || device.Candidates[0].Source != "physical" || device.Candidates[1].Source != "fallback" {
		t.Fatalf("候选地址错误：%+v", device.Candidates)
	}
	if len(got.Memberships) != 1 || got.Memberships[0].RegistrationOrder != 1 {
		t.Fatalf("网络成员数据错误：%+v", got.Memberships)
	}
	if len(got.LayoutCoordinates) != 1 || got.LayoutCoordinates[0].ViewKey != "physical" || got.LayoutCoordinates[0].X != 12.5 {
		t.Fatalf("布局坐标数据错误：%+v", got.LayoutCoordinates)
	}
	if !got.LocalDevice.Known || got.LocalDevice.DeviceName != "工作站" {
		t.Fatalf("本机设备数据错误：%+v", got.LocalDevice)
	}
}

func TestAuthorityTopologyDataEndpointRequiresLoginThenReturnsJSON(t *testing.T) {
	source := &stubTopologySource{data: topologyFixture()}
	handler, _ := newTopologyTestHandler(t, RoleServer, source, "")
	cookie := authoritySuccessfulLogin(t, handler, "127.0.0.1:7879", "correct-password")

	resp := authorityGet(t, handler, "127.0.0.1:7879", "/ui/api/topology", cookie)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("权威端拓扑数据接口状态码 = %d，响应：%s", resp.StatusCode, body)
	}
	var data TopologyData
	if err := json.Unmarshal(body, &data); err != nil {
		t.Fatalf("权威端拓扑数据不是合法 JSON：%v\n%s", err, body)
	}
	if len(data.Devices) != 2 || data.Devices[0].Name != "工作站" {
		t.Fatalf("权威端拓扑设备数据错误：%+v", data.Devices)
	}
}

func TestTopologyDataEndpointUsesSessionClientInstance(t *testing.T) {
	source := &stubTopologySource{data: &TopologyData{}}
	handler, startup := newTopologyTestHandler(t, RoleClient, source, "lab")
	resp := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookies := resp.Cookies()
	resp.Body.Close()

	data := authorityGet(t, handler, "127.0.0.1:18080", "/ui/api/topology", cookies[0])
	data.Body.Close()
	if data.StatusCode != http.StatusOK {
		t.Fatalf("拓扑数据接口状态码 = %d，应为 200", data.StatusCode)
	}
	if source.instance != "lab" {
		t.Fatalf("拓扑数据接口应沿用会话客户端实例，实际 %q", source.instance)
	}
}

func TestTopologyEndpointsRequireSession(t *testing.T) {
	loopback, _ := newTopologyTestHandler(t, RoleStandalone, &stubTopologySource{data: &TopologyData{}}, "")
	resp := doGet(t, loopback, "127.0.0.1:18080", "/ui/api/topology")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无会话访问拓扑数据状态码 = %d，应为 401", resp.StatusCode)
	}

	authorityHandler, _ := newTopologyTestHandler(t, RoleServer, &stubTopologySource{data: &TopologyData{}}, "")
	redirect := authorityGet(t, authorityHandler, "127.0.0.1:7879", "/ui/api/topology", nil)
	redirect.Body.Close()
	if redirect.StatusCode != http.StatusSeeOther || redirect.Header.Get("Location") != loginPath {
		t.Fatalf("无会话访问权威端拓扑数据应跳转登录，状态码=%d 跳转=%q",
			redirect.StatusCode, redirect.Header.Get("Location"))
	}
}

func TestTopologyRoutesAreReadOnly(t *testing.T) {
	for _, role := range []Role{RoleStandalone, RoleClient} {
		uiHandler, startup := newTopologyTestHandler(t, role, &stubTopologySource{data: &TopologyData{}}, "")
		resp := exchangeStartupToken(t, uiHandler, startup.URL("127.0.0.1:18080"))
		cookies := resp.Cookies()
		resp.Body.Close()

		noCSRF := httptest.NewRequest(http.MethodPost, "/ui/api/topology", strings.NewReader(""))
		noCSRF.Host = "127.0.0.1:18080"
		noCSRF.Header.Set("Origin", "http://127.0.0.1:18080")
		noCSRF.AddCookie(cookies[0])
		recorder := httptest.NewRecorder()
		uiHandler.ServeHTTP(recorder, noCSRF)
		rejected := recorder.Result()
		rejected.Body.Close()
		if rejected.StatusCode != http.StatusForbidden {
			t.Fatalf("角色 %d 写请求缺防跨站请求伪造令牌状态码 = %d，应为 403", role, rejected.StatusCode)
		}

		root := uiHandler.(*handler)
		root.mu.Lock()
		csrf := root.session.csrf
		root.mu.Unlock()
		withCSRF := httptest.NewRequest(http.MethodPost, "/ui/api/topology", strings.NewReader(""))
		withCSRF.Host = "127.0.0.1:18080"
		withCSRF.Header.Set("Origin", "http://127.0.0.1:18080")
		withCSRF.Header.Set("X-CSRF-Token", csrf)
		withCSRF.AddCookie(cookies[0])
		recorder = httptest.NewRecorder()
		uiHandler.ServeHTTP(recorder, withCSRF)
		methodRejected := recorder.Result()
		methodRejected.Body.Close()
		if methodRejected.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("角色 %d 拓扑接口写请求状态码 = %d，应为 405", role, methodRejected.StatusCode)
		}
	}
}

// stubTopologyWriter 记录布局写请求，供处理器测试断言接口行为。
type stubTopologyWriter struct {
	saveView   string
	saveLayout TopologyLayoutSave
	saveSource TopologyWriteSource
	saveErr    error
	saveCalls  int

	resetView   string
	resetSource TopologyWriteSource
	resetErr    error
	resetCalls  int
}

func (w *stubTopologyWriter) SaveTopologyLayout(save TopologyLayoutSave, source TopologyWriteSource) error {
	w.saveCalls++
	w.saveView, w.saveLayout, w.saveSource = save.ViewKey, save, source
	return w.saveErr
}

func (w *stubTopologyWriter) ResetTopologyLayout(view string, source TopologyWriteSource) error {
	w.resetCalls++
	w.resetView, w.resetSource = view, source
	return w.resetErr
}

// postTopologyJSON 发起一次带会话、同源来源与防跨站请求伪造令牌的 JSON 写
// 请求，返回响应与处理器内部的令牌。
func postTopologyJSON(t *testing.T, handler http.Handler, host, path, csrfToken string, cookie *http.Cookie, body string) *http.Response {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Host = host
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://"+host)
	if csrfToken != "" {
		request.Header.Set("X-CSRF-Token", csrfToken)
	}
	if cookie != nil {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder.Result()
}

func sessionCSRFToken(t *testing.T, uiHandler http.Handler) string {
	t.Helper()
	root := uiHandler.(*handler)
	root.mu.Lock()
	defer root.mu.Unlock()
	if root.session == nil {
		t.Fatal("回环处理器没有会话")
	}
	return root.session.csrf
}

func TestTopologyLayoutSaveEndpointValidatesAndCallsWriter(t *testing.T) {
	writer := &stubTopologyWriter{}
	handler, startup := newTopologyWritableTestHandler(t, RoleStandalone, &stubTopologySource{data: topologyFixture()}, writer)
	resp := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookies := resp.Cookies()
	resp.Body.Close()
	csrf := sessionCSRFToken(t, handler)

	body := `{"view":"physical","devices":[{"device_id":"device-1","x":12.5,"y":-3}],"containers":[{"entity_type":"site","entity_id":"site-1","x":20,"y":30}]}`
	saved := postTopologyJSON(t, handler, "127.0.0.1:18080", topologyLayoutPath, csrf, cookies[0], body)
	saved.Body.Close()
	if saved.StatusCode != http.StatusNoContent {
		t.Fatalf("布局保存状态码 = %d，应为 204", saved.StatusCode)
	}
	if writer.saveCalls != 1 || writer.saveView != "physical" {
		t.Fatalf("保存请求未到达写来源：%+v", writer)
	}
	if len(writer.saveLayout.Devices) != 1 || writer.saveLayout.Devices[0].DeviceID != "device-1" ||
		writer.saveLayout.Devices[0].X != 12.5 || writer.saveLayout.Devices[0].Y != -3 {
		t.Fatalf("保存的设备坐标错误：%+v", writer.saveLayout.Devices)
	}
	if len(writer.saveLayout.Containers) != 1 || writer.saveLayout.Containers[0].EntityType != "site" ||
		writer.saveLayout.Containers[0].EntityID != "site-1" ||
		writer.saveLayout.Containers[0].X != 20 || writer.saveLayout.Containers[0].Y != 30 {
		t.Fatalf("保存的容器坐标错误：%+v", writer.saveLayout.Containers)
	}
	if writer.saveSource.RemoteAddress != "192.0.2.1" {
		t.Fatalf("保存请求来源地址错误：%+v", writer.saveSource)
	}

	cases := []struct {
		name   string
		path   string
		body   string
		status int
	}{
		{name: "未知视图", path: topologyLayoutPath, body: `{"view":"主视图"}`, status: http.StatusUnprocessableEntity},
		{name: "非法实体类型", path: topologyLayoutPath,
			body:   `{"view":"physical","containers":[{"entity_type":"device","entity_id":"device-1","x":1,"y":2}]}`,
			status: http.StatusUnprocessableEntity},
		{name: "设备缺横坐标", path: topologyLayoutPath,
			body: `{"view":"physical","devices":[{"device_id":"device-1","y":2}]}`, status: http.StatusBadRequest},
		{name: "JSON 不合法", path: topologyLayoutPath, body: `{`, status: http.StatusBadRequest},
		{name: "重置未知视图", path: topologyLayoutResetPath, body: `{"view":"主视图"}`, status: http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := postTopologyJSON(t, handler, "127.0.0.1:18080", tc.path, csrf, cookies[0], tc.body)
			defer response.Body.Close()
			if response.StatusCode != tc.status {
				t.Fatalf("状态码 = %d，应为 %d", response.StatusCode, tc.status)
			}
			var payload map[string]string
			if err := json.NewDecoder(response.Body).Decode(&payload); err != nil || payload["error"] == "" {
				t.Fatalf("应返回带中文错误的 JSON：%v %v", err, payload)
			}
		})
	}
}

func TestTopologyLayoutResetEndpointCallsWriter(t *testing.T) {
	writer := &stubTopologyWriter{}
	handler, startup := newTopologyWritableTestHandler(t, RoleStandalone, &stubTopologySource{data: topologyFixture()}, writer)
	resp := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookies := resp.Cookies()
	resp.Body.Close()
	csrf := sessionCSRFToken(t, handler)

	response := postTopologyJSON(t, handler, "127.0.0.1:18080", topologyLayoutResetPath, csrf, cookies[0], `{"view":"virtual"}`)
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("布局重置状态码 = %d，应为 204", response.StatusCode)
	}
	if writer.resetCalls != 1 || writer.resetView != "virtual" || writer.resetSource.RemoteAddress != "192.0.2.1" {
		t.Fatalf("重置请求未到达写来源：%+v", writer)
	}
}

func TestTopologyLayoutWriteRequiresCSRFAndWritableRole(t *testing.T) {
	// 客户端角色不注册布局写路由。
	clientHandler, clientStartup := newTopologyTestHandler(t, RoleClient, &stubTopologySource{data: &TopologyData{}}, "")
	resp := exchangeStartupToken(t, clientHandler, clientStartup.URL("127.0.0.1:18080"))
	cookies := resp.Cookies()
	resp.Body.Close()
	clientCSRF := sessionCSRFToken(t, clientHandler)
	for _, path := range []string{topologyLayoutPath, topologyLayoutResetPath} {
		notFound := postTopologyJSON(t, clientHandler, "127.0.0.1:18080", path, clientCSRF, cookies[0], `{"view":"physical"}`)
		notFound.Body.Close()
		if notFound.StatusCode != http.StatusNotFound {
			t.Fatalf("客户端布局写路由 %s 状态码 = %d，应为 404", path, notFound.StatusCode)
		}
	}

	// 可写角色缺防跨站请求伪造令牌时拒绝。
	writeHandler, startup := newTopologyTestHandler(t, RoleStandalone, &stubTopologySource{data: &TopologyData{}}, "")
	resp = exchangeStartupToken(t, writeHandler, startup.URL("127.0.0.1:18080"))
	cookies = resp.Cookies()
	resp.Body.Close()
	forbidden := postTopologyJSON(t, writeHandler, "127.0.0.1:18080", topologyLayoutPath, "", cookies[0], `{"view":"physical"}`)
	forbidden.Body.Close()
	if forbidden.StatusCode != http.StatusForbidden {
		t.Fatalf("缺防跨站请求伪造令牌的布局保存状态码 = %d，应为 403", forbidden.StatusCode)
	}
}

func TestTopologyLayoutWriterErrorsMapToClientErrors(t *testing.T) {
	writer := &stubTopologyWriter{saveErr: ErrTopologyLayoutInvalid}
	handler, startup := newTopologyWritableTestHandler(t, RoleStandalone, &stubTopologySource{data: &TopologyData{}}, writer)
	resp := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookies := resp.Cookies()
	resp.Body.Close()
	csrf := sessionCSRFToken(t, handler)

	rejected := postTopologyJSON(t, handler, "127.0.0.1:18080", topologyLayoutPath, csrf, cookies[0], `{"view":"physical","devices":[{"device_id":"missing","x":1,"y":2}]}`)
	defer rejected.Body.Close()
	if rejected.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("内容非法的保存状态码 = %d，应为 422", rejected.StatusCode)
	}

	writer.saveErr = errors.New("数据库不可用")
	failed := postTopologyJSON(t, handler, "127.0.0.1:18080", topologyLayoutPath, csrf, cookies[0], `{"view":"physical"}`)
	failed.Body.Close()
	if failed.StatusCode != http.StatusInternalServerError {
		t.Fatalf("写来源失败的状态码 = %d，应为 500", failed.StatusCode)
	}
}
