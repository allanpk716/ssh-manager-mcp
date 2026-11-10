package webui

import (
	"errors"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type stubInitializer struct {
	snapshot      InitializationSnapshot
	result        InitializationResult
	initializeErr error
	currentRole   InitializationRole
	vaultReady    bool

	selectedRole    InitializationRole
	initializedRole InitializationRole
	password        string
	passwordConfirm string
	passwordSource  InitializationWriteSource
	serveRunning    bool
	serveAddresses  []string
	servePort       int
	needsManual     bool
}

func (s *stubInitializer) ReadSnapshot() (InitializationSnapshot, error) {
	role := s.currentRole
	if role == "" {
		role = s.snapshot.Role
	}
	return InitializationSnapshot{
		Role: role, Step: s.snapshot.Step, VaultReady: s.vaultReady,
	}, nil
}

func (s *stubInitializer) SelectRoleWithoutVault(role InitializationRole) error {
	s.selectedRole = role
	s.currentRole = role
	return nil
}

func (s *stubInitializer) Initialize(role InitializationRole) (InitializationResult, error) {
	s.initializedRole = role
	s.currentRole = role
	if s.initializeErr == nil {
		s.vaultReady = true
	}
	return s.result, s.initializeErr
}

func (s *stubInitializer) ValidateAdminPassword(password, confirmation []byte) error {
	if string(password) != string(confirmation) {
		return errors.New("两次输入的管理员口令不一致")
	}
	if len(password) == 0 {
		return errors.New("管理员口令不能为空")
	}
	return nil
}

func (s *stubInitializer) SetAdminPassword(password, confirmation []byte, source InitializationWriteSource) (int64, error) {
	s.password = string(password)
	s.passwordConfirm = string(confirmation)
	s.passwordSource = source
	return 1, nil
}

func (s *stubInitializer) ProbeServeWebUI() (ServeWebUIProbe, error) {
	return ServeWebUIProbe{
		Running:   s.serveRunning,
		Addresses: s.serveAddresses, Port: s.servePort, NeedsManualListen: s.needsManual,
	}, nil
}

func newInitializationWizardForTest(
	t *testing.T,
	initializer Initializer,
	configure ...func(*Config),
) (http.Handler, Startup) {
	return newInitializationWizardForTestWithConfig(t, initializer, func(config *Config) {
		for _, configureConfig := range configure {
			configureConfig(config)
		}
	})
}

func newInitializationWizardForTestWithConfig(t *testing.T, initializer Initializer, change func(*Config)) (http.Handler, Startup) {
	t.Helper()
	config := Config{
		Role:    RoleStandalone,
		Servers: &stubServerSource{rows: []ServerRow{{Name: "正式服务器", User: "root", Host: "192.0.2.10", Port: 22}}},
		Access:  &stubAccessStore{},
		Port:    18080,
	}
	if change != nil {
		change(&config)
	}
	handler, startup, err := NewInitializationWizard(config, initializer)
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
	for _, want := range []string{"选择本机角色", "单机", "权威端", "客户端"} {
		if !strings.Contains(choice, want) {
			t.Fatalf("选角色页应包含 %q：\n%s", want, choice)
		}
	}
	if strings.Contains(choice, "权威端与客户端的后续步骤尚未实现") {
		t.Fatalf("权威端初始化已提供后续步骤，选角色页不应再整体提示尚未实现：\n%s", choice)
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
	for _, want := range []string{"磁盘没有写入权限", "<code>sshmgr doctor</code>", "创建 vault"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("创建失败页应包含 %q：\n%s", want, body)
		}
	}
	if strings.Contains(string(body), "`") {
		t.Fatalf("创建失败页不应把命令的反引号原样显示：\n%s", body)
	}
}

func TestInitializationWizardStandaloneFinishRequiresCreatedVault(t *testing.T) {
	initializer := &stubInitializer{}
	handler, startup := newInitializationWizardForTest(t, initializer)
	start := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookie := start.Cookies()[0]
	start.Body.Close()

	choice := wizardPage(t, handler, cookie, "/ui/")
	finished := wizardPost(t, handler, cookie, "/ui/init/standalone/finish", hiddenInputValue(t, choice, "csrf_token"), nil)
	finished.Body.Close()
	if finished.StatusCode != http.StatusSeeOther || finished.Header.Get("Location") != "/ui/init/standalone" {
		t.Fatalf("未建 vault 时提交完成应回到建 vault 步骤，状态码=%d 跳转=%q",
			finished.StatusCode, finished.Header.Get("Location"))
	}
	if initializer.initializedRole != "" {
		t.Fatalf("未建 vault 时不得切换正式页面，实际调用了初始化：%q", initializer.initializedRole)
	}

	root := wizardGet(t, handler, cookie, "/ui/")
	rootBody, _ := io.ReadAll(root.Body)
	root.Body.Close()
	if !strings.Contains(string(rootBody), "选择本机角色") {
		t.Fatalf("未建 vault 时应停留在向导，页面：\n%s", rootBody)
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

func TestInitializationWizardChoosesServerAndStartsVaultStep(t *testing.T) {
	initializer := &stubInitializer{}
	handler, startup := newInitializationWizardForTest(t, initializer)
	start := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookie := start.Cookies()[0]
	start.Body.Close()

	choice := wizardPage(t, handler, cookie, "/ui/")
	selected := wizardPost(t, handler, cookie, "/ui/init/role", hiddenInputValue(t, choice, "csrf_token"), url.Values{"role": {"server"}})
	selected.Body.Close()
	if selected.StatusCode != http.StatusSeeOther || selected.Header.Get("Location") != "/ui/init/server/vault" {
		t.Fatalf("选权威端应进入建 vault 步骤，状态码=%d 跳转=%q", selected.StatusCode, selected.Header.Get("Location"))
	}
	if initializer.selectedRole != InitializationRoleServer {
		t.Fatalf("选权威端应先写入 setup_complete 为假的角色文件，实际 %q", initializer.selectedRole)
	}

	confirmation := wizardPage(t, handler, cookie, "/ui/init/server/vault")
	for _, want := range []string{"建 vault", "设置管理员口令", "安装 serve"} {
		if !strings.Contains(confirmation, want) {
			t.Fatalf("权威端向导应显示三步步骤条，缺少 %q：\n%s", want, confirmation)
		}
	}
}

func TestInitializationWizardResumesServerAtVaultCreation(t *testing.T) {
	initializer := &stubInitializer{
		snapshot: InitializationSnapshot{Role: InitializationRoleServer, Step: InitializationStepCreateVault},
	}
	handler, startup := newInitializationWizardForTest(t, initializer)
	start := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookie := start.Cookies()[0]
	start.Body.Close()

	root := wizardGet(t, handler, cookie, "/ui/")
	root.Body.Close()
	if root.StatusCode != http.StatusSeeOther || root.Header.Get("Location") != "/ui/init/server/vault" {
		t.Fatalf("权威端续做应从建 vault 开始，状态码=%d 跳转=%q", root.StatusCode, root.Header.Get("Location"))
	}
	if initializer.selectedRole != "" {
		t.Fatalf("权威端续做不应重写角色，实际调用了选角色：%q", initializer.selectedRole)
	}
}

func TestInitializationWizardServerCreatesVaultAndMovesToPasswordStep(t *testing.T) {
	initializer := &stubInitializer{result: InitializationResult{MasterKeyPath: "/tmp/authority.key"}}
	handler, startup := newInitializationWizardForTest(t, initializer)
	start := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookie := start.Cookies()[0]
	start.Body.Close()

	vaultPage := wizardPage(t, handler, cookie, "/ui/init/server/vault")
	created := wizardPost(t, handler, cookie, "/ui/init/server/vault/create", hiddenInputValue(t, vaultPage, "csrf_token"), nil)
	created.Body.Close()
	if created.StatusCode != http.StatusSeeOther || created.Header.Get("Location") != "/ui/init/server/password" {
		t.Fatalf("权威端建 vault 后应进入设口令步骤，状态码=%d 跳转=%q", created.StatusCode, created.Header.Get("Location"))
	}
	if initializer.initializedRole != InitializationRoleServer {
		t.Fatalf("应初始化为权威端，实际 %q", initializer.initializedRole)
	}

	passwordPage := wizardPage(t, handler, cookie, "/ui/init/server/password")
	for _, want := range []string{"设置管理员口令", "输入管理员口令", "再输入一遍", "sshmgr serve admin set"} {
		if !strings.Contains(passwordPage, want) {
			t.Fatalf("设口令页应包含 %q：\n%s", want, passwordPage)
		}
	}
}

func TestInitializationWizardResumesServerAtLaterSteps(t *testing.T) {
	tests := []struct {
		name string
		step InitializationStep
		path string
	}{
		{
			name: "缺管理员口令时从第二步续做",
			step: InitializationStepSetAdminPassword,
			path: "/ui/init/server/password",
		},
		{
			name: "口令已设但 serve 未运行时从第三步续做",
			step: InitializationStepInstallServe,
			path: "/ui/init/server/install",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			initializer := &stubInitializer{
				snapshot: InitializationSnapshot{Role: InitializationRoleServer, Step: test.step},
			}
			handler, startup := newInitializationWizardForTest(t, initializer)
			start := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
			cookie := start.Cookies()[0]
			start.Body.Close()

			root := wizardGet(t, handler, cookie, "/ui/")
			root.Body.Close()
			if root.StatusCode != http.StatusSeeOther || root.Header.Get("Location") != test.path {
				t.Fatalf("权威端续做跳转错误，状态码=%d 地址=%q，应为 %q", root.StatusCode, root.Header.Get("Location"), test.path)
			}
			if initializer.selectedRole != "" || initializer.initializedRole != "" {
				t.Fatalf("续做不得重写角色或 vault：选角色=%q 初始化=%q", initializer.selectedRole, initializer.initializedRole)
			}
		})
	}
}

func TestInitializationWizardRefusesServerWritesWhenServeIsRunning(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{name: "建 vault 前发现 serve 已运行", path: "/ui/init/server/vault/create"},
		{name: "设置管理员口令前发现 serve 已运行", path: "/ui/init/server/password/set"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			initializer := &stubInitializer{serveRunning: true}
			handler, startup := newInitializationWizardForTest(t, initializer)
			start := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
			cookie := start.Cookies()[0]
			start.Body.Close()
			pagePath := "/ui/init/server/vault"
			if test.path == "/ui/init/server/password/set" {
				pagePath = "/ui/init/server/password"
			}
			page := wizardPage(t, handler, cookie, pagePath)
			refused := wizardPost(t, handler, cookie, test.path, hiddenInputValue(t, page, "csrf_token"), url.Values{
				"password": {"authority-password"}, "password_confirm": {"authority-password"},
			})
			body, _ := io.ReadAll(refused.Body)
			refused.Body.Close()
			if refused.StatusCode != http.StatusConflict {
				t.Fatalf("serve 已运行时应拒绝写入，状态码=%d 页面：\n%s", refused.StatusCode, body)
			}
			for _, want := range []string{
				"serve 已在运行", "请到正式 Web UI 操作", "<code>sshmgr serve admin set</code>",
			} {
				if !strings.Contains(string(body), want) {
					t.Fatalf("拒绝写入页应包含 %q：\n%s", want, body)
				}
			}
			if strings.Contains(string(body), "`") {
				t.Fatalf("拒绝写入页不应把命令的反引号原样显示：\n%s", body)
			}
			if initializer.initializedRole != "" || initializer.password != "" {
				t.Fatalf("serve 已运行时不得写入：初始化=%q 口令=%q", initializer.initializedRole, initializer.password)
			}
		})
	}
}

