package webui

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type stubInitializer struct {
	snapshot      InitializationSnapshot
	result        InitializationResult
	initializeErr error

	selectedRole    InitializationRole
	initializedRole InitializationRole
}

func (s *stubInitializer) ReadSnapshot() (InitializationSnapshot, error) {
	return s.snapshot, nil
}

func (s *stubInitializer) SelectRoleWithoutVault(role InitializationRole) error {
	s.selectedRole = role
	return nil
}

func (s *stubInitializer) Initialize(role InitializationRole) (InitializationResult, error) {
	s.initializedRole = role
	return s.result, s.initializeErr
}

func newInitializationWizardForTest(t *testing.T, initializer Initializer) (http.Handler, Startup) {
	t.Helper()
	handler, startup, err := NewInitializationWizard(Config{
		Role:    RoleStandalone,
		Servers: &stubServerSource{rows: []ServerRow{{Name: "正式服务器", User: "root", Host: "192.0.2.10", Port: 22}}},
		Access:  &stubAccessStore{},
		Port:    18080,
	}, initializer)
	if err != nil {
		t.Fatalf("构造初始化向导失败：%v", err)
	}
	return handler, startup
}

func wizardPage(t *testing.T, handler http.Handler, cookie *http.Cookie, path string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = "127.0.0.1:18080"
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	resp := rec.Result()
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("访问 %s 状态码 = %d，页面：\n%s", path, resp.StatusCode, body)
	}
	return string(body)
}

func wizardPost(t *testing.T, handler http.Handler, cookie *http.Cookie, path, csrfToken string, form url.Values) *http.Response {
	t.Helper()
	if form == nil {
		form = url.Values{}
	}
	form.Set("csrf_token", csrfToken)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Host = "127.0.0.1:18080"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:18080")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Result()
}

func TestInitializationWizardChoosesStandaloneAndSwitchesToFormalPage(t *testing.T) {
	initializer := &stubInitializer{result: InitializationResult{MasterKeyPath: "/tmp/master.key.plain"}}
	handler, startup := newInitializationWizardForTest(t, initializer)

	start := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	if start.StatusCode != http.StatusSeeOther || start.Header.Get("Location") != "/ui/" {
		start.Body.Close()
		t.Fatalf("启动令牌应换会话并进入向导首页，状态码=%d 跳转=%q", start.StatusCode, start.Header.Get("Location"))
	}
	cookie := start.Cookies()[0]
	start.Body.Close()

	choice := wizardPage(t, handler, cookie, "/ui/")
	for _, want := range []string{"选择本机角色", "单机", "权威端", "客户端", "尚未实现"} {
		if !strings.Contains(choice, want) {
			t.Fatalf("选角色页应包含 %q：\n%s", want, choice)
		}
	}
	if strings.Contains(choice, "正式服务器") {
		t.Fatalf("向导模式不应显示正式页面服务器数据：\n%s", choice)
	}

	selected := wizardPost(t, handler, cookie, "/ui/init/role", hiddenInputValue(t, choice, "csrf_token"), url.Values{"role": {"standalone"}})
	selected.Body.Close()
	if selected.StatusCode != http.StatusSeeOther || selected.Header.Get("Location") != "/ui/init/standalone" {
		t.Fatalf("选单机应进入确认页，状态码=%d 跳转=%q", selected.StatusCode, selected.Header.Get("Location"))
	}
	if initializer.selectedRole != InitializationRoleStandalone {
		t.Fatalf("应先写入单机角色，实际 %q", initializer.selectedRole)
	}

	confirmation := wizardPage(t, handler, cookie, "/ui/init/standalone")
	if !strings.Contains(confirmation, "创建 vault") {
		t.Fatalf("确认页应说明创建 vault：\n%s", confirmation)
	}

	created := wizardPost(t, handler, cookie, "/ui/init/standalone/create", hiddenInputValue(t, confirmation, "csrf_token"), nil)
	createdBody, _ := io.ReadAll(created.Body)
	created.Body.Close()
	if created.StatusCode != http.StatusOK {
		t.Fatalf("创建 vault 后应显示完成提示，状态码=%d，页面：\n%s", created.StatusCode, createdBody)
	}
	for _, want := range []string{initializer.result.MasterKeyPath, "请备份此文件", "进入单机正式页面"} {
		if !strings.Contains(string(createdBody), want) {
			t.Fatalf("创建 vault 完成页应包含 %q：\n%s", want, createdBody)
		}
	}
	if initializer.initializedRole != InitializationRoleStandalone {
		t.Fatalf("应初始化为单机，实际 %q", initializer.initializedRole)
	}

	finished := wizardPost(t, handler, cookie, "/ui/init/standalone/finish", hiddenInputValue(t, string(createdBody), "csrf_token"), nil)
	finished.Body.Close()
	if finished.StatusCode != http.StatusSeeOther || finished.Header.Get("Location") != "/ui/" {
		t.Fatalf("完成页应跳转正式首页，状态码=%d 跳转=%q", finished.StatusCode, finished.Header.Get("Location"))
	}

	formal := wizardPage(t, handler, cookie, "/ui/")
	if !strings.Contains(formal, "正式服务器") || strings.Contains(formal, "选择本机角色") {
		t.Fatalf("会话应延续到单机正式首页：\n%s", formal)
	}
}

