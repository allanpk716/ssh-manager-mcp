package webui

import (
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
)

type erroringDetailServerSource struct {
	*stubServerStore
	err   error
	calls int
}

func (s *erroringDetailServerSource) ServerDetail(_ string, _ string) (*ServerDetailView, error) {
	s.calls++
	return nil, s.err
}

// TestAuthorityServerWizardStagesCredentialForReverify 覆盖第 1 步的五分
// 钟重新验证守卫：未重新验证时不写入；重新验证后暂存凭据只续交一次，页面
// 不回显凭据内容。
func TestAuthorityServerWizardStagesCredentialForReverify(t *testing.T) {
	auth := &fakeAuthorityAuth{passwordSet: true, password: "correct-password", now: time.Unix(1800000000, 0)}
	store := newStubServerStore()
	store.details["srv-new"] = &ServerDetailView{ID: "srv-new", Name: "wizard", User: "deploy", Host: "192.0.2.20", Port: 22}
	writer := &stubServerWriter{}
	handler, _, err := NewAuthority(Config{
		Role: RoleServer, Servers: store, ServerWriter: writer, Topology: stubAddTopologySource{},
		Access: &stubAccessStore{}, Auth: auth, AllowedHosts: []string{"127.0.0.1"}, Port: 7879,
		CertificateFingerprint: "sha256:0123456789abcdef",
		Now:                    func() time.Time { return auth.now },
	})
	if err != nil {
		t.Fatalf("构造权威端 Web UI Handler 失败：%v", err)
	}
	cookie := authoritySuccessfulLogin(t, handler, "127.0.0.1:7879", "correct-password")
	auth.now = auth.now.Add(6 * time.Minute)

	formPage := authorityGet(t, handler, "127.0.0.1:7879", "/ui/servers/new", cookie)
	formBody := readResponseBody(t, formPage)
	csrf := hiddenInputValue(t, formBody, "csrf_token")
	submit := url.Values{
		"csrf_token": {csrf}, "name": {"wizard"}, "user": {"deploy"}, "port": {"22"},
		"address_network": {"other"}, "address": {"192.0.2.20"},
		"credential_action": {"password"}, "password": {"login-secret"}, "shell": {"auto"},
	}

	rejected := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/servers/new", submit, cookie)
	if rejected.StatusCode != http.StatusSeeOther ||
		rejected.Header.Get("Location") != `/ui/reverify?next=`+url.QueryEscape("/ui/servers/new?resume=1") {
		t.Fatalf("带凭据提交应先重新验证，状态码=%d 跳转=%q", rejected.StatusCode, rejected.Header.Get("Location"))
	}
	if len(writer.adds) != 0 {
		t.Fatalf("未重新验证时不得写入服务器，实际 %+v", writer.adds)
	}

	reverifyPage := authorityGet(t, handler, "127.0.0.1:7879", "/ui/reverify?next="+url.QueryEscape("/ui/servers/new?resume=1"), cookie)
	reverifyBody := readResponseBody(t, reverifyPage)
	reverified := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/reverify", url.Values{
		"csrf_token": {hiddenInputValue(t, reverifyBody, "csrf_token")},
		"password":   {"correct-password"}, "next": {"/ui/servers/new?resume=1"},
	}, cookie)
	if reverified.StatusCode != http.StatusSeeOther {
		t.Fatalf("重新验证后应回到新增表单，状态码=%d", reverified.StatusCode)
	}

	resumed := authorityGet(t, handler, "127.0.0.1:7879", "/ui/servers/new?resume=1", cookie)
	resumedBody := readResponseBody(t, resumed)
	for _, want := range []string{"已保留表单状态", `name="resume_credentials" value="1"`} {
		if !strings.Contains(resumedBody, want) {
			t.Fatalf("重新验证后的表单应携带暂存凭据标记，缺少 %q：\n%s", want, resumedBody)
		}
	}
	if strings.Contains(resumedBody, "login-secret") {
		t.Fatalf("暂存凭据不得回显到页面：\n%s", resumedBody)
	}
	delete(submit, "password")
	submit.Set("resume_credentials", "1")
	accepted := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/servers/new", submit, cookie)
	if accepted.StatusCode != http.StatusSeeOther || !strings.Contains(accepted.Header.Get("Location"), "/ui/servers/new/test?") {
		t.Fatalf("重新验证后应续交凭据并进入连接测试，状态码=%d 跳转=%q", accepted.StatusCode, accepted.Header.Get("Location"))
	}
	if len(writer.adds) != 1 || writer.adds[0].Password != "login-secret" {
		t.Fatalf("应沿用暂存登录密码写入，实际 %+v", writer.adds)
	}
	if again := authorityGet(t, handler, "127.0.0.1:7879", "/ui/servers/new?resume=1", cookie); !strings.Contains(readResponseBody(t, again), "已超过五分钟") {
		t.Fatalf("暂存凭据只能使用一次：\n%s", readResponseBody(t, again))
	}
}

