package webui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func locationsNetworksFixture() *TopologyData {
	return &TopologyData{
		Sites: []TopologySite{
			{ID: "site-1", Name: "公司", LocationPath: "公司"},
			{ID: "site-2", ParentID: "site-1", Name: "一厂区", LocationPath: "公司 / 一厂区"},
			{ID: "site-3", ParentID: "site-2", Name: "3 号楼", LocationPath: "公司 / 一厂区 / 3 号楼"},
			{ID: "site-4", Name: "机房", LocationPath: "机房"},
		},
		Networks: []TopologyNetwork{
			{ID: "net-1", Name: "办公网", Type: "physical", SiteID: "site-3", SiteName: "3 号楼",
				LocationPath: "公司 / 一厂区 / 3 号楼", NetworkRange: "192.0.2.0/24", BandwidthMbps: 1000, Metered: true},
			{ID: "net-2", Name: "机房网", Type: "physical", SiteID: "site-4", SiteName: "机房", LocationPath: "机房"},
			{ID: "net-3", Name: "虚拟网", Type: "virtual", NetworkRange: "100.64.0.0/24"},
		},
		Devices: []TopologyDevice{
			{ID: "device-1", Name: "工作站", Kind: "server", SiteID: "site-3", LocationPath: "公司 / 一厂区 / 3 号楼"},
			{ID: "device-2", Name: "NAS", Kind: "unmanaged", SiteID: "site-4", LocationPath: "机房"},
		},
		Memberships: []TopologyMembership{
			{DeviceID: "device-1", NetworkID: "net-1", Address: "192.0.2.10"},
			{DeviceID: "device-2", NetworkID: "net-2", Address: "192.0.2.30"},
		},
	}
}

func newLocationsNetworksHandler(
	t *testing.T, role Role, source TopologySource, writer TopologyWriter,
) (http.Handler, *http.Cookie) {
	t.Helper()
	if role == RoleServer {
		handler, _, err := NewAuthority(Config{
			Role: RoleServer, Servers: &stubServerSource{}, Access: &stubAccessStore{},
			Topology: source, TopologyWriter: writer, Ops: &stubOpsStore{},
			Auth:         &fakeAuthorityAuth{passwordSet: true, password: "correct-password"},
			AllowedHosts: []string{"127.0.0.1"}, Port: 7879,
		})
		if err != nil {
			t.Fatalf("构造权威端 Web UI 失败：%v", err)
		}
		cookie := authoritySuccessfulLogin(t, handler, "127.0.0.1:7879", "correct-password")
		return handler, cookie
	}
	handler, startup, err := New(Config{
		Role: role, Servers: &stubServerSource{}, Access: &stubAccessStore{},
		Topology: source, TopologyWriter: writer, Ops: &stubOpsStore{},
		VersionHint: func() string { return "" }, Port: 18080,
	})
	if err != nil {
		t.Fatalf("构造 Web UI Handler 失败：%v", err)
	}
	session := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookie := session.Cookies()[0]
	session.Body.Close()
	return handler, cookie
}

func locationsNetworksGet(t *testing.T, h http.Handler, cookie *http.Cookie, target string) string {
	t.Helper()
	response := doGetWithCookie(t, h, cookie, target)
	return readResponseBody(t, response)
}

func TestLocationsNetworksPageShowsTreeNetworksAndRefusalReasons(t *testing.T) {
	writer := &stubTopologyWriter{}
	handler, cookie := newLocationsNetworksHandler(t, RoleStandalone,
		&stubTopologySource{data: locationsNetworksFixture()}, writer)

	body := locationsNetworksGet(t, handler, cookie, "/ui/more/locations-networks/")
	for _, want := range []string{
		"<h1>地点与网络</h1>", "地点", "网络",
		"公司", "一厂区", "3 号楼", "公司 / 一厂区 / 3 号楼",
		"1 台机器 · 1 个物理网", "下级共 1 台机器", "工作站",
		"登记不跑 SSH 的机器", `/ui/devices/new`,
		"新建子地点", "改名", "移动到其他上级", "删除",
		"还有下级地点：一厂区；请先删除或改上级",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("地点与网络页缺少 %q：\n%s", want, body)
		}
	}
	networksBody := locationsNetworksGet(t, handler, cookie, "/ui/more/locations-networks/?tab=networks")
	for _, want := range []string{
		"物理局域网", "虚拟局域网", "办公网", "192.0.2.0/24", "1000 Mbps", "按流量计费", "覆盖地点",
	} {
		if !strings.Contains(networksBody, want) {
			t.Fatalf("地点与网络页的网络分组缺少 %q：\n%s", want, networksBody)
		}
	}
	for _, forbidden := range []string{"设备码", "项目令牌", "站点", "网络成员", "登录条目", "兜底主机", "cache token"} {
		if strings.Contains(body, forbidden) || strings.Contains(networksBody, forbidden) {
			t.Fatalf("地点与网络页不应出现 %q：\n%s\n%s", forbidden, body, networksBody)
		}
	}
}