func TestInitializationWizardInstallPageRendersOperatingSystemCommand(t *testing.T) {
	tests := []struct {
		name            string
		operatingSystem string
		executablePath  string
		wantCommand     string
		wantNotice      string
	}{
		{
			name:            "Linux 显示 sudo 命令",
			operatingSystem: "linux",
			executablePath:  "/usr/local/bin/sshmgr",
			wantCommand:     "sudo '/usr/local/bin/sshmgr' serve install --addr 0.0.0.0:7878",
			wantNotice:      "需要管理员权限",
		},
		{
			name:            "macOS 显示 sudo 命令",
			operatingSystem: "darwin",
			executablePath:  "/usr/local/bin/sshmgr",
			wantCommand:     "sudo '/usr/local/bin/sshmgr' serve install --addr 0.0.0.0:7878",
			wantNotice:      "需要管理员权限",
		},
		{
			name:            "Windows 显示管理员 PowerShell 命令",
			operatingSystem: "windows",
			executablePath:  `C:\Program Files\sshmgr.exe`,
			wantCommand:     `& "C:\Program Files\sshmgr.exe" serve install --addr 0.0.0.0:7878`,
			wantNotice:      "在管理员 PowerShell 中运行",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, startup := newInitializationWizardForTestWithConfig(t, &stubInitializer{
				snapshot: InitializationSnapshot{Role: InitializationRoleServer, Step: InitializationStepInstallServe},
			}, func(config *Config) {
				config.InitializationOperatingSystem = test.operatingSystem
				config.InitializationExecutablePath = test.executablePath
			})
			start := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
			cookie := start.Cookies()[0]
			start.Body.Close()

			page := html.UnescapeString(wizardPage(t, handler, cookie, "/ui/init/server/install"))
			for _, want := range []string{
				test.wantCommand, test.wantNotice,
				`id="serve-install-command"`, `data-copy-target="serve-install-command"`,
				`hx-get="/ui/init/server/probe"`, `hx-trigger="every 2s"`,
			} {
				if !strings.Contains(page, want) {
					t.Fatalf("安装页应包含 %q：\n%s", want, page)
				}
			}
			if test.operatingSystem == "windows" && strings.Contains(page, "sudo ") {
				t.Fatalf("Windows 安装命令不应使用 sudo：\n%s", page)
			}
		})
	}
}