func TestServerWizardDeviceContextKeepsExistingMachine(t *testing.T) {
	store := newStubServerStore()
	store.details["srv-new"] = &ServerDetailView{ID: "srv-new", Name: "wizard", User: "deploy", Host: "192.0.2.20", Port: 22}
	writer := &stubServerWriter{}
	h, cookie := newServersTestHandlerWithTopology(t, RoleStandalone, store, writer)

	page := serversGet(t, h, cookie, "/ui/servers/new?device="+url.QueryEscape("工作站"))
	for _, want := range []string{
		"将新增到机器 工作站 上的另一个账号",
		`name="device_choice" value="existing"`,
		`name="existing_device" value="工作站"`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("设备详情入口应显示并保留同一台机器，缺少 %q：\n%s", want, page)
		}
	}
	response := serversPost(t, h, cookie, "/ui/servers/new", url.Values{
		"csrf_token": {hiddenInputValue(t, page, "csrf_token")},
		"name":       {"wizard"}, "user": {"deploy"}, "port": {"22"},
		"address_network": {"other"}, "address": {"192.0.2.20"},
		"credential_action": {"none"}, "shell": {"auto"},
		"device_choice": {"existing"}, "existing_device": {"工作站"},
	})
	if response.StatusCode != http.StatusSeeOther || !strings.Contains(response.Header.Get("Location"), "same_machine=true") {
		t.Fatalf("挂到已有机器时应跳过地点步骤，状态码=%d 跳转=%s", response.StatusCode, response.Header.Get("Location"))
	}
	if len(writer.adds) != 1 || writer.adds[0].DeviceChoice != "existing" || writer.adds[0].ExistingDeviceName != "工作站" {
		t.Fatalf("提交应沿用已有机器，实际 %+v", writer.adds)
	}
}