func TestLocationsNetworksPageReadOnlyWithoutWriter(t *testing.T) {
	handler, cookie := newLocationsNetworksHandler(t, RoleClient,
		&stubTopologySource{data: locationsNetworksFixture()}, nil)

	body := locationsNetworksGet(t, handler, cookie, "/ui/more/locations-networks/")
	for _, want := range []string{"公司 / 一厂区 / 3 号楼", "办公网", "只读"} {
		if !strings.Contains(body, want) {
			t.Fatalf("客户端地点与网络页应显示 %q：\n%s", want, body)
		}
	}
	for _, hidden := range []string{"新建子地点", "/locations/rename", "移动到其他上级", "/locations/delete", "登记不跑 SSH 的机器"} {
		if strings.Contains(body, hidden) {
			t.Fatalf("客户端地点与网络页不应显示写入口 %q：\n%s", hidden, body)
		}
	}
	clientOpsPage := locationsNetworksGet(t, handler, cookie, "/ui/ops/client")
	match := regexp.MustCompile(`<input type="hidden" name="csrf_token" value="([^"]+)"`).FindStringSubmatch(clientOpsPage)
	if match == nil {
		t.Fatalf("客户端运维页缺少防跨站请求伪造令牌：\n%s", clientOpsPage)
	}
	form := url.Values{"name": {"新地点"}, "parent": {""}, "csrf_token": {match[1]}}
	req := httptest.NewRequest(http.MethodPost, "/ui/more/locations/new", strings.NewReader(form.Encode()))
	req.Host = "127.0.0.1:18080"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:18080")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	write := rec.Result()
	write.Body.Close()
	if write.StatusCode != http.StatusNotFound {
		t.Fatalf("客户端没有写来源时地点新建路由应不存在，状态码=%d", write.StatusCode)
	}
}

func TestLocationCreateRenameMoveAndDeleteHandlers(t *testing.T) {
	writer := &stubTopologyWriter{}
	handler, cookie := newLocationsNetworksHandler(t, RoleStandalone,
		&stubTopologySource{data: locationsNetworksFixture()}, writer)

	form := locationsNetworksGet(t, handler, cookie, "/ui/more/locations/new?parent="+url.QueryEscape("公司 / 一厂区"))
	if !strings.Contains(form, `value="公司 / 一厂区"`) || !strings.Contains(form, "新建地点") {
		t.Fatalf("新建地点表单应预选上级：\n%s", form)
	}
	created := postStandaloneForm(t, handler, cookie, "/ui/more/locations/new",
		url.Values{"name": {"3 号楼 B 座"}, "parent": {"公司 / 一厂区"}})
	created.Body.Close()
	if created.StatusCode != http.StatusSeeOther ||
		created.Header.Get("Location") != "/ui/more/locations-networks/?notice=location-created" {
		t.Fatalf("新建地点应回到列表，状态码=%d 跳转=%q", created.StatusCode, created.Header.Get("Location"))
	}
	if len(writer.locationAdds) != 1 ||
		writer.locationAdds[0].name != "3 号楼 B 座" || writer.locationAdds[0].parent != "公司 / 一厂区" {
		t.Fatalf("新建地点未传给写入口：%+v", writer.locationAdds)
	}

	empty := postStandaloneForm(t, handler, cookie, "/ui/more/locations/new", url.Values{"name": {" "}})
	if empty.StatusCode != http.StatusOK || !strings.Contains(readResponseBody(t, empty), "请填写地点名称") {
		t.Fatalf("空地点名应回到表单并提示：状态码=%d", empty.StatusCode)
	}

	renamed := postStandaloneForm(t, handler, cookie, "/ui/more/locations/rename",
		url.Values{"site": {"公司 / 一厂区 / 3 号楼"}, "name": {"4 号楼"}})
	renamed.Body.Close()
	if len(writer.locationEdits) != 1 || writer.locationEdits[0].ref != "公司 / 一厂区 / 3 号楼" ||
		writer.locationEdits[0].name == nil || *writer.locationEdits[0].name != "4 号楼" {
		t.Fatalf("改名未传给写入口：%+v", writer.locationEdits)
	}

	moved := postStandaloneForm(t, handler, cookie, "/ui/more/locations/move",
		url.Values{"site": {"公司 / 一厂区 / 3 号楼"}, "parent": {"公司"}})
	moved.Body.Close()
	if len(writer.locationEdits) != 2 || writer.locationEdits[1].ref != "公司 / 一厂区 / 3 号楼" ||
		writer.locationEdits[1].parent == nil || *writer.locationEdits[1].parent != "公司" {
		t.Fatalf("移动未传给写入口：%+v", writer.locationEdits)
	}

	blockedPage := locationsNetworksGet(t, handler, cookie,
		"/ui/more/locations/delete?site="+url.QueryEscape("公司"))
	if !strings.Contains(blockedPage, "还有下级地点") || strings.Contains(blockedPage, "确认删除") {
		t.Fatalf("有下级的地点删除页应说明原因且没有提交按钮：\n%s", blockedPage)
	}
	writer.nextInvalid = &TopologyInvalidInputError{Message: "地点 公司 不能删除：还有下级地点：一厂区；请先删除或改上级"}
	blocked := postStandaloneForm(t, handler, cookie, "/ui/more/locations/delete",
		url.Values{"site": {"公司"}})
	if blocked.StatusCode != http.StatusBadRequest || !strings.Contains(readResponseBody(t, blocked), "还有下级地点：一厂区") {
		t.Fatalf("删除被拒绝应显示存储层原因，状态码=%d", blocked.StatusCode)
	}

	deleted := postStandaloneForm(t, handler, cookie, "/ui/more/locations/delete",
		url.Values{"site": {"机房"}})
	deleted.Body.Close()
	if len(writer.locationDeletes) != 2 || writer.locationDeletes[1] != "机房" {
		t.Fatalf("删除未传给写入口：%v", writer.locationDeletes)
	}
}