func TestInitializationWizardReusesLoopbackSecurityAndLimitsRoutes(t *testing.T) {
	handler, startup := newInitializationWizardForTest(t, &stubInitializer{})

	badHost := exchangeStartupTokenWithHost(t, handler, "127.0.0.1.evil.example:18080", startup.URL("127.0.0.1:18080"))
	badHost.Body.Close()
	if badHost.StatusCode != http.StatusMisdirectedRequest || len(badHost.Cookies()) != 0 {
		t.Fatalf("非回环 Host 不得换取会话，状态码=%d cookie=%d", badHost.StatusCode, len(badHost.Cookies()))
	}

	start := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	start.Body.Close()
	if start.StatusCode != http.StatusSeeOther || len(start.Cookies()) != 1 {
		t.Fatalf("启动令牌应换会话，状态码=%d cookie=%d", start.StatusCode, len(start.Cookies()))
	}
	reused := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	reused.Body.Close()
	if reused.StatusCode != http.StatusForbidden {
		t.Fatalf("启动令牌应只能使用一次，第二次状态码=%d", reused.StatusCode)
	}
	cookie := start.Cookies()[0]

	choiceRequest := httptest.NewRequest(http.MethodGet, "/ui/", nil)
	choiceRequest.Host = "127.0.0.1:18080"
	choiceRequest.AddCookie(cookie)
	choiceRecorder := httptest.NewRecorder()
	handler.ServeHTTP(choiceRecorder, choiceRequest)
	choiceResponse := choiceRecorder.Result()
	choiceBody, _ := io.ReadAll(choiceResponse.Body)
	choiceResponse.Body.Close()
	if choiceResponse.StatusCode != http.StatusOK {
		t.Fatalf("向导首页状态码 = %d，页面：\n%s", choiceResponse.StatusCode, choiceBody)
	}
	if got := choiceResponse.Header.Get("Content-Security-Policy"); got != contentSecurityPolicy {
		t.Fatalf("初始化向导应沿用内容安全策略，实际：%q", got)
	}
	csrfToken := hiddenInputValue(t, string(choiceBody), "csrf_token")

	missingOrigin := httptest.NewRequest(http.MethodPost, "/ui/init/role", strings.NewReader(url.Values{
		"csrf_token": {csrfToken}, "role": {"standalone"},
	}.Encode()))
	missingOrigin.Host = "127.0.0.1:18080"
	missingOrigin.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	missingOrigin.AddCookie(cookie)
	missingOriginRecorder := httptest.NewRecorder()
	handler.ServeHTTP(missingOriginRecorder, missingOrigin)
	missingOriginResponse := missingOriginRecorder.Result()
	missingOriginResponse.Body.Close()
	if missingOriginResponse.StatusCode != http.StatusForbidden {
		t.Fatalf("缺少 Origin 的向导表单状态码 = %d，应为 403", missingOriginResponse.StatusCode)
	}

	wrongToken := wizardPost(t, handler, cookie, "/ui/init/role", "wrong-token", url.Values{"role": {"standalone"}})
	wrongToken.Body.Close()
	if wrongToken.StatusCode != http.StatusForbidden {
		t.Fatalf("防跨站请求伪造令牌错误时状态码 = %d，应为 403", wrongToken.StatusCode)
	}

	for _, path := range []string{"/ui/access/profiles", "/ui/ops/", "/ui/servers/import", "/ui/secrets/"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Host = "127.0.0.1:18080"
		request.AddCookie(cookie)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		resp := recorder.Result()
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("向导模式不应注册正式路由 %s，状态码 = %d", path, resp.StatusCode)
		}
	}
}