func TestAuthorityServerWizardDraftExpiresAndCanBeCleared(t *testing.T) {
	auth := &fakeAuthorityAuth{passwordSet: true, password: "correct-password", now: time.Unix(1800000000, 0)}
	store := newStubServerStore()
	store.details["srv-new"] = &ServerDetailView{ID: "srv-new", Name: "wizard", User: "deploy", Host: "192.0.2.20", Port: 22}
	writer := &stubServerWriter{}
	handler, _, err := NewAuthority(Config{
		Role: RoleServer, Servers: store, ServerWriter: writer, Topology: stubAddTopologySource{},
		Access: &stubAccessStore{}, Auth: auth, AllowedHosts: []string{"127.0.0.1"}, Port: 7879,
		CertificateFingerprint: "sha256:0123456789abcdef",
		Now:                    func() time.Time { return auth.now },
	})
	if err != nil {
		t.Fatalf("构造权威端 Web UI Handler 失败：%v", err)
	}
	cookie := authoritySuccessfulLogin(t, handler, "127.0.0.1:7879", "correct-password")
	auth.now = auth.now.Add(6 * time.Minute)
	formPage := authorityGet(t, handler, "127.0.0.1:7879", "/ui/servers/new", cookie)
	formBody := readResponseBody(t, formPage)
	csrf := hiddenInputValue(t, formBody, "csrf_token")

	authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/servers/new", url.Values{
		"csrf_token": {csrf}, "name": {"wizard"}, "user": {"deploy"}, "port": {"22"},
		"address_network": {"other"}, "address": {"192.0.2.20"},
		"credential_action": {"password"}, "password": {"staged-secret"}, "shell": {"auto"},
	}, cookie)
	auth.now = auth.now.Add(6 * time.Minute)

	expired := authorityGet(t, handler, "127.0.0.1:7879", "/ui/servers/new?resume=1", cookie)
	expiredBody := readResponseBody(t, expired)
	if !strings.Contains(expiredBody, "暂存的登录凭据已超过五分钟") {
		t.Fatalf("过期草稿应提示重新填写：\n%s", expiredBody)
	}
	expiredSubmit := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/servers/new", url.Values{
		"csrf_token": {hiddenInputValue(t, expiredBody, "csrf_token")},
		"name":       {"wizard"}, "user": {"deploy"}, "port": {"22"},
		"address_network": {"other"}, "address": {"192.0.2.20"},
		"credential_action": {"password"}, "resume_credentials": {"1"}, "shell": {"auto"},
	}, cookie)
	expiredSubmitBody := readResponseBody(t, expiredSubmit)
	if expiredSubmit.StatusCode != http.StatusBadRequest || !strings.Contains(expiredSubmitBody, "暂存的登录凭据已超过五分钟") ||
		len(writer.adds) != 0 {
		t.Fatalf("过期草稿不得被使用，状态码=%d 写入=%+v 页面=%s", expiredSubmit.StatusCode, writer.adds, expiredSubmitBody)
	}

	// 重新走一次暂存，验证 owner 可以主动清除暂存凭据。
	auth.now = auth.now.Add(6 * time.Minute)
	authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/servers/new", url.Values{
		"csrf_token": {csrf}, "name": {"wizard"}, "user": {"deploy"}, "port": {"22"},
		"address_network": {"other"}, "address": {"192.0.2.20"},
		"credential_action": {"password"}, "password": {"second-secret"}, "shell": {"auto"},
	}, cookie)
	reverifyPage := authorityGet(t, handler, "127.0.0.1:7879", "/ui/reverify?next="+url.QueryEscape("/ui/servers/new?resume=1"), cookie)
	reverifyBody := readResponseBody(t, reverifyPage)
	authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/reverify", url.Values{
		"csrf_token": {hiddenInputValue(t, reverifyBody, "csrf_token")},
		"password":   {"correct-password"}, "next": {"/ui/servers/new?resume=1"},
	}, cookie)
	resumed := authorityGet(t, handler, "127.0.0.1:7879", "/ui/servers/new?resume=1", cookie)
	resumedBody := readResponseBody(t, resumed)
	if !strings.Contains(resumedBody, `name="clear_staged_credentials"`) {
		t.Fatalf("续交页面应提供清除暂存凭据的入口：\n%s", resumedBody)
	}
	cleared := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/servers/new", url.Values{
		"csrf_token": {hiddenInputValue(t, resumedBody, "csrf_token")},
		"name":       {"wizard"}, "user": {"deploy"}, "port": {"22"},
		"address_network": {"other"}, "address": {"192.0.2.20"},
		"credential_action": {"password"}, "password": {"new-secret"},
		"clear_staged_credentials": {"1"}, "shell": {"auto"},
	}, cookie)
	clearedBody := readResponseBody(t, cleared)
	if cleared.StatusCode != http.StatusOK || !strings.Contains(clearedBody, "已清除暂存的登录凭据") ||
		strings.Contains(clearedBody, "new-secret") || strings.Contains(clearedBody, `name="resume_credentials"`) {
		t.Fatalf("owner 应能主动清除草稿，状态码=%d 页面=%s", cleared.StatusCode, clearedBody)
	}
	afterClear := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/servers/new", url.Values{
		"csrf_token": {hiddenInputValue(t, clearedBody, "csrf_token")},
		"name":       {"wizard"}, "user": {"deploy"}, "port": {"22"},
		"address_network": {"other"}, "address": {"192.0.2.20"},
		"credential_action": {"password"}, "resume_credentials": {"1"}, "shell": {"auto"},
	}, cookie)
	if afterClear.StatusCode != http.StatusBadRequest || len(writer.adds) != 0 {
		t.Fatalf("清除后不得继续使用草稿，状态码=%d 写入=%+v", afterClear.StatusCode, writer.adds)
	}
}

func TestServerWizardRejectsHalfFilledAddressRow(t *testing.T) {
	store := newStubServerStore()
	writer := &stubServerWriter{}
	h, cookie := newServersTestHandlerWithTopology(t, RoleStandalone, store, writer)
	page := serversGet(t, h, cookie, "/ui/servers/new")

	response := serversPost(t, h, cookie, "/ui/servers/new", url.Values{
		"csrf_token": {hiddenInputValue(t, page, "csrf_token")},
		"name":       {"wizard"}, "user": {"deploy"}, "port": {"22"},
		"address_network": {"家A局域网"}, "address": {""},
		"credential_action": {"none"}, "shell": {"auto"},
	})
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(body, "每行网络地址必须同时选择网络并填写地址") {
		t.Fatalf("半填的地址行应被拒绝，状态码=%d 页面：\n%s", response.StatusCode, body)
	}
	if len(writer.adds) != 0 {
		t.Fatalf("校验失败不得写入，实际 %+v", writer.adds)
	}
}

