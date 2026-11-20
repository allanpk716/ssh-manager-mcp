package webui

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// stubSiteBatchEdit 记录一次批量设置地点写请求的设备名与目标地点。
type stubSiteBatchEdit struct {
	Devices []string
	Site    string
}

type batchLocationTopologySource struct{}

func (batchLocationTopologySource) Topology(string, string) (*TopologyData, error) {
	return &TopologyData{
		Sites: []TopologySite{
			{ID: "site-company", Name: "公司", LocationPath: "公司"},
			{ID: "site-factory", ParentID: "site-company", Name: "一厂区", LocationPath: "公司 / 一厂区"},
		},
		Devices: []TopologyDevice{
			{
				ID: "device-a", Name: "机器甲", SiteID: "site-company", LocationPath: "公司",
				Servers: []TopologyDeviceServer{{ID: "srv-a1", Name: "甲账号一"}, {ID: "srv-a2", Name: "甲账号二"}},
			},
			{ID: "device-b", Name: "机器乙", Servers: []TopologyDeviceServer{{ID: "srv-b1", Name: "乙账号"}}},
		},
	}, nil
}

func newBatchLocationTestHandler(t *testing.T, role Role, writer TopologyWriter) (http.Handler, *http.Cookie) {
	t.Helper()
	servers := newStubServerStore()
	servers.rows = []ServerListEntry{
		{ID: "srv-a1", Name: "甲账号一", User: "root", Host: "192.0.2.10", Port: 22},
		{ID: "srv-a2", Name: "甲账号二", User: "root", Host: "192.0.2.11", Port: 22},
		{ID: "srv-b1", Name: "乙账号", User: "root", Host: "192.0.2.12", Port: 22},
		{ID: "srv-bare", Name: "未关联条目", User: "root", Host: "192.0.2.13", Port: 22},
	}
	config := Config{
		Role: role, Servers: servers, ServerWriter: &stubServerWriter{},
		Topology: batchLocationTopologySource{}, Access: &stubAccessStore{},
		VersionHint: func() string { return "" }, Port: 18080,
	}
	if writer != nil {
		config.TopologyWriter = writer
	}
	handler, startup, err := New(config)
	if err != nil {
		t.Fatalf("构造 Web UI Handler 失败：%v", err)
	}
	return handler, exchangeStartupTokenForCookie(t, handler, startup)
}

// TestServerBatchLocationEntrance 校验列表页的设置地点入口：按钮可用、
// 弹层提供完整路径单选与「未登记地点」，查看后果在未选择地点时禁用。
func TestServerBatchLocationEntrance(t *testing.T) {
	handler, cookie := newBatchLocationTestHandler(t, RoleStandalone, &stubTopologyWriter{})
	page := serversGet(t, handler, cookie, "/ui/servers/")

	button := regexp.MustCompile(`<button[^>]*data-server-set-location-action[^>]*>`).FindString(page)
	if button == "" {
		t.Fatalf("缺少设置地点按钮：\n%s", page)
	}
	if strings.Contains(button, "disabled") || strings.Contains(page, "批量设置地点流程尚未开放") {
		t.Fatalf("设置地点按钮应已启用且不再显示尚未开放提示：%q\n%s", button, page)
	}
	for _, want := range []string{
		`data-server-location-drawer`, `aria-label="选择目标地点"`,
		`name="batch_site" value="公司 / 一厂区"`, `value="__unregistered__"`, "未登记地点",
		`data-server-location-next`, "查看后果",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("设置地点弹层缺少 %q：\n%s", want, page)
		}
	}
	next := regexp.MustCompile(`<button[^>]*data-server-location-next[^>]*>`).FindString(page)
	if !strings.Contains(next, "disabled") {
		t.Fatalf("未选择地点时查看后果应禁用：%q", next)
	}
}

