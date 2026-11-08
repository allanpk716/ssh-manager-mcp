package webui

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type settingsStubOpsStore struct {
	*stubOpsStore
	settings []OpsSetting
	role     OpsRole

	validateErr   error
	validateKey   string
	validateValue string

	setKey    string
	setValue  string
	setSource OpsWriteSource
	unsetKey  string
}

func (s *settingsStubOpsStore) ListSettings() ([]OpsSetting, error) {
	return s.settings, nil
}

func (s *settingsStubOpsStore) ValidateSetting(key, value string) error {
	s.validateKey, s.validateValue = key, value
	return s.validateErr
}

func (s *settingsStubOpsStore) SetSetting(key, value string, source OpsWriteSource) error {
	s.setKey, s.setValue, s.setSource = key, value, source
	return nil
}

func (s *settingsStubOpsStore) UnsetSetting(key string, source OpsWriteSource) error {
	s.unsetKey = key
	s.setSource = source
	return nil
}

func (s *settingsStubOpsStore) ReadRole() (OpsRole, error) {
	return s.role, nil
}

func testSettingsRows() []OpsSetting {
	return []OpsSetting{
		{
			Key: "serve.pairing", Type: OpsSettingTypeSwitch, StoredValue: "false",
			CurrentValue: "true", DefaultValue: "true", Source: OpsSettingSourceEnvironment,
			Overridden: true, CanBeOverridden: true, Effect: "约 5 秒内生效",
		},
		{
			Key: "serve.ui_port", Type: OpsSettingTypePort, StoredValue: "7879",
			CurrentValue: "7879", DefaultValue: "7879", Source: OpsSettingSourceStore,
			Effect: "重启服务后生效",
		},
		{
			Key: "pair.default_profile", Type: OpsSettingTypeProfile, DefaultValue: "",
			CurrentValue: "", Source: OpsSettingSourceDefault, Effect: "下次配对时生效",
			Choices: []OpsSettingChoice{{Value: "ops", Label: "ops"}},
		},
	}
}

func newSettingsTestHandler(t *testing.T, role Role, ops *settingsStubOpsStore) (http.Handler, *http.Cookie) {
	t.Helper()
	if role == RoleServer {
		auth := &fakeAuthorityAuth{passwordSet: true, password: "correct-password"}
		handler, _, err := NewAuthority(Config{
			Role: role, Servers: &stubServerSource{}, Access: &stubAccessStore{},
			Ops: ops, Auth: auth, AllowedHosts: []string{"127.0.0.1"}, Port: 7879,
		})
		if err != nil {
			t.Fatal(err)
		}
		cookie := authoritySuccessfulLogin(t, handler, "127.0.0.1:7879", "correct-password")
		return handler, cookie
	}
	handler, startup, err := New(Config{
		Role: role, Servers: &stubServerSource{}, Access: &stubAccessStore{},
		Ops: ops, Port: 18080,
	})
	if err != nil {
		t.Fatal(err)
	}
	session := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookie := session.Cookies()[0]
	session.Body.Close()
	return handler, cookie
}

func TestOpsSettingsPageRendersTableAndRole(t *testing.T) {
	ops := &settingsStubOpsStore{
		stubOpsStore: &stubOpsStore{}, settings: testSettingsRows(),
		role: OpsRole{RoleName: "standalone", SetupComplete: true, Path: "/tmp/vault/role.json"},
	}
	handler, cookie := newSettingsTestHandler(t, RoleStandalone, ops)
	response := doGetWithCookie(t, handler, cookie, "/ui/ops/settings")
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "<h1>设置</h1>") {
		t.Fatalf("单机应打开设置页，状态码=%d 页面：\n%s", response.StatusCode, body)
	}
	for _, want := range []string{
		"serve.pairing", "约 5 秒内生效", "当前被覆盖，修改设置存储不会立即生效",
		"serve.ui_port", "重启服务后生效", "pair.default_profile", "下次配对时生效",
		"standalone", "已完成", "/tmp/vault/role.json",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("设置页缺少 %q：\n%s", want, body)
		}
	}
	if !strings.Contains(body, `<option value="ops">ops</option>`) {
		t.Fatalf("默认 profile 应由设置项表生成候选：\n%s", body)
	}
}

func TestOpsSettingsInvalidValueRendersPageWithError(t *testing.T) {
	ops := &settingsStubOpsStore{
		stubOpsStore: &stubOpsStore{}, settings: testSettingsRows(),
		validateErr: errors.New("设置项 serve.ui_port 必须是 1 到 65535 的端口"),
	}
	handler, cookie := newSettingsTestHandler(t, RoleStandalone, ops)
	response := postStandaloneForm(t, handler, cookie, "/ui/ops/settings", url.Values{
		"key": {"serve.ui_port"}, "value": {"70000"},
	})
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(body, "<h1>设置</h1>") ||
		!strings.Contains(body, "必须是 1 到 65535 的端口") || !strings.Contains(body, `value="70000"`) {
		t.Fatalf("非法设置值应重新渲染设置页并显示错误，状态码=%d 页面：\n%s", response.StatusCode, body)
	}
	if ops.setKey != "" {
		t.Fatalf("非法设置值不应写入：%+v", ops)
	}
}