func TestServerWizardAddAddressStagesCredentialWithoutScript(t *testing.T) {
	store := newStubServerStore()
	store.details["srv-new"] = &ServerDetailView{ID: "srv-new", Name: "wizard", User: "deploy", Host: "192.0.2.20", Port: 22}
	writer := &stubServerWriter{}
	h, cookie := newServersTestHandlerWithTopology(t, RoleStandalone, store, writer)
	page := serversGet(t, h, cookie, "/ui/servers/new")
	csrf := hiddenInputValue(t, page, "csrf_token")

	appended := serversPost(t, h, cookie, "/ui/servers/new", url.Values{
		"csrf_token": {csrf}, "name": {"wizard"}, "user": {"deploy"}, "port": {"22"},
		"address_network": {"other"}, "address": {"192.0.2.20"},
		"credential_action": {"password"}, "password": {"login-secret"},
		"shell": {"auto"}, "add_address": {"1"},
	})
	appendedBody := readResponseBody(t, appended)
	if appended.StatusCode != http.StatusOK || !strings.Contains(appendedBody, `name="resume_credentials" value="1"`) {
		t.Fatalf("无脚本追加地址行应暂存凭据，状态码=%d 页面：\n%s", appended.StatusCode, appendedBody)
	}
	if strings.Contains(appendedBody, "login-secret") {
		t.Fatalf("追加行时不得回显登录密码：%s", appendedBody)
	}
	saved := serversPost(t, h, cookie, "/ui/servers/new", url.Values{
		"csrf_token": {csrf}, "name": {"wizard"}, "user": {"deploy"}, "port": {"22"},
		"address_network": {"other", "other"}, "address": {"192.0.2.20", "192.0.2.21"},
		"credential_action": {"password"}, "resume_credentials": {"1"}, "shell": {"auto"},
	})
	if saved.StatusCode != http.StatusSeeOther || len(writer.adds) != 1 || writer.adds[0].Password != "login-secret" {
		t.Fatalf("最终提交应沿用暂存密码，状态码=%d 写入=%+v", saved.StatusCode, writer.adds)
	}
}

func TestServerWizardAnchorCopyFlowsIntoTestPage(t *testing.T) {
	store := newStubServerStore()
	store.details["srv-new"] = &ServerDetailView{ID: "srv-new", Name: "web", User: "deploy", Host: "192.0.2.20", Port: 22}
	writer := &stubServerWriter{anchorCopied: true, anchorFrom: "sibling"}
	tester := &stubServerConnectionTester{}
	h, cookie := newServersTestHandlerWithConnectionTester(t, RoleStandalone, store, writer, tester)
	formPage := serversGet(t, h, cookie, "/ui/servers/new")
	response := serversPost(t, h, cookie, "/ui/servers/new", url.Values{
		"csrf_token": {hiddenInputValue(t, formPage, "csrf_token")},
		"name":       {"web"}, "user": {"deploy"}, "port": {"22"},
		"address_network": {"other"}, "address": {"192.0.2.20"},
		"shell": {"auto"}, "credential_action": {"none"},
	})
	readResponseBody(t, response)
	if len(tester.starts) != 1 || !tester.starts[0].anchor.Copied ||
		tester.starts[0].anchor.FromServerName != "sibling" {
		t.Fatalf("锚复制事实应随连接测试任务携带：%+v", tester.starts)
	}
	tester.status = &ServerConnectionTestStatus{
		ID: "task-1", State: ServerConnectionTestRunning, ServerID: "srv-new",
		AnchorCopied: true, AnchorCopyFromServerName: "sibling",
	}
	page := serversGet(t, h, cookie, response.Header.Get("Location"))
	if !strings.Contains(page, "已从服务器条目 sibling 复制同端口主机密钥锚") {
		t.Fatalf("连接测试页应显示任务携带的锚复制来源：%s", page)
	}
}