// TestServerBatchLocationConfirmPage 校验后果确认页：台数与目标地点、同
// 机其他服务器条目、未关联机器单列，以及确认表单携带的回传字段。
func TestServerBatchLocationConfirmPage(t *testing.T) {
	handler, cookie := newBatchLocationTestHandler(t, RoleStandalone, &stubTopologyWriter{})
	target := "/ui/servers/batch/location?" + url.Values{
		"server":    {"srv-a1", "srv-b1", "srv-bare"},
		"site":      {"公司 / 一厂区"},
		"return_to": {"/ui/servers/?query=batch"},
	}.Encode()
	page, status := serversGetStatus(t, handler, cookie, target)
	if status != http.StatusOK {
		t.Fatalf("确认页状态码 = %d，应为 200：\n%s", status, page)
	}
	for _, want := range []string{
		"将把 2 台服务器所在的机器地点设为 公司 / 一厂区。",
		"机器甲", "当前地点：公司", "甲账号二",
		"机器乙", "无", "未关联条目", "无法设置（未关联机器）",
		`method="post"`, `action="/ui/servers/batch/location"`,
		`name="server" value="srv-a1"`, `name="server" value="srv-b1"`, `name="server" value="srv-bare"`,
		`name="site" value="公司 / 一厂区"`, `name="return_to" value="/ui/servers/?query=batch"`,
		`name="csrf_token"`, "确认修改", "上一步", "取消",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("确认页缺少 %q：\n%s", want, page)
		}
	}
	if !strings.Contains(page, "/ui/servers/?query=batch&amp;batch_location=1") {
		t.Fatalf("上一步应回到原列表并重新打开地点弹层：\n%s", page)
	}
}

// TestServerBatchLocationPostWritesDevicesOnce 校验确认提交：按设备去重
// 后一次写入，选择「未登记地点」时清空地点，成功后回到原列表。
func TestServerBatchLocationPostWritesDevicesOnce(t *testing.T) {
	writer := &stubTopologyWriter{}
	handler, cookie := newBatchLocationTestHandler(t, RoleStandalone, writer)
	form := url.Values{
		"server":    {"srv-a1", "srv-a2", "srv-b1"},
		"site":      {"公司 / 一厂区"},
		"return_to": {"/ui/servers/?query=batch"},
	}
	response := serversPost(t, handler, cookie, "/ui/servers/batch/location", form)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("提交状态码 = %d，应为 303", response.StatusCode)
	}
	if location := response.Header.Get("Location"); location != "/ui/servers/?query=batch" {
		t.Fatalf("成功后应返回原列表，实际 %q", location)
	}
	if len(writer.siteBatchEdits) != 1 {
		t.Fatalf("确认一次只应写入一次，实际 %d 次", len(writer.siteBatchEdits))
	}
	write := writer.siteBatchEdits[0]
	if strings.Join(write.Devices, ",") != "机器甲,机器乙" || write.Site != "公司 / 一厂区" {
		t.Fatalf("写入参数应按设备去重：%+v", write)
	}
	if len(writer.sources) != 1 || writer.sources[0].RemoteAddress == "" {
		t.Fatalf("写入应携带来源地址：%+v", writer.sources)
	}

	writer.siteBatchEdits = nil
	unregistered := url.Values{
		"server": {"srv-a1"}, "site": {"__unregistered__"}, "return_to": {"/ui/servers/"},
	}
	response = serversPost(t, handler, cookie, "/ui/servers/batch/location", unregistered)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("清空地点提交状态码 = %d，应为 303", response.StatusCode)
	}
	if len(writer.siteBatchEdits) != 1 || writer.siteBatchEdits[0].Site != "" {
		t.Fatalf("未登记地点应以空地点引用写入：%+v", writer.siteBatchEdits)
	}
}