func TestInitializationWizardPollsServeAndCompletesWithOrderedAddresses(t *testing.T) {
	completed := make(chan InitializationCompletion, 1)
	shutdown := make(chan struct{}, 1)
	initializer := &stubInitializer{
		serveAddresses: []string{"127.0.0.1", "192.0.2.10"},
		servePort:      7879,
	}
	handler, startup := newInitializationWizardForTestWithConfig(t, initializer, func(config *Config) {
		config.InitializationComplete = func(completion InitializationCompletion) {
			completed <- completion
		}
		config.RequestShutdown = func(ShutdownReason) { shutdown <- struct{}{} }
	})
	start := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookie := start.Cookies()[0]
	start.Body.Close()

	waiting := wizardProbe(t, handler, cookie, false)
	waiting.Body.Close()
	if waiting.StatusCode != http.StatusNoContent {
		t.Fatalf("serve 未运行时轮询应保持页面，状态码=%d", waiting.StatusCode)
	}

	initializer.serveRunning = true
	detected := wizardProbe(t, handler, cookie, true)
	detected.Body.Close()
	if detected.StatusCode != http.StatusOK || detected.Header.Get("HX-Redirect") != "/ui/init/server/finish" {
		t.Fatalf("轮询发现 serve 后应进入完成页，状态码=%d 跳转=%q", detected.StatusCode, detected.Header.Get("HX-Redirect"))
	}

	finishPage := wizardPage(t, handler, cookie, "/ui/init/server/finish")
	if strings.Index(finishPage, "https://192.0.2.10:7879/ui/") > strings.Index(finishPage, "https://127.0.0.1:7879/ui/") {
		t.Fatalf("虚拟局域网地址应排在回环地址前：\n%s", finishPage)
	}
	if !strings.Contains(finishPage, "https://127.0.0.1:7879/ui/") || !strings.Contains(finishPage, "仅本机") {
		t.Fatalf("完成页应列出回环地址并注明仅本机：\n%s", finishPage)
	}

	select {
	case completion := <-completed:
		if completion.Port != 7879 ||
			len(completion.Addresses) != 2 ||
			completion.Addresses[0] != "192.0.2.10" ||
			completion.Addresses[1] != "127.0.0.1" {
			t.Fatalf("完成事件地址排序错误：%+v", completion)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("完成页写完后应通知初始化完成")
	}
	select {
	case <-shutdown:
	case <-time.After(2 * time.Second):
		t.Fatal("完成页写完后应触发优雅退出")
	}
}

func TestInitializationWizardHeadRequestsDoNotScheduleShutdown(t *testing.T) {
	shutdowns := make(chan struct{}, 2)
	handler, startup := newInitializationWizardForTestWithConfig(t, &stubInitializer{
		serveRunning:   true,
		serveAddresses: []string{"127.0.0.1"},
		servePort:      7879,
	}, func(config *Config) {
		config.RequestShutdown = func(ShutdownReason) { shutdowns <- struct{}{} }
		config.Sleep = func(time.Duration) {}
		config.InitializationComplete = func(InitializationCompletion) {}
	})
	start := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookie := start.Cookies()[0]
	start.Body.Close()

	for _, path := range []string{"/ui/init/server/finish", "/ui/init/client"} {
		request := httptest.NewRequest(http.MethodHead, path, nil)
		request.Host = "127.0.0.1:18080"
		request.AddCookie(cookie)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		response := recorder.Result()
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("HEAD %s 应渲染页面，状态码=%d", path, response.StatusCode)
		}
	}
	select {
	case <-shutdowns:
		t.Fatal("HEAD 请求只渲染页面，不应排程进程退出")
	case <-time.After(150 * time.Millisecond):
	}
}