func TestInitializationWizardShowsVaultCreationFailureAndAdvice(t *testing.T) {
	initializer := &stubInitializer{initializeErr: errors.New("磁盘没有写入权限")}
	handler, startup := newInitializationWizardForTest(t, initializer)
	start := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookie := start.Cookies()[0]
	start.Body.Close()

	choice := wizardPage(t, handler, cookie, "/ui/")
	selected := wizardPost(t, handler, cookie, "/ui/init/role", hiddenInputValue(t, choice, "csrf_token"), url.Values{"role": {"standalone"}})
	selected.Body.Close()
	confirmation := wizardPage(t, handler, cookie, "/ui/init/standalone")
	created := wizardPost(t, handler, cookie, "/ui/init/standalone/create", hiddenInputValue(t, confirmation, "csrf_token"), nil)
	body, _ := io.ReadAll(created.Body)
	created.Body.Close()

	if created.StatusCode != http.StatusInternalServerError {
		t.Fatalf("创建失败状态码 = %d，页面：\n%s", created.StatusCode, body)
	}
	for _, want := range []string{"磁盘没有写入权限", "sshmgr doctor", "创建 vault"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("创建失败页应包含 %q：\n%s", want, body)
		}
	}
}

func TestInitializationWizardResumesAtVaultCreation(t *testing.T) {
	initializer := &stubInitializer{
		snapshot: InitializationSnapshot{Role: InitializationRoleStandalone, Step: InitializationStepCreateVault},
	}
	handler, startup := newInitializationWizardForTest(t, initializer)
	start := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookie := start.Cookies()[0]
	start.Body.Close()

	root := wizardGet(t, handler, cookie, "/ui/")
	root.Body.Close()
	if root.StatusCode != http.StatusSeeOther || root.Header.Get("Location") != "/ui/init/standalone" {
		t.Fatalf("续做应从创建 vault 开始，状态码=%d 跳转=%q", root.StatusCode, root.Header.Get("Location"))
	}
	if initializer.selectedRole != "" {
		t.Fatalf("续做不应重写角色，实际调用了选角色：%q", initializer.selectedRole)
	}

	confirmation := wizardPage(t, handler, cookie, "/ui/init/standalone")
	if !strings.Contains(confirmation, "创建 vault") {
		t.Fatalf("续做页应是创建 vault 确认页：\n%s", confirmation)
	}
}

func wizardGet(t *testing.T, handler http.Handler, cookie *http.Cookie, path string) *http.Response {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Host = "127.0.0.1:18080"
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder.Result()
}

func TestInitializationWizardRolePlaceholdersDoNotChangeMachine(t *testing.T) {
	initializer := &stubInitializer{}
	handler, startup := newInitializationWizardForTest(t, initializer)
	start := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookie := start.Cookies()[0]
	start.Body.Close()
	choice := wizardPage(t, handler, cookie, "/ui/")

	for _, testCase := range []struct {
		role string
		path string
		name string
	}{
		{role: "server", path: "/ui/init/server", name: "权威端"},
		{role: "client", path: "/ui/init/client", name: "客户端"},
	} {
		selected := wizardPost(t, handler, cookie, "/ui/init/role", hiddenInputValue(t, choice, "csrf_token"), url.Values{"role": {testCase.role}})
		selected.Body.Close()
		if selected.StatusCode != http.StatusSeeOther || selected.Header.Get("Location") != testCase.path {
			t.Fatalf("%s 占位入口跳转错误，状态码=%d 地址=%q", testCase.name, selected.StatusCode, selected.Header.Get("Location"))
		}
		page := wizardPage(t, handler, cookie, testCase.path)
		if !strings.Contains(page, testCase.name) || !strings.Contains(page, "尚未实现") {
			t.Fatalf("%s 占位页应说明尚未实现：\n%s", testCase.name, page)
		}
	}

	if initializer.selectedRole != "" || initializer.initializedRole != "" {
		t.Fatalf("占位选项不得改写本机状态：选角色=%q 初始化=%q", initializer.selectedRole, initializer.initializedRole)
	}
}