func TestServerWizardUploadsPrivateKeyFile(t *testing.T) {
	store := newStubServerStore()
	store.details["srv-new"] = &ServerDetailView{ID: "srv-new", Name: "web", User: "deploy", Host: "192.0.2.20", Port: 22}
	writer := &stubServerWriter{}
	tester := &stubServerConnectionTester{}
	h, cookie := newServersTestHandlerWithConnectionTester(t, RoleStandalone, store, writer, tester)
	formPage := serversGet(t, h, cookie, "/ui/servers/new")
	csrf := hiddenInputValue(t, formPage, "csrf_token")

	var body bytes.Buffer
	multipartForm := multipart.NewWriter(&body)
	values := map[string]string{
		"csrf_token": csrf, "name": "uploaded-key", "user": "deploy", "port": "22",
		"shell": "auto", "credential_action": "key", "key_source": "file",
		"address_network": "other", "address": "192.0.2.20",
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

func TestServerWizardHasItsOwnRequestBodyLimitAndMessage(t *testing.T) {
	store := newStubServerStore()
	writer := &stubServerWriter{}
	h, cookie := newServersTestHandler(t, RoleStandalone, store, writer)
	formPage := serversGet(t, h, cookie, "/ui/servers/new")
	csrf := hiddenInputValue(t, formPage, "csrf_token")

	form := url.Values{
		"csrf_token": {csrf}, "name": {"too-large"}, "user": {"deploy"}, "port": {"22"},
		"shell": {"auto"}, "credential_action": {"none"},
		"address_network": {"other"},
		"address":         {strings.Repeat("a", 128*1024)},
	}
	req := httptest.NewRequest(http.MethodPost, "/ui/servers/new", strings.NewReader(form.Encode()))
	req.Host = "127.0.0.1:18080"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:18080")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	response := rec.Result()
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusRequestEntityTooLarge ||
		!strings.Contains(body, "新增服务器表单过大") {
		t.Fatalf("新增服务器表单应有独立上限与错误说明，状态码=%d：%s", response.StatusCode, body)
	}
	if len(writer.adds) != 0 {
		t.Fatalf("超限请求不得保存服务器：%+v", writer.adds)
	}
}

func TestServerWizardContextRejectsUnknownNetworkOrDevice(t *testing.T) {
	h, cookie := newServersTestHandlerWithTopology(t, RoleStandalone, newStubServerStore(), &stubServerWriter{})
	for _, item := range []struct{ name, query, want string }{
		{name: "网络", query: "/ui/servers/new?network=" + url.QueryEscape("不存在网络"), want: "选择的物理局域网或虚拟局域网不存在"},
		{name: "设备", query: "/ui/servers/new?device=" + url.QueryEscape("不存在设备"), want: "选择的设备不存在"},
	} {
		body, status := serversGetStatus(t, h, cookie, item.query)
		if status != http.StatusNotFound || !strings.Contains(body, item.want) {
			t.Fatalf("%s预选无效时应返回 404，状态码=%d 页面：\n%s", item.name, status, body)
		}
	}
}

func TestServerWizardFailedTestReturnsToSavedEntryEditor(t *testing.T) {
	store := newStubServerStore()
	store.details["srv-new"] = &ServerDetailView{ID: "srv-new", Name: "wizard", User: "deploy", Host: "192.0.2.20", Port: 22}
	tester := &stubServerConnectionTester{status: &ServerConnectionTestStatus{
		ID: "task-1", State: ServerConnectionTestFailed, ServerID: "srv-new",
	}}
	h, cookie := newServersTestHandlerWithConnectionTester(t, RoleStandalone, store, &stubServerWriter{}, tester)

	page := serversGet(t, h, cookie, "/ui/servers/new/test?server=srv-new&task=task-1")
	want := `href="/ui/servers/srv-new?tab=connection&amp;edit=1"`
	if !strings.Contains(page, want) {
		t.Fatalf("连接测试失败后应返回已保存服务器条目的编辑入口，缺少 %q：%s", want, page)
	}
	if strings.Contains(page, `href="/ui/servers/new"`) {
		t.Fatalf("连接测试失败后不得再进入会重复新建的入口：%s", page)
	}
}

func TestServerAddResultReportsStorageReadFailure(t *testing.T) {
	store := newStubServerStore()
	store.details["srv-new"] = &ServerDetailView{ID: "srv-new", Name: "web", User: "deploy"}
	source := &erroringDetailServerSource{stubServerStore: store, err: errors.New("database is locked")}
	h, cookie := newServersTestHandler(t, RoleStandalone, source, &stubServerWriter{})

	body, status := serversGetStatus(t, h, cookie, "/ui/servers/new/result?server=srv-new")
	if status != http.StatusInternalServerError {
		t.Fatalf("读取失败应返回 500，实际 %d：%s", status, body)
	}
	if !strings.Contains(body, "读取服务器详情失败") {
		t.Fatalf("读取失败应向 owner 说明详情读取失败：%s", body)
	}
	if source.calls != 1 {
		t.Fatalf("读取失败不应重试，实际调用 %d 次", source.calls)
	}
}

func TestServerAddResultUsesStoredFactsInsteadOfQueryParameters(t *testing.T) {
	store := newStubServerStore()
	store.details["srv-new"] = &ServerDetailView{
		ID: "srv-new", Name: "web", User: "deploy", Host: "192.0.2.20", Port: 22,
		DeviceName: "真实设备", HasDevice: true,
	}
	tester := &stubServerConnectionTester{status: &ServerConnectionTestStatus{
		ID: "task-1", State: ServerConnectionTestFailed, ServerID: "srv-new",
	}}
	h, cookie := newServersTestHandlerWithConnectionTester(t, RoleStandalone, store, &stubServerWriter{}, tester)

	page := serversGet(t, h, cookie, "/ui/servers/new/result?server=srv-new&task=task-1"+
		"&device="+url.QueryEscape("网址声称的设备")+
		"&profiles="+url.QueryEscape("网址声称的profile")+
		"&anchor=copy&anchor_from="+url.QueryEscape("网址声称的来源"))
	for _, banned := range []string{"网址声称的设备", "网址声称的profile", "网址声称的来源", "已从服务器条目"} {
		if strings.Contains(page, banned) {
			t.Fatalf("结果页不得把网址查询参数当事实显示，包含 %q：%s", banned, page)
		}
	}
	if !strings.Contains(page, "真实设备") || !strings.Contains(page, "未授权") {
		t.Fatalf("结果页应显示存储中的设备与授权状态：%s", page)
	}
}

// TestServerWizardTestRunningReusesSharedResult 校验进行中状态：新增向导
// 测试页复用共享结果片段与视觉状态，由外层页面区域负责轮询，不出现旧的
// 「状态：未验证」表述。
func TestServerWizardTestRunningReusesSharedResult(t *testing.T) {
	store := newStubServerStore()
	store.details["srv-new"] = &ServerDetailView{ID: "srv-new", Name: "wizard", User: "deploy", Host: "192.0.2.20", Port: 22}
	tester := &stubServerConnectionTester{}
	h, cookie := newServersTestHandlerWithConnectionTester(t, RoleStandalone, store, &stubServerWriter{}, tester)

	page := serversGet(t, h, cookie, "/ui/servers/new/test?server=srv-new&task=task-1")
	for _, want := range []string{
		`class="connection-test-result is-running"`,
		"正在连接…",
		`hx-get="/ui/servers/new/test?server=srv-new&amp;task=task-1"`,
		`hx-trigger="every 1s"`,
		"返回修改连接信息",
		"先看服务器详情",
		"shell 未验证，条目保持默认 POSIX。",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("进行中的新增向导测试页缺少 %q：\n%s", want, page)
		}
	}
	for _, banned := range []string{
		"状态：未验证", "已验证状态等待测试完成", `hx-target="#server-connection-test-live"`,
	} {
		if strings.Contains(page, banned) {
			t.Fatalf("进行中的新增向导测试页不得包含 %q：\n%s", banned, page)
		}
	}
}

// TestServerWizardTestSuccessReusesSharedResult 校验成功状态：共享片段显
// 示实际使用的地址、网络与耗时，成功后才能继续，shell 未判定与首次锚定说
// 明仍保留。
func TestServerWizardTestSuccessReusesSharedResult(t *testing.T) {
	store := newStubServerStore()
	store.details["srv-new"] = &ServerDetailView{ID: "srv-new", Name: "wizard", User: "deploy", Host: "192.0.2.20", Port: 22}
	tester := &stubServerConnectionTester{status: &ServerConnectionTestStatus{
		ID: "task-1", State: ServerConnectionTestSucceeded, ServerID: "srv-new", ProbeShell: true,
		AnchorFingerprint: "SHA256:test-host-key", AnchorResult: "首次连接已记录主机密钥锚",
		ShellNotice: "无法判定 shell 类型，未猜测；请人工确认后修改标注。",
		Result: &ServerConnectionTestResultView{
			State: ServerConnectionTestSucceeded, Succeeded: true,
			CompletedAt:        time.Unix(1_782_000_000, 0),
			SuccessAddress:     "192.0.2.20",
			SuccessNetworkName: "家A局域网",
			SuccessSource:      models.DialAddressSourcePhysical,
			Duration:           42 * time.Millisecond,
			Attempts: []ServerConnectionTestAttemptView{{
				NetworkName: "家A局域网", Address: "192.0.2.20", Source: models.DialAddressSourcePhysical,
				OK: true, Duration: 40 * time.Millisecond,
			}},
		},
	}}
	h, cookie := newServersTestHandlerWithConnectionTester(t, RoleStandalone, store, &stubServerWriter{}, tester)

	page := serversGet(t, h, cookie, "/ui/servers/new/test?server=srv-new&task=task-1")
	for _, want := range []string{
		`class="connection-test-result is-succeeded"`,
		"已连上 · 用的地址 192.0.2.20（家A局域网）· 耗时 42 毫秒",
		"继续选择地点",
		"SHA256:test-host-key",
		"首次连接已记录主机密钥锚",
		"无法判定 shell 类型，未猜测；请人工确认后修改标注。",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("成功的新增向导测试页缺少 %q：\n%s", want, page)
		}
	}
	for _, banned := range []string{"状态：未验证", "已验证状态等待测试完成", "返回修改连接信息"} {
		if strings.Contains(page, banned) {
			t.Fatalf("成功的新增向导测试页不得包含 %q：\n%s", banned, page)
		}
	}
}

