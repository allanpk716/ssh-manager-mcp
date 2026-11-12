package webui

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	for _, target := range []string{"/ui/add", "/ui/servers/new"} {
		if body, status := serversGetStatus(t, client, clientCookie, target); status != http.StatusNotFound {
			t.Fatalf("客户端新增路由 %s 状态码 = %d，应为 404：%s", target, status, body)
		}
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
