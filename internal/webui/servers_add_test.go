package webui

import (
	"bytes"
	"html"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

type stubAddTopologySource struct{}

func (stubAddTopologySource) Topology(string, string) (*TopologyData, error) {
	return &TopologyData{
		Sites: []TopologySite{{ID: "site-1", Name: "家A"}},
		Networks: []TopologyNetwork{
			{ID: "net-physical", Name: "家A局域网", Type: "physical", SiteName: "家A"},
			{ID: "net-virtual", Name: "虚拟网", Type: "virtual"},
		},
		Devices: []TopologyDevice{{ID: "device-1", Name: "工作站", Kind: "server"}},
	}, nil
}

func TestServerAddTypeKeepsTopologyContext(t *testing.T) {
	h, cookie := newServersTestHandlerWithTopology(t, RoleStandalone, newStubServerStore(), &stubServerWriter{})
	page := serversGet(t, h, cookie, "/ui/add?network="+url.QueryEscape("虚拟网")+"&return_to="+url.QueryEscape("/ui/topology/"))

	hrefPattern := regexp.MustCompile(`href="([^"]+)"`)
	hrefs := hrefPattern.FindAllStringSubmatch(page, -1)
	var serverHref, deviceHref string
	for _, match := range hrefs {
		value, err := url.QueryUnescape(html.UnescapeString(match[1]))
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(value, "/ui/servers/new") || strings.Contains(value, "/ui/servers/new?") {
			serverHref = value
		}
		if strings.HasSuffix(value, "/ui/devices/new") || strings.Contains(value, "/ui/devices/new?") {
			deviceHref = value
		}
	}
	for name, href := range map[string]string{"服务器": serverHref, "其他设备": deviceHref} {
		parsed, err := url.Parse(href)
		if err != nil {
			t.Fatalf("%s新增地址无效：%v", name, err)
		}
		if parsed.Path != "/ui/servers/new" && parsed.Path != "/ui/devices/new" {
			t.Fatalf("%s新增地址错误：%s", name, href)
		}
		if got := parsed.Query().Get("network"); got != "虚拟网" {
			t.Fatalf("%s新增地址应保留网络上下文，实际 %q：%s", name, got, href)
		}
		if got := parsed.Query().Get("return_to"); got != "/ui/topology/" {
			t.Fatalf("%s新增地址应保留返回画布位置，实际 %q：%s", name, got, href)
		}
	}
}

func TestAddResultCanReturnToTopology(t *testing.T) {
	store := newStubServerStore()
	store.details["srv-new"] = &ServerDetailView{ID: "srv-new", Name: "same-device", User: "deploy"}
	writer := &stubServerWriter{}
	h, cookie := newServersTestHandlerWithTopology(t, RoleStandalone, store, writer)
	formPage := serversGet(t, h, cookie, "/ui/servers/new?device="+url.QueryEscape("工作站"))

	serverForm := url.Values{
		"csrf_token": {hiddenInputValue(t, formPage, "csrf_token")},
		"name":       {"same-device"}, "user": {"deploy"}, "port": {"22"},
		"shell": {"auto"}, "credential_action": {"none"},
		"device_choice": {"existing"}, "existing_device": {"工作站"}, "return_to": {"/ui/topology/"},
	}
	serverResponse := serversPost(t, h, cookie, "/ui/servers/new", serverForm)
	if serverResponse.StatusCode != http.StatusSeeOther || !strings.Contains(serverResponse.Header.Get("Location"), "return_to=%2Fui%2Ftopology%2F") {
		t.Fatalf("服务器保存后应携带画布返回位置，状态码=%d 跳转=%s", serverResponse.StatusCode, serverResponse.Header.Get("Location"))
	}
	serverResult := serversGet(t, h, cookie, serverResponse.Header.Get("Location"))
	if !strings.Contains(serverResult, `href="/ui/topology/">返回网络拓扑`) {
		t.Fatalf("服务器新增结果应提供返回网络拓扑入口：\n%s", serverResult)
	}

	deviceForm := url.Values{
		"csrf_token": {hiddenInputValue(t, formPage, "csrf_token")}, "device_name": {"画布设备"},
		"return_to": {"/ui/topology/"},
	}
	deviceResponse := serversPost(t, h, cookie, "/ui/devices/new", deviceForm)
	if deviceResponse.StatusCode != http.StatusSeeOther || !strings.Contains(deviceResponse.Header.Get("Location"), "return_to=%2Fui%2Ftopology%2F") {
		t.Fatalf("其他设备保存后应携带画布返回位置，状态码=%d 跳转=%s", deviceResponse.StatusCode, deviceResponse.Header.Get("Location"))
	}
	deviceResult := serversGet(t, h, cookie, deviceResponse.Header.Get("Location"))
	if !strings.Contains(deviceResult, `href="/ui/topology/">返回网络拓扑`) {
		t.Fatalf("其他设备新增结果应提供返回网络拓扑入口：\n%s", deviceResult)
	}
}

func TestServerAddFormReadsPastedOrUploadedPrivateKey(t *testing.T) {
	store := newStubServerStore()
	writer := &stubServerWriter{}
	h, cookie := newServersTestHandler(t, RoleStandalone, store, writer)
	formPage := serversGet(t, h, cookie, "/ui/servers/new")
	csrf := hiddenInputValue(t, formPage, "csrf_token")

	var body bytes.Buffer
	multipartForm := multipart.NewWriter(&body)
	values := map[string]string{
		"csrf_token": csrf, "name": "uploaded-key", "user": "deploy", "port": "22",
		"shell": "auto", "credential_action": "key", "key_source": "file", "host": "192.0.2.10",
	}
	for name, value := range values {
		if err := multipartForm.WriteField(name, value); err != nil {
			t.Fatal(err)
		}
	}
	file, err := multipartForm.CreateFormFile("key_file", "private-key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(file, "PRIVATE KEY BODY"); err != nil {
		t.Fatal(err)
	}
	if err := multipartForm.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/ui/servers/new", &body)
	req.Host = "127.0.0.1:18080"
	req.Header.Set("Content-Type", multipartForm.FormDataContentType())
	req.Header.Set("Origin", "http://127.0.0.1:18080")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	response := rec.Result()
	readResponseBody(t, response)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("文件私钥新增状态码 = %d，应为 303", response.StatusCode)
	}
	if len(writer.adds) != 1 || writer.adds[0].KeySource != "file" || writer.adds[0].KeyContent != "PRIVATE KEY BODY" {
		t.Fatalf("应读取上传的私钥文件：%+v", writer.adds)
	}
}