// TestServerBatchLocationValidationAndErrors 校验未选服务器、未选地点、
// 条目或地点不存在、写入失败与防跨站请求伪造错误都不写入。
func TestServerBatchLocationValidationAndErrors(t *testing.T) {
	writer := &stubTopologyWriter{}
	handler, cookie := newBatchLocationTestHandler(t, RoleStandalone, writer)
	target := "/ui/servers/batch/location"

	getCases := []struct {
		name       string
		query      url.Values
		wantStatus int
		wantText   string
	}{
		{
			name:       "未选服务器",
			query:      url.Values{"site": {"公司"}},
			wantStatus: http.StatusBadRequest, wantText: "请先选择要设置地点的服务器。",
		},
		{
			name:       "未选地点",
			query:      url.Values{"server": {"srv-a1"}},
			wantStatus: http.StatusBadRequest, wantText: "请先选择目标地点。",
		},
		{
			name:       "服务器不存在",
			query:      url.Values{"server": {"srv-missing"}, "site": {"公司"}},
			wantStatus: http.StatusBadRequest, wantText: "选中的服务器条目不存在，请刷新列表后重试。",
		},
		{
			name:       "目标地点不存在",
			query:      url.Values{"server": {"srv-a1"}, "site": {"不存在的地方"}},
			wantStatus: http.StatusBadRequest, wantText: "目标地点不存在，请返回上一步重选。",
		},
		{
			name:       "全部未关联机器",
			query:      url.Values{"server": {"srv-bare"}, "site": {"公司"}},
			wantStatus: http.StatusBadRequest, wantText: "选中的服务器条目都没有关联机器，无法设置地点。",
		},
	}
	for _, testCase := range getCases {
		page, status := serversGetStatus(t, handler, cookie, target+"?"+testCase.query.Encode())
		if status != testCase.wantStatus {
			t.Fatalf("%s 状态码 = %d，应为 %d：\n%s", testCase.name, status, testCase.wantStatus, page)
		}
		if !strings.Contains(page, testCase.wantText) {
			t.Fatalf("%s 应显示 %q：\n%s", testCase.name, testCase.wantText, page)
		}
		if len(writer.siteBatchEdits) != 0 {
			t.Fatalf("%s 不应触发写入：%+v", testCase.name, writer.siteBatchEdits)
		}
	}

	// 缺少或错误的防跨站请求伪造令牌都被统一入口拒绝，且不触发写入。
	for name, token := range map[string]string{"缺少防跨站请求伪造令牌": "", "防跨站请求伪造令牌错误": "坏令牌"} {
		form := url.Values{"server": {"srv-a1"}, "site": {"公司"}}
		if token != "" {
			form.Set("csrf_token", token)
		}
		request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
		request.Host = "127.0.0.1:18080"
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Origin", "http://127.0.0.1:18080")
		request.AddCookie(cookie)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, request)
		page := readResponseBody(t, rec.Result())
		if rec.Code != http.StatusForbidden || !strings.Contains(page, "防跨站请求伪造校验失败") {
			t.Fatalf("%s应按 403 拒绝：%d\n%s", name, rec.Code, page)
		}
		if len(writer.siteBatchEdits) != 0 {
			t.Fatalf("%s不应触发写入：%+v", name, writer.siteBatchEdits)
		}
	}

	// 写入口按名称不存在或内容非法返回时，页面给出明确错误且不跳转成功。
	writer.nextNotFoundError = true
	response := serversPost(t, handler, cookie, target, url.Values{
		"server": {"srv-a1"}, "site": {"公司"}, "return_to": {"/ui/servers/"},
	})
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("名称不存在应按 404 显示，实际 %d", response.StatusCode)
	}
	writer.nextInvalid = &TopologyInvalidInputError{Message: "地点引用不能为空"}
	response = serversPost(t, handler, cookie, target, url.Values{
		"server": {"srv-a1"}, "site": {"公司"}, "return_to": {"/ui/servers/"},
	})
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("内容非法应按 400 显示，实际 %d", response.StatusCode)
	}
	writer.nextPlainError = errors.New("底层存储故障")
	response = serversPost(t, handler, cookie, target, url.Values{
		"server": {"srv-a1"}, "site": {"公司"}, "return_to": {"/ui/servers/"},
	})
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("其它写入失败应按 500 显示，实际 %d", response.StatusCode)
	}
}

// TestServerBatchLocationRequiresWriter 校验只读角色与缺少拓扑写来源时
// 不提供批量设置地点入口。
func TestServerBatchLocationRequiresWriter(t *testing.T) {
	clientHandler, clientCookie := newBatchLocationTestHandler(t, RoleClient, &stubTopologyWriter{})
	page, status := serversGetStatus(t, clientHandler, clientCookie, "/ui/servers/batch/location?server=srv-a1&site=公司")
	if status != http.StatusNotFound || strings.Contains(page, "确认批量设置地点") {
		t.Fatalf("客户端只读角色不应访问批量设置地点：%d\n%s", status, page)
	}

	noWriterHandler, noWriterCookie := newBatchLocationTestHandler(t, RoleStandalone, nil)
	page, status = serversGetStatus(t, noWriterHandler, noWriterCookie, "/ui/servers/batch/location?server=srv-a1&site=公司")
	if status != http.StatusNotFound || strings.Contains(page, "确认批量设置地点") {
		t.Fatalf("缺少拓扑写来源时不应访问批量设置地点：%d\n%s", status, page)
	}
}
