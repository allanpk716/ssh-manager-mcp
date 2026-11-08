package webui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

type fakeAuthorityAuth struct {
	passwordSet bool
	password    string
	generation  int64
	audits      []AuditEvent
	failures    map[string]int
}

func (s *fakeAuthorityAuth) AdminPasswordIsSet() (bool, error) {
	return s.passwordSet, nil
}

func (s *fakeAuthorityAuth) VerifyAdminPassword(password []byte) (bool, error) {
	return s.passwordSet && string(password) == s.password, nil
}

func (s *fakeAuthorityAuth) SetAdminPassword(password []byte) (int64, error) {
	s.passwordSet = true
	s.password = string(password)
	s.generation++
	return s.generation, nil
}

func (s *fakeAuthorityAuth) SessionGeneration() (int64, error) {
	return s.generation, nil
}

func (s *fakeAuthorityAuth) LogoutAllSessions() (int64, error) {
	s.generation++
	return s.generation, nil
}

func (s *fakeAuthorityAuth) LoginLimit(ip string, now time.Time) (LoginLimit, error) {
	return LoginLimit{}, nil
}

func (s *fakeAuthorityAuth) RecordLoginFailure(ip string, now time.Time) (LoginLimit, error) {
	if s.failures == nil {
		s.failures = map[string]int{}
	}
	s.failures[ip]++
	return LoginLimit{}, nil
}

func (s *fakeAuthorityAuth) ClearLoginLock(ip string) error { return nil }

func (s *fakeAuthorityAuth) WriteAudit(event AuditEvent) error {
	s.audits = append(s.audits, event)
	return nil
}

func TestAuthorityWithoutPasswordShowsOnlySetupGuidance(t *testing.T) {
	handler, _, err := NewAuthority(Config{
		Role:                   RoleServer,
		Servers:                &stubServerSource{},
		Auth:                   &fakeAuthorityAuth{},
		CertificateFingerprint: "sha256:0123456789abcdef",
		AllowedHosts:           []string{"127.0.0.1"},
		Port:                   7879,
		Now:                    func() time.Time { return time.Unix(1800000000, 0) },
	})
	if err != nil {
		t.Fatalf("构造权威端 Web UI 失败：%v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/ui/login", nil)
	req.Host = "127.0.0.1:7879"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	resp := rec.Result()
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	page := string(body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("未设口令的登录页状态码 = %d，应为 200，页面：\n%s", resp.StatusCode, page)
	}
	for _, want := range []string{
		"sshmgr ui", "sshmgr serve admin set", "sha256:0123456789abcdef",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("未设口令页面应包含 %q：\n%s", want, page)
		}
	}
	if strings.Contains(page, "<form") {
		t.Fatalf("未设口令页面不应提供表单或操作入口：\n%s", page)
	}
}

func TestAuthorityLoginExchangesPasswordForSecureSession(t *testing.T) {
	auth := &fakeAuthorityAuth{passwordSet: true, password: "correct-password"}
	handler, _, err := NewAuthority(Config{
		Role:                   RoleServer,
		Servers:                &stubServerSource{rows: []ServerRow{{Name: "权威服务器"}}},
		Auth:                   auth,
		CertificateFingerprint: "sha256:0123456789abcdef",
		AllowedHosts:           []string{"127.0.0.1", "192.0.2.20"},
		Port:                   7879,
		Now:                    func() time.Time { return time.Unix(1800000000, 0) },
	})
	if err != nil {
		t.Fatalf("构造权威端 Web UI 失败：%v", err)
	}

	loginPage := httptest.NewRequest(http.MethodGet, "/ui/login", nil)
	loginPage.Host = "192.0.2.20:7879"
	loginRecorder := httptest.NewRecorder()
	handler.ServeHTTP(loginRecorder, loginPage)
	loginResponse := loginRecorder.Result()
	if loginResponse.StatusCode != http.StatusOK {
		t.Fatalf("登录页状态码 = %d，应为 200", loginResponse.StatusCode)
	}
	pageBody, _ := io.ReadAll(loginResponse.Body)
	loginResponse.Body.Close()
	match := regexp.MustCompile(`<input type="hidden" name="csrf_token" value="([^"]+)"`).FindStringSubmatch(string(pageBody))
	if match == nil {
		t.Fatalf("登录表单应包含防跨站请求伪造令牌：\n%s", pageBody)
	}
	var csrfCookie *http.Cookie
	for _, cookie := range loginResponse.Cookies() {
		if cookie.Name == authorityCSRFCookieName {
			csrfCookie = cookie
		}
	}
	if csrfCookie == nil {
		t.Fatal("登录页应签发防跨站请求伪造 cookie")
	}

	form := url.Values{"password": {"correct-password"}, "csrf_token": {match[1]}}
	loginRequest := httptest.NewRequest(http.MethodPost, "/ui/login", strings.NewReader(form.Encode()))
	loginRequest.Host = "192.0.2.20:7879"
	loginRequest.RemoteAddr = "198.51.100.9:45678"
	loginRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginRequest.Header.Set("Origin", "https://192.0.2.20:7879")
	loginRequest.AddCookie(csrfCookie)
	loginRecorder = httptest.NewRecorder()
	handler.ServeHTTP(loginRecorder, loginRequest)
	loginResponse = loginRecorder.Result()
	loginResponse.Body.Close()
	if loginResponse.StatusCode != http.StatusSeeOther {
		t.Fatalf("正确口令登录状态码 = %d，应为 303", loginResponse.StatusCode)
	}
	if location := loginResponse.Header.Get("Location"); location != "/ui/" {
		t.Fatalf("登录后跳转 = %q，应为 /ui/", location)
	}
	cookies := loginResponse.Cookies()
	if len(cookies) != 1 || cookies[0].Name != "__Host-sshmgr_ui" {
		t.Fatalf("登录应只签发权威端会话 cookie，实际：%v", cookies)
	}
	sessionCookie := cookies[0]
	if !sessionCookie.Secure || !sessionCookie.HttpOnly || sessionCookie.Path != "/" ||
		sessionCookie.SameSite != http.SameSiteStrictMode || sessionCookie.Domain != "" {
		t.Fatalf("权威端会话 cookie 属性不正确：%+v", sessionCookie)
	}
	if len(auth.audits) != 1 || auth.audits[0].Action != "web.login.success" ||
		auth.audits[0].SourceIP != "198.51.100.9" {
		t.Fatalf("登录成功应写入来源 IP 的 Web 审计：%+v", auth.audits)
	}

	listRequest := httptest.NewRequest(http.MethodGet, "/ui/", nil)
	listRequest.Host = "192.0.2.20:7879"
	listRequest.AddCookie(sessionCookie)
	listRecorder := httptest.NewRecorder()
	handler.ServeHTTP(listRecorder, listRequest)
	listResponse := listRecorder.Result()
	listBody, _ := io.ReadAll(listResponse.Body)
	listResponse.Body.Close()
	if listResponse.StatusCode != http.StatusOK || !strings.Contains(string(listBody), "权威服务器") {
		t.Fatalf("登录后应看到服务器列表，状态码 = %d，页面：\n%s", listResponse.StatusCode, listBody)
	}
}