// TestServerWizardTestFailuresReusesSharedResult 校验认证失败、端口不通与
// 超时：共享片段显示白话原因和可展开的候选地址，失败后仍能返回修改连接信
// 息，锚复制来源与冲突提示不丢失。
func TestServerWizardTestFailuresReusesSharedResult(t *testing.T) {
	store := newStubServerStore()
	store.details["srv-new"] = &ServerDetailView{ID: "srv-new", Name: "wizard", User: "deploy", Host: "192.0.2.20", Port: 22}
	for _, item := range []struct {
		name  string
		label string
	}{
		{name: "认证失败", label: "认证失败"},
		{name: "端口不通", label: "端口不通"},
		{name: "超时", label: "超时"},
	} {
		tester := &stubServerConnectionTester{status: &ServerConnectionTestStatus{
			ID: "task-1", State: ServerConnectionTestFailed, ServerID: "srv-new", ProbeShell: true,
			AnchorCopied: true, AnchorCopyFromServerName: "sibling", AnchorCopyConflict: true,
			Result: &ServerConnectionTestResultView{
				State: ServerConnectionTestFailed, FailureLabel: item.label,
				Attempts: []ServerConnectionTestAttemptView{{
					NetworkName: "家A局域网", Address: "192.0.2.20",
					Source: models.DialAddressSourcePhysical, FailureLabel: item.label,
					Duration: 23 * time.Millisecond,
				}},
			},
		}}
		h, cookie := newServersTestHandlerWithConnectionTester(t, RoleStandalone, store, &stubServerWriter{}, tester)

		page := serversGet(t, h, cookie, "/ui/servers/new/test?server=srv-new&task=task-1")
		for _, want := range []string{
			`class="connection-test-result is-failed"`,
			"连不上</strong> · " + item.label,
			"展开看每个候选地址的结果",
			`aria-expanded="false"`,
			"家A局域网",
			"192.0.2.20",
			item.label + " · 耗时 23 毫秒",
			"返回修改连接信息",
			"已从服务器条目 sibling 复制同端口主机密钥锚",
			"同一设备、同一端口的其他服务器条目主机密钥锚不一致，未复制；请手工确认后锚定。",
			"shell 未验证，条目保持默认 POSIX。",
		} {
			if !strings.Contains(page, want) {
				t.Fatalf("失败原因 %s 的新增向导测试页缺少 %q：\n%s", item.name, want, page)
			}
		}
		for _, banned := range []string{"状态：未验证", "已验证状态等待测试完成", "继续选择地点"} {
			if strings.Contains(page, banned) {
				t.Fatalf("失败原因 %s 的新增向导测试页不得包含 %q：\n%s", item.name, banned, page)
			}
		}
	}
}