func TestServerAddEntryRoleMatrixAndCSRF(t *testing.T) {
	store := newStubServerStore()
	writer := &stubServerWriter{}
	h, cookie := newServersTestHandler(t, RoleStandalone, store, writer)

	list := serversGet(t, h, cookie, "/ui/")
	for _, want := range []string{"新增服务器", `href="/ui/add"`, "添加"} {
		if !strings.Contains(list, want) {
			t.Fatalf("可写角色的列表与页头应提供新增入口，缺少 %q：\n%s", want, list)
		}
	}
	typePage := serversGet(t, h, cookie, "/ui/add")
	if !strings.Contains(typePage, "服务器") {
		t.Fatalf("类型选择页应先提供服务器类型：\n%s", typePage)
	}
	formPage := serversGet(t, h, cookie, "/ui/servers/new")
	for _, want := range []string{"名称", "用户", "端口", "保存后自动探测", "新建设备", "已有设备", "profile 授权", "选择文件", "粘贴私钥"} {
		if !strings.Contains(formPage, want) {
			t.Fatalf("新增表单缺少 %q：\n%s", want, formPage)
		}
	}
	if !strings.Contains(formPage, "保存并进入待连接测试") {
		t.Fatalf("新增表单应完整渲染到提交按钮：\n%s", formPage)
	}

	invalid := serversPost(t, h, cookie, "/ui/servers/new", url.Values{
		"name": {"bad"}, "user": {"deploy"}, "port": {"0"},
		"credential_action": {"other"},
	})
	invalidBody := readResponseBody(t, invalid)
	if invalid.StatusCode != http.StatusBadRequest || !strings.Contains(invalidBody, "登录凭据类型无效") ||
		!strings.Contains(invalidBody, "端口必须") || len(writer.adds) != 0 {
		t.Fatalf("非法凭据类型与端口应返回表单错误且不写入，状态码=%d：\n%s", invalid.StatusCode, invalidBody)
	}

	form := url.Values{
		"name": {"web"}, "user": {"deploy"}, "port": {"22"}, "host": {"192.0.2.10"},
		"shell": {"auto"}, "credential_action": {"password"}, "password": {"login-secret"},
		"device_choice": {"existing"}, "existing_device": {"工作站"},
	}
	resp := serversPost(t, h, cookie, "/ui/servers/new", form)
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/ui/servers/new/result?") ||
		!strings.Contains(resp.Header.Get("Location"), "server=") {
		t.Fatalf("新增提交应成功进入结果页，状态码=%d 跳转=%s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if len(writer.adds) != 1 || writer.adds[0].Password != "login-secret" ||
		writer.adds[0].DeviceChoice != "existing" || writer.adds[0].ExistingDeviceName != "工作站" {
		t.Fatalf("新增提交内容不完整：%+v", writer.adds)
	}

	form.Del("csrf_token")
	req := httptest.NewRequest(http.MethodPost, "/ui/servers/new", strings.NewReader(form.Encode()))
	req.Host = "127.0.0.1:18080"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:18080")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("缺少防跨站请求伪造令牌的新增状态码 = %d，应为 403", rec.Code)
	}

	client, clientCookie := newServersTestHandler(t, RoleClient, store, nil)
	clientList := serversGet(t, client, clientCookie, "/ui/")
	if strings.Contains(clientList, "新增服务器") || strings.Contains(clientList, `href="/ui/add"`) ||
		!strings.Contains(clientList, "修改需到权威端 Web UI 去做") {
		t.Fatalf("客户端不应显示新增入口，且应提示到权威端修改：\n%s", clientList)
	}
	for _, target := range []string{"/ui/add", "/ui/servers/new", "/ui/devices/new"} {
		if body, status := serversGetStatus(t, client, clientCookie, target); status != http.StatusNotFound {
			t.Fatalf("客户端新增路由 %s 状态码 = %d，应为 404：%s", target, status, body)
		}
	}
}

func TestServerAddFormAppliesValidTopologyContext(t *testing.T) {
	store := newStubServerStore()
	writer := &stubServerWriter{}
	h, cookie := newServersTestHandlerWithTopology(t, RoleStandalone, store, writer)

	physical := serversGet(t, h, cookie, "/ui/servers/new?network="+url.QueryEscape("家A局域网")+"&return_to="+url.QueryEscape("/ui/topology/"))
	for _, want := range []string{
		`<option value="家A" selected>`,
		`<option value="家A局域网" selected>`,
		`<input type="hidden" name="return_to" value="/ui/topology/">`,
	} {
		if !strings.Contains(physical, want) {
			t.Fatalf("物理局域网入口应预选站点与网络，缺少 %q：\n%s", want, physical)
		}
	}

	virtual := serversGet(t, h, cookie, "/ui/devices/new?network="+url.QueryEscape("虚拟网"))
	if !strings.Contains(virtual, `<option value="虚拟网" selected>`) {
		t.Fatalf("虚拟局域网入口应预选第一个虚拟网络成员：\n%s", virtual)
	}

	device := serversGet(t, h, cookie, "/ui/servers/new?device="+url.QueryEscape("工作站"))
	for _, want := range []string{
		`id="device-existing" name="device_choice" value="existing" checked`,
		`<option value="工作站" selected>`,
	} {
		if !strings.Contains(device, want) {
			t.Fatalf("设备详情入口应预选已有设备，缺少 %q：\n%s", want, device)
		}
	}
	if len(writer.adds) != 0 || len(writer.otherAdds) != 0 {
		t.Fatalf("打开表单不应写入数据：%+v %+v", writer.adds, writer.otherAdds)
	}
}

func TestServerAddFormRejectsMissingTopologyContext(t *testing.T) {
	store := newStubServerStore()
	writer := &stubServerWriter{}
	h, cookie := newServersTestHandlerWithTopology(t, RoleStandalone, store, writer)

	for _, target := range []string{
		"/ui/servers/new?network=" + url.QueryEscape("不存在的网络"),
		"/ui/devices/new?network=" + url.QueryEscape("不存在的网络"),
		"/ui/servers/new?device=" + url.QueryEscape("不存在的设备"),
	} {
		body, status := serversGetStatus(t, h, cookie, target)
		if status != http.StatusNotFound {
			t.Fatalf("不存在的新增上下文 %s 状态码 = %d，应为 404", target, status)
		}
		if !strings.Contains(body, "不存在") {
			t.Fatalf("不存在的新增上下文应返回中文说明：\n%s", body)
		}
	}
	if len(writer.adds) != 0 || len(writer.otherAdds) != 0 {
		t.Fatalf("无效上下文不应写入数据：%+v %+v", writer.adds, writer.otherAdds)
	}
}

func TestAuthorityServerAddCredentialPreservesSubmittedFieldsAfterReverify(t *testing.T) {
	store := newStubServerStore()
	writer := &stubServerWriter{}
	auth := &fakeAuthorityAuth{passwordSet: true, password: "correct-password"}
	now := time.Unix(1800000000, 0)
	handler, _, err := NewAuthority(Config{
		Role: RoleServer, Servers: store, ServerWriter: writer,
		Topology: stubAddTopologySource{}, Access: &stubAccessStore{}, Auth: auth,
		CertificateFingerprint: "sha256:0123456789abcdef",
		AllowedHosts:           []string{"127.0.0.1"}, Port: 7879,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie := authoritySuccessfulLogin(t, handler, "127.0.0.1:7879", "correct-password")
	now = now.Add(6 * time.Minute)
	addPage := authorityGet(t, handler, "127.0.0.1:7879", "/ui/servers/new", cookie)
	addPageBody := readResponseBody(t, addPage)

	form := url.Values{
		"csrf_token": {hiddenInputValue(t, addPageBody, "csrf_token")},
		"name":       {"web"}, "user": {"deploy"}, "port": {"2202"}, "shell": {"auto"},
		"credential_action": {"password"}, "password": {"authority-secret"},
		"description": {"保留的说明"}, "role": {"网站"},
		"device_choice": {"new"}, "device_name": {"工作站"},
		"physical_network": {"家A局域网"}, "physical_address": {"192.168.1.10"},
	}
	resp := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/servers/new", form, cookie)
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "/ui/reverify") ||
		strings.Contains(resp.Header.Get("Location"), "authority-secret") {
		t.Fatalf("未验证的凭据新增应跳转重新验证且不得带凭据：%d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	next := resp.Header.Get("Location")
	reverify := authorityGet(t, handler, "127.0.0.1:7879", next, cookie)
	reverifyBody := readResponseBody(t, reverify)
	verified := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/reverify", url.Values{
		"csrf_token": {hiddenInputValue(t, reverifyBody, "csrf_token")},
		"password":   {"correct-password"},
		"next":       {hiddenInputValue(t, reverifyBody, "next")},
	}, cookie)
	if verified.StatusCode != http.StatusSeeOther {
		t.Fatalf("重新验证状态码 = %d", verified.StatusCode)
	}
	resumed := authorityGet(t, handler, "127.0.0.1:7879", verified.Header.Get("Location"), cookie)
	resumedBody := readResponseBody(t, resumed)
	if resumed.StatusCode != http.StatusOK || !strings.Contains(resumedBody, "保留的说明") ||
		!strings.Contains(resumedBody, "已保留表单状态") || strings.Contains(resumedBody, "authority-secret") {
		t.Fatalf("重新验证回跳后应保留非凭据字段且不回显凭据，状态码=%d：\n%s", resumed.StatusCode, resumedBody)
	}
	resumedForm := url.Values{
		"csrf_token": {hiddenInputValue(t, resumedBody, "csrf_token")},
		"name":       {"web"}, "user": {"deploy"}, "port": {"2202"}, "shell": {"auto"},
		"credential_action": {"password"}, "resume_credentials": {"1"},
		"description": {"保留的说明"}, "role": {"网站"},
		"device_choice": {"new"}, "device_name": {"工作站"},
		"physical_network": {"家A局域网"}, "physical_address": {"192.168.1.10"},
	}
	saved := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/servers/new", resumedForm, cookie)
	readResponseBody(t, saved)
	if saved.StatusCode != http.StatusSeeOther || !strings.HasPrefix(saved.Header.Get("Location"), "/ui/servers/new/result?") ||
		!strings.Contains(saved.Header.Get("Location"), "server=") {
		t.Fatalf("重新验证后的新增应保存，状态码=%d 跳转=%s", saved.StatusCode, saved.Header.Get("Location"))
	}
	if len(writer.adds) != 1 || writer.adds[0].Password != "authority-secret" || writer.adds[0].Description != "保留的说明" {
		t.Fatalf("重新验证后的提交应使用暂存凭据并保留字段：%+v", writer.adds)
	}
}

// TestServerAddResultPollsConnectionTest 覆盖保存后的公共页面边界：表单提交
// 先返回结果页，连接测试任务在后台进行，结果页按现有 htmx 轮询模式刷新。
func TestServerAddResultPollsConnectionTest(t *testing.T) {
	store := newStubServerStore()
	store.details["srv-new"] = &ServerDetailView{
		ID: "srv-new", Name: "web", User: "deploy", Host: "192.0.2.20", Port: 22, Shell: "posix",
	}
	writer := &stubServerWriter{}
	tester := &stubServerConnectionTester{}
	h, cookie := newServersTestHandlerWithConnectionTester(t, RoleStandalone, store, writer, tester)
	formPage := serversGet(t, h, cookie, "/ui/servers/new")
	form := url.Values{
		"csrf_token": {hiddenInputValue(t, formPage, "csrf_token")},
		"name":       {"web"}, "user": {"deploy"}, "port": {"22"}, "host": {"192.0.2.20"},
		"shell": {"auto"}, "credential_action": {"none"},
	}
	resp := serversPost(t, h, cookie, "/ui/servers/new", form)
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "task=task-1") {
		t.Fatalf("保存后应立即进入连接测试结果页，状态码=%d 跳转=%s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if len(tester.starts) != 1 || tester.starts[0].serverID != "srv-new" || !tester.starts[0].probeShell {
		t.Fatalf("应立即启动一次自动 shell 连接测试：%+v", tester.starts)
	}

	page := serversGet(t, h, cookie, resp.Header.Get("Location"))
	body := page
	for _, want := range []string{"连接测试进行中", "已验证状态等待测试完成", `hx-get="/ui/servers/new/result?`} {
		if !strings.Contains(body, want) {
			t.Fatalf("结果页应显示进行中任务并继续轮询，缺少 %q：\n%s", want, body)
		}
	}
}

// TestServerAddResultShowsConnectionTestOutcome 覆盖结果页对成功与失败的
// 呈现：成功显示已验证、指纹、锚定与 shell 结论；失败保留条目并明确未验证。
func TestServerAddResultShowsConnectionTestOutcome(t *testing.T) {
	store := newStubServerStore()
	store.details["srv-new"] = &ServerDetailView{
		ID: "srv-new", Name: "web", User: "deploy", Host: "192.0.2.20", Port: 22, Shell: "posix",
	}
	tester := &stubServerConnectionTester{}
	h, cookie := newServersTestHandlerWithConnectionTester(t, RoleStandalone, store, &stubServerWriter{}, tester)

	tester.status = &ServerConnectionTestStatus{
		ID: "task-1", State: ServerConnectionTestSucceeded, ServerID: "srv-new", ProbeShell: true,
		AnchorFingerprint: "SHA256:known-host-key-fingerprint", AnchorResult: "首次连接已记录主机密钥锚",
		HandshakeAddress: "100.64.0.20:22", ShellDetected: "powershell",
		ShellEvidence: "PowerShell 输出了版本表", ConnectionNotes: []string{"物理网络 第1个地址呈现了不同的主机密钥，已跳过"},
	}
	success := serversGet(t, h, cookie, "/ui/servers/new/result?server=srv-new&task=task-1")
	for _, want := range []string{
		"已验证", "SHA256:known-host-key-fingerprint", "首次连接已记录主机密钥锚",
		"100.64.0.20:22", "shell 类型已保存为 powershell", "PowerShell 输出了版本表",
		"物理网络 第1个地址呈现了不同的主机密钥，已跳过",
	} {
		if !strings.Contains(success, want) {
			t.Fatalf("成功结果页缺少 %q：\n%s", want, success)
		}
	}
	if strings.Contains(success, "hx-get=\"/ui/servers/new/result?") {
		t.Fatalf("已完成任务不应继续轮询：\n%s", success)
	}

	tester.status = &ServerConnectionTestStatus{
		ID: "task-1", State: ServerConnectionTestFailed, ServerID: "srv-new", ProbeShell: true,
		Message:         "全部候选地址连接失败：\n兜底地址：连接超时",
		ConnectionNotes: []string{"虚拟网络 第1个地址呈现了不同的主机密钥，已跳过"},
	}
	failure := serversGet(t, h, cookie, "/ui/servers/new/result?server=srv-new&task=task-1")
	for _, want := range []string{"未验证", "全部候选地址连接失败", "兜底地址：连接超时", "shell 未验证", "默认 POSIX"} {
		if !strings.Contains(failure, want) {
			t.Fatalf("失败结果页缺少 %q：\n%s", want, failure)
		}
	}
}

type stubServerConnectionStart struct {
	instance   string
	serverID   string
	probeShell bool
	source     ServerWriteSource
}

type stubServerConnectionTester struct {
	starts []stubServerConnectionStart
	status *ServerConnectionTestStatus
}

func (s *stubServerConnectionTester) StartServerTest(
	instance, serverID string, probeShell bool, source ServerWriteSource,
) (string, error) {
	s.starts = append(s.starts, stubServerConnectionStart{
		instance: instance, serverID: serverID, probeShell: probeShell, source: source,
	})
	return "task-1", nil
}

func (s *stubServerConnectionTester) ServerTestStatus(taskID string) (*ServerConnectionTestStatus, bool) {
	if taskID != "task-1" {
		return nil, false
	}
	if s.status != nil {
		return s.status, true
	}
	return &ServerConnectionTestStatus{
		ID: taskID, State: ServerConnectionTestRunning, ServerID: "srv-new", ProbeShell: true,
	}, true
}

func newServersTestHandlerWithConnectionTester(
	t *testing.T, role Role, store ServerSource, writer ServerWriter, tester ServerConnectionTester,
) (http.Handler, *http.Cookie) {
	t.Helper()
	handler, startup, err := New(Config{
		Role: role, Servers: store, ServerWriter: writer,
		ServerConnectionTester: tester, Access: &stubAccessStore{},
		VersionHint: func() string { return "" }, Port: 18080,
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

func newServersTestHandlerWithTopology(
	t *testing.T, role Role, store ServerSource, writer ServerWriter,
) (http.Handler, *http.Cookie) {
	t.Helper()
	handler, startup, err := New(Config{
		Role: role, Servers: store, ServerWriter: writer, Topology: stubAddTopologySource{},
		Access: &stubAccessStore{}, VersionHint: func() string { return "" }, Port: 18080,
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