func TestNetworkCreateAndEditHandlers(t *testing.T) {
	writer := &stubTopologyWriter{}
	handler, cookie := newLocationsNetworksHandler(t, RoleStandalone,
		&stubTopologySource{data: locationsNetworksFixture()}, writer)

	physicalForm := locationsNetworksGet(t, handler, cookie, "/ui/more/networks/new?type=physical")
	for _, want := range []string{"新建网络", "物理局域网", "覆盖地点", `value="公司 / 一厂区 / 3 号楼"`, "标称带宽"} {
		if !strings.Contains(physicalForm, want) {
			t.Fatalf("新建物理局域网表单缺少 %q：\n%s", want, physicalForm)
		}
	}
	created := postStandaloneForm(t, handler, cookie, "/ui/more/networks/new", url.Values{
		"type": {"physical"}, "name": {"新办公网"}, "site": {"公司 / 一厂区 / 3 号楼"},
		"range": {"192.0.2.0/24"}, "bandwidth_mbps": {"1000"}, "metered": {"1"},
	})
	created.Body.Close()
	if created.StatusCode != http.StatusSeeOther {
		t.Fatalf("新建网络应成功，状态码=%d", created.StatusCode)
	}
	if len(writer.networkAdds) != 1 {
		t.Fatalf("新建网络未传给写入口：%+v", writer.networkAdds)
	}
	add := writer.networkAdds[0]
	if add.Name != "新办公网" || add.Type != "physical" || add.Site != "公司 / 一厂区 / 3 号楼" ||
		add.NetworkRange != "192.0.2.0/24" || add.BandwidthMbps != 1000 || !add.Metered {
		t.Fatalf("新建网络字段不符：%+v", add)
	}

	virtual := postStandaloneForm(t, handler, cookie, "/ui/more/networks/new", url.Values{
		"type": {"virtual"}, "name": {"新虚拟网"}, "range": {"100.64.0.0/24"},
	})
	virtual.Body.Close()
	if len(writer.networkAdds) != 2 || writer.networkAdds[1].Site != "" {
		t.Fatalf("新建虚拟局域网不应带地点：%+v", writer.networkAdds)
	}

	virtualWithSite := postStandaloneForm(t, handler, cookie, "/ui/more/networks/new", url.Values{
		"type": {"virtual"}, "name": {"坏虚拟网"}, "site": {"机房"},
	})
	if virtualWithSite.StatusCode != http.StatusOK ||
		!strings.Contains(readResponseBody(t, virtualWithSite), "虚拟局域网不覆盖地点") {
		t.Fatalf("虚拟局域网带地点应被拒绝，状态码=%d", virtualWithSite.StatusCode)
	}

	editForm := locationsNetworksGet(t, handler, cookie, "/ui/more/networks/edit?name="+url.QueryEscape("办公网"))
	for _, want := range []string{"编辑网络", `value="办公网"`, `value="192.0.2.0/24"`, "1000", "checked"} {
		if !strings.Contains(editForm, want) {
			t.Fatalf("编辑网络表单缺少 %q：\n%s", want, editForm)
		}
	}
	edited := postStandaloneForm(t, handler, cookie, "/ui/more/networks/edit", url.Values{
		"name": {"办公网"}, "site": {"公司 / 一厂区"}, "range": {"192.0.2.0/23"},
		"bandwidth_mbps": {"2000"},
	})
	edited.Body.Close()
	if len(writer.networkEdits) != 1 {
		t.Fatalf("编辑网络未传给写入口：%+v", writer.networkEdits)
	}
	edit := writer.networkEdits[0]
	if edit.name != "办公网" || edit.changes.Site == nil || *edit.changes.Site != "公司 / 一厂区" ||
		edit.changes.BandwidthMbps == nil || *edit.changes.BandwidthMbps != 2000 {
		t.Fatalf("编辑网络字段不符：%+v", edit)
	}
}