func TestInitializationWizardSchedulesShutdownAndCompletionOnlyOnce(t *testing.T) {
	var completions, shutdowns atomic.Int32
	delays := make(chan time.Duration, 4)
	handler, startup := newInitializationWizardForTestWithConfig(t, &stubInitializer{
		serveRunning:   true,
		serveAddresses: []string{"127.0.0.1"},
		servePort:      7879,
	}, func(config *Config) {
		config.InitializationComplete = func(InitializationCompletion) { completions.Add(1) }
		config.RequestShutdown = func(ShutdownReason) { shutdowns.Add(1) }
		config.Sleep = func(delay time.Duration) { delays <- delay }
	})
	start := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookie := start.Cookies()[0]
	start.Body.Close()

	for i := 0; i < 2; i++ {
		response := wizardGet(t, handler, cookie, "/ui/init/server/finish")
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("第 %d 次打开完成页状态码=%d", i+1, response.StatusCode)
		}
	}
	waitForShutdownCount(t, &shutdowns, 1)
	select {
	case delay := <-delays:
		if delay <= 0 {
			t.Fatalf("完成页退出延迟应经注入的睡眠函数传入正数，实际 %v", delay)
		}
	default:
		t.Fatal("完成页退出延迟应经注入的睡眠函数传入，不应直接睡眠")
	}
	time.Sleep(150 * time.Millisecond)
	if got := completions.Load(); got != 1 {
		t.Fatalf("重复刷新完成页只应通知一次完成回调，实际 %d 次", got)
	}
	if got := shutdowns.Load(); got != 1 {
		t.Fatalf("重复刷新完成页只应排程一次退出，实际 %d 次", got)
	}
}

