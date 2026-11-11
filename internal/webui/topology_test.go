package webui

// 网络拓扑页与内部只读 JSON 接口的处理器测试。断言只看外部行为：页面可
// 见内容、JSON 响应、会话与防跨站请求伪造要求，不关心页面脚本的内部函数。

import (
	"encoding/json"
	"fmt"
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

func newTopologyTestHandler(t *testing.T, role Role, source TopologySource, clientInstance string, writer ...TopologyWriter) (http.Handler, Startup) {
	t.Helper()
	var topologyWriter TopologyWriter
	if len(writer) > 0 {
		topologyWriter = writer[0]
	}
	if role == RoleServer {
		handler, _, err := NewAuthority(Config{
			Role: RoleServer, Servers: &stubServerSource{}, Access: &stubAccessStore{},
			Auth:     &fakeAuthorityAuth{passwordSet: true, password: "correct-password"},
			Topology: source, TopologyWriter: topologyWriter, AllowedHosts: []string{"127.0.0.1"}, Port: 7879,
		})
		if err != nil {
			t.Fatalf("构造权威端 Web UI 失败：%v", err)
		}
		return handler, Startup{}
	}
	handler, startup, err := New(Config{
		Role: role, Servers: &stubServerSource{}, Access: &stubAccessStore{},
		Topology: source, TopologyWriter: topologyWriter, ClientInstance: clientInstance,
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

// stubTopologyWriter 记录收到的写请求并返回固定成功提示，供处理器测试断
// 言接口行为；存储语义由 webuitopology 适配器测试覆盖。
type stubTopologyWriter struct {
	addCalls          [][3]string
	editCalls         [][3]string
	removeCalls       [][2]string
	moveCalls         [][4]string
	removePhysical    []string
	networkAddCalls   [][2]string
	nextNotFoundError bool
	nextPlainError    error
}

func (w *stubTopologyWriter) AddNetworkMembership(device, network, address string) (string, error) {
	w.addCalls = append(w.addCalls, [3]string{device, network, address})
	return "已把设备加入网络", w.nextError()
}

func (w *stubTopologyWriter) EditNetworkMembership(device, network, address string) (string, error) {
	w.editCalls = append(w.editCalls, [3]string{device, network, address})
	return "已修改网络成员地址", w.nextError()
}

func (w *stubTopologyWriter) RemoveNetworkMembership(device, network string) (string, error) {
	w.removeCalls = append(w.removeCalls, [2]string{device, network})
	return "已把设备移出网络", w.nextError()
}

func (w *stubTopologyWriter) MoveNetworkMembership(device, fromNetwork, toNetwork, address string) (string, error) {
	w.moveCalls = append(w.moveCalls, [4]string{device, fromNetwork, toNetwork, address})
	return "已移动网络成员", w.nextError()
}

func (w *stubTopologyWriter) RemovePhysicalNetworkMemberships(device string) (string, error) {
	w.removePhysical = append(w.removePhysical, device)
	return "已删除全部物理局域网成员", w.nextError()
}

func (w *stubTopologyWriter) AddPhysicalNetwork(name, site string) (string, error) {
	w.networkAddCalls = append(w.networkAddCalls, [2]string{name, site})
	return "已登记网络", w.nextError()
}

func (w *stubTopologyWriter) nextError() error {
	if w.nextNotFoundError {
		w.nextNotFoundError = false
		return &TopologyNameNotFoundError{Message: "设备不存在：没有这台设备"}
	}
	if w.nextPlainError != nil {
		err := w.nextPlainError
		w.nextPlainError = nil
		return err
	}
	return nil
}

func postTopologyJSON(t *testing.T, h http.Handler, cookie *http.Cookie, csrf, path, body string) *http.Response {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Host = "127.0.0.1:18080"
	request.Header.Set("Origin", "http://127.0.0.1:18080")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", csrf)
	if cookie != nil {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	return recorder.Result()
}

func TestTopologyWriteEndpointsDelegateToWriter(t *testing.T) {
	writer := &stubTopologyWriter{}
	uiHandler, startup := newTopologyTestHandler(t, RoleStandalone, &stubTopologySource{data: topologyFixture()}, "", writer)
	resp := exchangeStartupToken(t, uiHandler, startup.URL("127.0.0.1:18080"))
	cookies := resp.Cookies()
	resp.Body.Close()
	root := uiHandler.(*handler)
	root.mu.Lock()
	csrf := root.session.csrf
	root.mu.Unlock()

	cases := []struct {
		path     string
		body     string
		message  string
		validate func()
	}{
		{
			path: "/ui/api/topology/membership/add", body: `{"device":"笔记本","network":"家A局域网","address":"192.168.1.9"}`,
			message: "已把设备加入网络",
			validate: func() {
				if len(writer.addCalls) != 1 || writer.addCalls[0] != [3]string{"笔记本", "家A局域网", "192.168.1.9"} {
					t.Fatalf("新增网络成员参数错误：%+v", writer.addCalls)
				}
			},
		},
		{
			path: "/ui/api/topology/membership/edit", body: `{"device":"工作站","network":"家A局域网","address":"192.168.1.21"}`,
			message: "已修改网络成员地址",
			validate: func() {
				if len(writer.editCalls) != 1 || writer.editCalls[0] != [3]string{"工作站", "家A局域网", "192.168.1.21"} {
					t.Fatalf("修改网络成员地址参数错误：%+v", writer.editCalls)
				}
			},
		},
		{
			path: "/ui/api/topology/membership/remove", body: `{"device":"工作站","network":"虚拟网"}`,
			message: "已把设备移出网络",
			validate: func() {
				if len(writer.removeCalls) != 1 || writer.removeCalls[0] != [2]string{"工作站", "虚拟网"} {
					t.Fatalf("退出网络成员参数错误：%+v", writer.removeCalls)
				}
			},
		},
		{
			path: "/ui/api/topology/membership/move", body: `{"device":"工作站","from_network":"家A局域网","to_network":"机房局域网","address":"10.0.0.9"}`,
			message: "已移动网络成员",
			validate: func() {
				if len(writer.moveCalls) != 1 || writer.moveCalls[0] != [4]string{"工作站", "家A局域网", "机房局域网", "10.0.0.9"} {
					t.Fatalf("移动网络成员参数错误：%+v", writer.moveCalls)
				}
			},
		},
		{
			path: "/ui/api/topology/membership/remove-physical", body: `{"device":"工作站"}`,
			message: "已删除全部物理局域网成员",
			validate: func() {
				if len(writer.removePhysical) != 1 || writer.removePhysical[0] != "工作站" {
					t.Fatalf("删除全部物理局域网成员参数错误：%+v", writer.removePhysical)
				}
			},
		},
		{
			path: "/ui/api/topology/network/add", body: `{"name":"新局域网","site":"家A"}`,
			message: "已登记网络",
			validate: func() {
				if len(writer.networkAddCalls) != 1 || writer.networkAddCalls[0] != [2]string{"新局域网", "家A"} {
					t.Fatalf("新建物理局域网参数错误：%+v", writer.networkAddCalls)
				}
			},
		},
	}
	for _, tc := range cases {
		response := postTopologyJSON(t, uiHandler, cookies[0], csrf, tc.path, tc.body)
		payload, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s 状态码 = %d，响应：%s", tc.path, response.StatusCode, payload)
		}
		var got struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(payload, &got); err != nil {
			t.Fatalf("%s 响应不是合法 JSON：%v\n%s", tc.path, err, payload)
		}
		if got.Message != tc.message {
			t.Fatalf("%s 响应提示 = %q，应为 %q", tc.path, got.Message, tc.message)
		}
		tc.validate()
	}
}

func TestTopologyWriteEndpointErrors(t *testing.T) {
	cases := []struct {
		name     string
		writer   *stubTopologyWriter
		status   int
		message  string
		readonly bool
	}{
		{
			name:    "名称不存在返回404",
			writer:  &stubTopologyWriter{nextNotFoundError: true},
			status:  http.StatusNotFound,
			message: "设备不存在：没有这台设备",
		},
		{
			name:    "地址重复返回400",
			writer:  &stubTopologyWriter{nextPlainError: errTopologyTestDuplicate},
			status:  http.StatusBadRequest,
			message: "网络 家A局域网 的地址 192.168.1.20 已被设备 工作站 占用",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			uiHandler, startup := newTopologyTestHandler(t, RoleStandalone, &stubTopologySource{data: topologyFixture()}, "", tc.writer)
			resp := exchangeStartupToken(t, uiHandler, startup.URL("127.0.0.1:18080"))
			cookies := resp.Cookies()
			resp.Body.Close()
			root := uiHandler.(*handler)
			root.mu.Lock()
			csrf := root.session.csrf
			root.mu.Unlock()
			response := postTopologyJSON(t, uiHandler, cookies[0], csrf, "/ui/api/topology/membership/add",
				`{"device":"笔记本","network":"家A局域网","address":"192.168.1.20"}`)
			payload, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != tc.status {
				t.Fatalf("状态码 = %d，应为 %d，响应：%s", response.StatusCode, tc.status, payload)
			}
			var got struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(payload, &got); err != nil {
				t.Fatalf("错误响应不是合法 JSON：%v\n%s", err, payload)
			}
			if got.Error != tc.message {
				t.Fatalf("错误提示 = %q，应为 %q", got.Error, tc.message)
			}
		})
	}
}

var errTopologyTestDuplicate = fmt.Errorf("网络 家A局域网 的地址 192.168.1.20 已被设备 工作站 占用")

func TestTopologyWriteEndpointsRejectMissingFieldsAndBadJSON(t *testing.T) {
	writer := &stubTopologyWriter{}
	uiHandler, startup := newTopologyTestHandler(t, RoleStandalone, &stubTopologySource{data: topologyFixture()}, "", writer)
	resp := exchangeStartupToken(t, uiHandler, startup.URL("127.0.0.1:18080"))
	cookies := resp.Cookies()
	resp.Body.Close()
	root := uiHandler.(*handler)
	root.mu.Lock()
	csrf := root.session.csrf
	root.mu.Unlock()

	missing := postTopologyJSON(t, uiHandler, cookies[0], csrf, "/ui/api/topology/membership/add", `{"device":"笔记本"}`)
	missingBody, _ := io.ReadAll(missing.Body)
	missing.Body.Close()
	if missing.StatusCode != http.StatusBadRequest || !strings.Contains(string(missingBody), "网络") {
		t.Fatalf("缺少字段状态码 = %d，响应：%s", missing.StatusCode, missingBody)
	}
	broken := postTopologyJSON(t, uiHandler, cookies[0], csrf, "/ui/api/topology/membership/add", `{not-json`)
	brokenBody, _ := io.ReadAll(broken.Body)
	broken.Body.Close()
	if broken.StatusCode != http.StatusBadRequest || !strings.Contains(string(brokenBody), "请求内容") {
		t.Fatalf("非法 JSON 状态码 = %d，响应：%s", broken.StatusCode, brokenBody)
	}
	if len(writer.addCalls) != 0 {
		t.Fatalf("校验失败不应调用写来源：%+v", writer.addCalls)
	}
}

func TestTopologyWriteRoutesOnlyExistForWritableRole(t *testing.T) {
	// 客户端不注入写来源：即便携带有效会话与令牌，写路径也不存在。
	client, startup := newTopologyTestHandler(t, RoleClient, &stubTopologySource{data: &TopologyData{}}, "")
	resp := exchangeStartupToken(t, client, startup.URL("127.0.0.1:18080"))
	cookies := resp.Cookies()
	resp.Body.Close()
	clientRoot := client.(*handler)
	clientRoot.mu.Lock()
	csrf := clientRoot.session.csrf
	clientRoot.mu.Unlock()
	response := postTopologyJSON(t, client, cookies[0], csrf, "/ui/api/topology/membership/add",
		`{"device":"工作站","network":"家A局域网","address":"192.168.1.20"}`)
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("客户端写接口状态码 = %d，应为 404", response.StatusCode)
	}

	// 单机注入写来源后，缺少防跨站请求伪造令牌的写请求在进入处理器前被拒。
	standalone, standaloneStartup := newTopologyTestHandler(t, RoleStandalone, &stubTopologySource{data: &TopologyData{}}, "", &stubTopologyWriter{})
	session := exchangeStartupToken(t, standalone, standaloneStartup.URL("127.0.0.1:18080"))
	standaloneCookies := session.Cookies()
	session.Body.Close()
	noCSRF := postTopologyJSON(t, standalone, standaloneCookies[0], "wrong-token",
		"/ui/api/topology/membership/add", `{"device":"工作站","network":"家A局域网","address":"192.168.1.20"}`)
	noCSRF.Body.Close()
	if noCSRF.StatusCode != http.StatusForbidden {
		t.Fatalf("缺少令牌的写请求状态码 = %d，应为 403", noCSRF.StatusCode)
	}

	anonymous := postTopologyJSON(t, standalone, nil, "", "/ui/api/topology/membership/add", `{}`)
	anonymous.Body.Close()
	if anonymous.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无会话写请求状态码 = %d，应为 401", anonymous.StatusCode)
	}
}

func TestTopologyPageEditableFlagFollowsWriterInjection(t *testing.T) {
	cases := []struct {
		name     string
		role     Role
		writer   TopologyWriter
		editable bool
	}{
		{name: "单机注入写来源后可编辑", role: RoleStandalone, writer: &stubTopologyWriter{}, editable: true},
		{name: "权威端注入写来源后可编辑", role: RoleServer, writer: &stubTopologyWriter{}, editable: true},
		{name: "客户端不可编辑", role: RoleClient},
		{name: "没有写来源的构造不可编辑", role: RoleStandalone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var writer TopologyWriter
			if tc.writer != nil {
				writer = tc.writer
			}
			uiHandler, startup := newTopologyTestHandler(t, tc.role, &stubTopologySource{data: topologyFixture()}, "", writer)
			var cookie *http.Cookie
			host := "127.0.0.1:18080"
			if tc.role == RoleServer {
				cookie = authoritySuccessfulLogin(t, uiHandler, "127.0.0.1:7879", "correct-password")
				host = "127.0.0.1:7879"
			} else {
				resp := exchangeStartupToken(t, uiHandler, startup.URL(host))
				cookies := resp.Cookies()
				resp.Body.Close()
				cookie = cookies[0]
			}
			page := authorityGet(t, uiHandler, host, "/ui/topology/", cookie)
			body, _ := io.ReadAll(page.Body)
			page.Body.Close()
			content := string(body)
			if tc.editable && !strings.Contains(content, `data-editable="true"`) {
				t.Fatalf("可编辑角色的拓扑页应带可编辑标记：\n%s", content)
			}
			if !tc.editable && strings.Contains(content, "data-editable") {
				t.Fatalf("只读角色的拓扑页不得出现可编辑标记：\n%s", content)
			}
		})
	}
}