func TestOpsSettingsStandaloneWriteAndRestoreDoNotRequireReverify(t *testing.T) {
	ops := &settingsStubOpsStore{stubOpsStore: &stubOpsStore{}, settings: testSettingsRows()}
	handler, cookie := newSettingsTestHandler(t, RoleStandalone, ops)
	response := postStandaloneForm(t, handler, cookie, "/ui/ops/settings", url.Values{
		"key": {"serve.pairing"}, "value": {"false"},
	})
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/ui/ops/settings" {
		t.Fatalf("单机保存设置不应要求重新验证，状态码=%d 跳转=%q", response.StatusCode, response.Header.Get("Location"))
	}
	if ops.validateKey != "serve.pairing" || ops.validateValue != "false" ||
		ops.setKey != "serve.pairing" || ops.setValue != "false" ||
		ops.setSource.RemoteAddress != "198.51.100.9" {
		t.Fatalf("单机保存设置传参不符：%+v", ops)
	}

	response = postStandaloneForm(t, handler, cookie, "/ui/ops/settings", url.Values{
		"key": {"serve.pairing"}, "action": {"restore_default"},
	})
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther || ops.unsetKey != "serve.pairing" {
		t.Fatalf("单机恢复默认值不应要求重新验证，状态码=%d 删除键=%q", response.StatusCode, ops.unsetKey)
	}
}

func TestAuthorityOpsSettingsWritesRequireReverify(t *testing.T) {
	auth := &fakeAuthorityAuth{passwordSet: true, password: "correct-password", now: time.Unix(1800000000, 0)}
	ops := &settingsStubOpsStore{stubOpsStore: &stubOpsStore{}, settings: testSettingsRows()}
	handler, _, err := NewAuthority(Config{
		Role: RoleServer, Servers: &stubServerSource{}, Access: &stubAccessStore{},
		Ops: ops, Auth: auth, AllowedHosts: []string{"127.0.0.1"}, Port: 7879,
		Now: func() time.Time { return auth.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie := authoritySuccessfulLogin(t, handler, "127.0.0.1:7879", "correct-password")
	auth.now = auth.now.Add(6 * time.Minute)
	page := authorityGet(t, handler, "127.0.0.1:7879", "/ui/ops/settings", cookie)
	pageBody := readResponseBody(t, page)
	csrfToken := hiddenInputValue(t, pageBody, "csrf_token")

	rejected := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/ops/settings", url.Values{
		"csrf_token": {csrfToken}, "key": {"serve.pairing"}, "value": {"false"},
	}, cookie)
	rejectedBody := readResponseBody(t, rejected)
	resume := "/ui/ops/settings?key=serve.pairing&reverify=1&value=false"
	wantRedirect := "/ui/reverify?next=" + url.QueryEscape(resume)
	if rejected.StatusCode != http.StatusSeeOther || rejected.Header.Get("Location") != wantRedirect ||
		ops.validateKey != "" || ops.setKey != "" {
		t.Fatalf("权威端保存设置应先重新验证，状态码=%d 跳转=%q 传参=%+v 页面：\n%s",
			rejected.StatusCode, rejected.Header.Get("Location"), ops, rejectedBody)
	}

	reverified := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/reverify", url.Values{
		"csrf_token": {csrfToken}, "password": {"correct-password"}, "next": {resume},
	}, cookie)
	reverified.Body.Close()
	if reverified.StatusCode != http.StatusSeeOther || reverified.Header.Get("Location") != resume {
		t.Fatalf("重新验证后应回到设置页，状态码=%d 跳转=%q", reverified.StatusCode, reverified.Header.Get("Location"))
	}
	resumedPage := authorityGet(t, handler, "127.0.0.1:7879", resume, cookie)
	resumedBody := readResponseBody(t, resumedPage)
	if resumedPage.StatusCode != http.StatusOK ||
		!strings.Contains(resumedBody, `value="false"`) {
		t.Fatalf("重新验证回跳后应预填提交的设置值，状态码=%d 页面：\n%s",
			resumedPage.StatusCode, resumedBody)
	}
	allowed := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/ops/settings", url.Values{
		"csrf_token": {csrfToken}, "key": {"serve.pairing"}, "value": {"false"},
	}, cookie)
	allowed.Body.Close()
	if allowed.StatusCode != http.StatusSeeOther || ops.setKey != "serve.pairing" ||
		ops.setValue != "false" || ops.setSource.RemoteAddress != "198.51.100.9" {
		t.Fatalf("重新验证后应能保存设置，状态码=%d 传参=%+v", allowed.StatusCode, ops)
	}
}

func TestOpsSettingsWritesRequireOriginAndCSRFToken(t *testing.T) {
	ops := &settingsStubOpsStore{stubOpsStore: &stubOpsStore{}, settings: testSettingsRows()}
	handler, cookie := newSettingsTestHandler(t, RoleStandalone, ops)
	form := url.Values{"key": {"serve.pairing"}, "value": {"false"}}
	missingTokenForm := url.Values{}
	for key, values := range form {
		missingTokenForm[key] = append([]string(nil), values...)
	}
	missingToken := postAccessWrite(t, handler, cookie, "/ui/ops/settings", missingTokenForm, "http://127.0.0.1:18080", "")
	missingTokenBody := readResponseBody(t, missingToken)
	if missingToken.StatusCode != http.StatusForbidden || ops.setKey != "" {
		t.Fatalf("缺少防跨站请求伪造令牌应拒绝，状态码=%d 传参=%+v 页面：\n%s",
			missingToken.StatusCode, ops, missingTokenBody)
	}

	page := doGetWithCookie(t, handler, cookie, "/ui/ops/settings")
	pageBody := readResponseBody(t, page)
	csrfToken := hiddenInputValue(t, pageBody, "csrf_token")
	wrongOrigin := postAccessWrite(t, handler, cookie, "/ui/ops/settings", form, "https://evil.example", csrfToken)
	wrongOriginBody := readResponseBody(t, wrongOrigin)
	if wrongOrigin.StatusCode != http.StatusForbidden || ops.setKey != "" {
		t.Fatalf("来源不对应拒绝，状态码=%d 传参=%+v 页面：\n%s",
			wrongOrigin.StatusCode, ops, wrongOriginBody)
	}
}