func TestInitializationWizardClientGuideSchedulesShutdownOnlyOnce(t *testing.T) {
	var shutdowns atomic.Int32
	handler, startup := newInitializationWizardForTestWithConfig(t, &stubInitializer{}, func(config *Config) {
		config.RequestShutdown = func(ShutdownReason) { shutdowns.Add(1) }
		config.Sleep = func(time.Duration) {}
	})
	start := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookie := start.Cookies()[0]
	start.Body.Close()

	for i := 0; i < 2; i++ {
		response := wizardGet(t, handler, cookie, "/ui/init/client")
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("第 %d 次打开客户端指引状态码=%d", i+1, response.StatusCode)
		}
	}
	waitForShutdownCount(t, &shutdowns, 1)
	time.Sleep(150 * time.Millisecond)
	if got := shutdowns.Load(); got != 1 {
		t.Fatalf("重复刷新客户端指引只应排程一次退出，实际 %d 次", got)
	}
}

func waitForShutdownCount(t *testing.T, counter *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if counter.Load() >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待进程退出排程超时，实际 %d 次，应为 %d 次", counter.Load(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func wizardProbe(t *testing.T, handler http.Handler, cookie *http.Cookie, htmxRequest bool) *http.Response {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/ui/init/server/probe", nil)
	request.Host = "127.0.0.1:18080"
	request.AddCookie(cookie)
	if htmxRequest {
		request.Header.Set("HX-Request", "true")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder.Result()
}

func TestInitializationWizardServerSetsAdminPasswordWithCommandLineRules(t *testing.T) {
	initializer := &stubInitializer{}
	handler, startup := newInitializationWizardForTest(t, initializer)
	start := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookie := start.Cookies()[0]
	start.Body.Close()

	passwordPage := wizardPage(t, handler, cookie, "/ui/init/server/password")
	mismatched := wizardPost(t, handler, cookie, "/ui/init/server/password/set", hiddenInputValue(t, passwordPage, "csrf_token"), url.Values{
		"password": {"first-password"}, "password_confirm": {"second-password"},
	})
	mismatchedBody, _ := io.ReadAll(mismatched.Body)
	mismatched.Body.Close()
	if mismatched.StatusCode != http.StatusBadRequest || !strings.Contains(string(mismatchedBody), "两次输入的管理员口令不一致") {
		t.Fatalf("两次输入不一致应沿用命令行规则，状态码=%d 页面：\n%s", mismatched.StatusCode, mismatchedBody)
	}
	if initializer.password != "" {
		t.Fatalf("两次输入不一致时不应写入口令，实际调用：%q", initializer.password)
	}

	passwordPage = wizardPage(t, handler, cookie, "/ui/init/server/password")
	set := wizardPost(t, handler, cookie, "/ui/init/server/password/set", hiddenInputValue(t, passwordPage, "csrf_token"), url.Values{
		"password": {"authority-password"}, "password_confirm": {"authority-password"},
	})
	set.Body.Close()
	if set.StatusCode != http.StatusSeeOther || set.Header.Get("Location") != "/ui/init/server/install" {
		t.Fatalf("设置管理员口令后应进入安装步骤，状态码=%d 跳转=%q", set.StatusCode, set.Header.Get("Location"))
	}
	if initializer.password != "authority-password" || initializer.passwordConfirm != "authority-password" {
		t.Fatalf("应把两次输入交给初始化模块，第一遍=%q 第二遍=%q", initializer.password, initializer.passwordConfirm)
	}
	if initializer.passwordSource.RemoteAddress == "" {
		t.Fatal("写管理员口令应记录来源地址")
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

func TestInitializationWizardServerPasswordPageRejectsPostWithPageWording(t *testing.T) {
	handler, startup := newInitializationWizardForTest(t, &stubInitializer{})
	start := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookie := start.Cookies()[0]
	start.Body.Close()
	choice := wizardPage(t, handler, cookie, "/ui/")

	response := wizardPost(t, handler, cookie, "/ui/init/server/password", hiddenInputValue(t, choice, "csrf_token"), nil)
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("设口令页用 POST 访问状态码=%d，应为 405", response.StatusCode)
	}
	if !strings.Contains(string(body), "设置管理员口令页只接受 GET 请求。") {
		t.Fatalf("设口令页的方法错误说明应说明只接受 GET，页面：\n%s", body)
	}
}

func TestInitializationWizardClientShowsPairingGuideAndRequestsShutdown(t *testing.T) {
	initializer := &stubInitializer{}
	shutdown := make(chan struct{}, 1)
	handler, startup := newInitializationWizardForTest(t, initializer, func(config *Config) {
		config.RequestShutdown = func(ShutdownReason) { shutdown <- struct{}{} }
		config.Sleep = func(time.Duration) {}
	})
	start := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookie := start.Cookies()[0]
	start.Body.Close()
	choice := wizardPage(t, handler, cookie, "/ui/")

	selected := wizardPost(t, handler, cookie, "/ui/init/role", hiddenInputValue(t, choice, "csrf_token"), url.Values{"role": {"client"}})
	selected.Body.Close()
	if selected.StatusCode != http.StatusSeeOther || selected.Header.Get("Location") != "/ui/init/client" {
		t.Fatalf("选客户端应进入配对指引页，状态码=%d 跳转=%q", selected.StatusCode, selected.Header.Get("Location"))
	}

	response := wizardGet(t, handler, cookie, "/ui/init/client")
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("客户端配对指引状态码=%d，页面：\n%s", response.StatusCode, body)
	}
	if declared := response.Header.Get("Content-Length"); declared == "" {
		t.Fatalf("客户端配对指引应声明完整页面长度，页面：\n%s", body)
	}
	page := html.UnescapeString(string(body))
	for _, want := range []string{
		"sshmgr pair --instance <名字>",
		"--url <权威端地址>：不使用自动发现，直接指定权威端地址。",
		"--pin <证书公钥指纹>：校验权威端证书，防止连接被冒充。",
		"--write-mcp <文件路径>：把配对生成的 MCP 配置文件另存到指定路径。",
		"到权威端 Web UI 的配对页批准",
		"本页不执行配对，也不会写入任何文件",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("客户端配对指引应包含 %q：\n%s", want, page)
		}
	}
	if initializer.selectedRole != "" || initializer.initializedRole != "" {
		t.Fatalf("客户端指引不得改写本机状态：选角色=%q 初始化=%q", initializer.selectedRole, initializer.initializedRole)
	}

	select {
	case <-shutdown:
	case <-time.After(2 * time.Second):
		t.Fatal("客户端配对指引完整返回后应触发 Web UI 退出")
	}
}
