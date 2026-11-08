package webui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeAuthorityAuth struct {
	passwordSet bool
	password    string
	generation  int64
	audits      []AuditEvent
	failures    map[string]int
	sleeps      []time.Duration
	now         time.Time
	mu          sync.Mutex
	verifyCalls int
}

func (s *fakeAuthorityAuth) AdminPasswordIsSet() (bool, error) {
	return s.passwordSet, nil
}

func (s *fakeAuthorityAuth) VerifyAdminPassword(password []byte) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.verifyCalls++
	return s.passwordSet && string(password) == s.password, nil
}

func (s *fakeAuthorityAuth) SetAdminPasswordWithAudit(password []byte, event AuditEvent) (int64, error) {
	s.passwordSet = true
	s.password = string(password)
	s.generation++
	s.mu.Lock()
	s.audits = append(s.audits, event)
	s.mu.Unlock()
	return s.generation, nil
}

func (s *fakeAuthorityAuth) SessionGeneration() (int64, error) {
	return s.generation, nil
}

func (s *fakeAuthorityAuth) LogoutAllSessionsWithAudit(event AuditEvent) (int64, error) {
	s.generation++
	s.mu.Lock()
	s.audits = append(s.audits, event)
	s.mu.Unlock()
	return s.generation, nil
}

func (s *fakeAuthorityAuth) LoginLimit(ip string, now time.Time) (LoginLimit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, count := range s.failures {
		total += count
	}
	delay := time.Duration(total) * time.Second
	if delay > 10*time.Second {
		delay = 10 * time.Second
	}
	return LoginLimit{Locked: s.failures[ip] >= 5, LockRemaining: 15 * time.Minute, Delay: delay}, nil
}

func (s *fakeAuthorityAuth) RecordLoginFailure(ip string, now time.Time) (LoginLimit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failures == nil {
		s.failures = map[string]int{}
	}
	s.failures[ip]++
	ipFailures := s.failures[ip]
	total := 0
	for _, count := range s.failures {
		total += count
	}
	delay := time.Duration(total) * time.Second
	if delay > 10*time.Second {
		delay = 10 * time.Second
	}
	return LoginLimit{Locked: ipFailures >= 5, LockRemaining: 15 * time.Minute, Delay: delay}, nil
}

func (s *fakeAuthorityAuth) ClearLoginLock(ip string) error { return nil }

func (s *fakeAuthorityAuth) WriteAudit(event AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audits = append(s.audits, event)
	return nil
}

func TestAuthorityWithoutPasswordShowsOnlySetupGuidance(t *testing.T) {
	handler, _, err := NewAuthority(Config{
		Role:                   RoleServer,
		Servers:                &stubServerSource{},
		Access:                 &stubAccessStore{},
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
	for _, target := range []string{"/ui/", "/ui/password", "/ui/other"} {
		resp := authorityGet(t, handler, "127.0.0.1:7879", target, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != loginPath {
			t.Fatalf("未设口令时 %s 应跳转登录页，状态码=%d 跳转=%q", target, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
}

func TestAuthorityLoginExchangesPasswordForSecureSession(t *testing.T) {
	auth := &fakeAuthorityAuth{passwordSet: true, password: "correct-password"}
	handler, _, err := NewAuthority(Config{
		Role:                   RoleServer,
		Servers:                &stubServerSource{rows: []ServerRow{{Name: "权威服务器"}}},
		Access:                 &stubAccessStore{},
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
	if len(auth.audits) != 1 || auth.audits[0].Action != "admin.login" ||
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

func authorityLoginPOST(t *testing.T, handler http.Handler, host, password string, cookies ...*http.Cookie) *http.Response {
	t.Helper()
	form := url.Values{"password": {password}}
	var csrfCookie *http.Cookie
	for _, cookie := range cookies {
		if cookie.Name == authorityCSRFCookieName {
			csrfCookie = cookie
		}
	}
	if csrfCookie != nil {
		form.Set("csrf_token", csrfCookie.Value)
	}
	req := httptest.NewRequest(http.MethodPost, "/ui/login", strings.NewReader(form.Encode()))
	req.Host = host
	req.RemoteAddr = "198.51.100.9:45678"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://"+host)
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Result()
}

func authorityLoginPage(t *testing.T, handler http.Handler, host string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/ui/login", nil)
	req.Host = host
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Result()
}

func authoritySuccessfulLogin(t *testing.T, handler http.Handler, host, password string) *http.Cookie {
	t.Helper()
	page := authorityLoginPage(t, handler, host)
	cookies := page.Cookies()
	page.Body.Close()
	resp := authorityLoginPOST(t, handler, host, password, cookies...)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("登录状态码 = %d，应为 303", resp.StatusCode)
	}
	for _, cookie := range resp.Cookies() {
		if cookie.Name == authoritySessionCookieName {
			return cookie
		}
	}
	t.Fatal("登录响应缺少会话 cookie")
	return nil
}

func authorityGet(t *testing.T, handler http.Handler, host, target string, cookie *http.Cookie) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = host
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Result()
}

func TestAuthoritySessionSlidesAndGenerationInvalidates(t *testing.T) {
	auth := &fakeAuthorityAuth{passwordSet: true, password: "correct-password"}
	now := time.Unix(1800000000, 0)
	handler, _, err := NewAuthority(Config{
		Role: RoleServer, Servers: &stubServerSource{rows: []ServerRow{{Name: "服务器"}}}, Access: &stubAccessStore{}, Auth: auth,
		CertificateFingerprint: "sha256:0123456789abcdef",
		AllowedHosts:           []string{"127.0.0.1"}, Port: 7879,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("构造权威端 Web UI 失败：%v", err)
	}
	cookie := authoritySuccessfulLogin(t, handler, "127.0.0.1:7879", "correct-password")

	now = now.Add(11 * time.Hour)
	resp := authorityGet(t, handler, "127.0.0.1:7879", "/ui/", cookie)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("11 小时后的会话状态码 = %d，应为 200", resp.StatusCode)
	}
	now = now.Add(11 * time.Hour)
	resp = authorityGet(t, handler, "127.0.0.1:7879", "/ui/", cookie)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("滑动后续 11 小时后的会话状态码 = %d，应为 200", resp.StatusCode)
	}
	now = now.Add(12 * time.Hour)
	resp = authorityGet(t, handler, "127.0.0.1:7879", "/ui/", cookie)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != loginPath {
		t.Fatalf("超过 12 小时未活动应回登录页，状态码=%d 跳转=%q", resp.StatusCode, resp.Header.Get("Location"))
	}

	cookie = authoritySuccessfulLogin(t, handler, "127.0.0.1:7879", "correct-password")
	auth.generation++
	resp = authorityGet(t, handler, "127.0.0.1:7879", "/ui/", cookie)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != loginPath {
		t.Fatalf("会话代变化后既有会话应失效，状态码=%d 跳转=%q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestAuthorityConcurrentRequestsUseOneSessionSafely(t *testing.T) {
	auth := &fakeAuthorityAuth{passwordSet: true, password: "correct-password"}
	handler, _, err := NewAuthority(Config{
		Role: RoleServer, Servers: &stubServerSource{rows: []ServerRow{{Name: "服务器"}}}, Access: &stubAccessStore{}, Auth: auth,
		CertificateFingerprint: "sha256:0123456789abcdef",
		AllowedHosts:           []string{"127.0.0.1"}, Port: 7879,
		Now: func() time.Time { return time.Unix(1800000000, 0) },
	})
	if err != nil {
		t.Fatalf("构造权威端 Web UI 失败：%v", err)
	}
	cookie := authoritySuccessfulLogin(t, handler, "127.0.0.1:7879", "correct-password")

	const requests = 32
	var wait sync.WaitGroup
	wait.Add(requests)
	for i := 0; i < requests; i++ {
		go func() {
			defer wait.Done()
			resp := authorityGet(t, handler, "127.0.0.1:7879", "/ui/", cookie)
			resp.Body.Close()
		}()
	}
	wait.Wait()
}

func TestAuthorityConcurrentFailuresCannotBypassLoginLimit(t *testing.T) {
	auth := &fakeAuthorityAuth{passwordSet: true, password: "correct-password"}
	handler, _, err := NewAuthority(Config{
		Role: RoleServer, Servers: &stubServerSource{}, Access: &stubAccessStore{}, Auth: auth,
		CertificateFingerprint: "sha256:0123456789abcdef",
		AllowedHosts:           []string{"127.0.0.1"}, Port: 7879,
		Now:   func() time.Time { return time.Unix(1800000000, 0) },
		Sleep: func(time.Duration) {},
	})
	if err != nil {
		t.Fatalf("构造权威端 Web UI 失败：%v", err)
	}
	page := authorityLoginPage(t, handler, "127.0.0.1:7879")
	var csrfCookie *http.Cookie
	for _, cookie := range page.Cookies() {
		if cookie.Name == authorityCSRFCookieName {
			csrfCookie = cookie
		}
	}
	page.Body.Close()
	if csrfCookie == nil {
		t.Fatal("登录页应签发防跨站请求伪造 cookie")
	}

	const requests = 12
	start := make(chan struct{})
	responses := make(chan *http.Response, requests)
	var wait sync.WaitGroup
	wait.Add(requests)
	for i := 0; i < requests; i++ {
		go func() {
			defer wait.Done()
			form := url.Values{"password": {"wrong-password"}, "csrf_token": {csrfCookie.Value}}
			req := httptest.NewRequest(http.MethodPost, "/ui/login", strings.NewReader(form.Encode()))
			req.Host = "127.0.0.1:7879"
			req.RemoteAddr = "198.51.100.9:45678"
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Origin", "https://127.0.0.1:7879")
			req.AddCookie(csrfCookie)
			<-start
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			responses <- rec.Result()
		}()
	}
	close(start)
	wait.Wait()
	close(responses)
	for response := range responses {
		response.Body.Close()
	}

	auth.mu.Lock()
	verifyCalls := auth.verifyCalls
	auth.mu.Unlock()
	if verifyCalls > 5 {
		t.Fatalf("并发错误口令实际进入校验 %d 次，不得超过 5 次", verifyCalls)
	}
}

func TestAuthorityLoginCleanupDropsExpiredSessionsOnNewLogin(t *testing.T) {
	auth := &fakeAuthorityAuth{passwordSet: true, password: "correct-password", now: time.Unix(1800000000, 0)}
	handler, _, err := NewAuthority(Config{
		Role: RoleServer, Servers: &stubServerSource{}, Access: &stubAccessStore{}, Auth: auth,
		CertificateFingerprint: "sha256:0123456789abcdef",
		AllowedHosts:           []string{"127.0.0.1"}, Port: 7879,
		Now: func() time.Time {
			auth.mu.Lock()
			defer auth.mu.Unlock()
			return auth.now
		},
	})
	if err != nil {
		t.Fatalf("构造权威端 Web UI 失败：%v", err)
	}
	oldCookie := authoritySuccessfulLogin(t, handler, "127.0.0.1:7879", "correct-password")
	base := time.Unix(1800000000, 0)
	auth.mu.Lock()
	auth.now = base.Add(12*time.Hour + time.Second)
	auth.mu.Unlock()
	newCookie := authoritySuccessfulLogin(t, handler, "127.0.0.1:7879", "correct-password")

	auth.mu.Lock()
	auth.now = base.Add(11 * time.Hour)
	auth.mu.Unlock()
	expired := authorityGet(t, handler, "127.0.0.1:7879", "/ui/", oldCookie)
	expired.Body.Close()
	if expired.StatusCode != http.StatusSeeOther || expired.Header.Get("Location") != loginPath {
		t.Fatalf("新登录应顺带清理已过期会话，状态码=%d 跳转=%q", expired.StatusCode, expired.Header.Get("Location"))
	}
	fresh := authorityGet(t, handler, "127.0.0.1:7879", "/ui/", newCookie)
	fresh.Body.Close()
	if fresh.StatusCode != http.StatusOK {
		t.Fatalf("新登录会话应仍有效，状态码=%d", fresh.StatusCode)
	}
}

func TestAuthorityHeaderAndLogoutActions(t *testing.T) {
	auth := &fakeAuthorityAuth{passwordSet: true, password: "correct-password"}
	now := time.Unix(1800000000, 0)
	handler, _, err := NewAuthority(Config{
		Role: RoleServer, Servers: &stubServerSource{rows: []ServerRow{{Name: "服务器"}}}, Access: &stubAccessStore{}, Auth: auth,
		CertificateFingerprint: "sha256:0123456789abcdef",
		AllowedHosts:           []string{"127.0.0.1"}, Port: 7879,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("构造权威端 Web UI 失败：%v", err)
	}
	cookie := authoritySuccessfulLogin(t, handler, "127.0.0.1:7879", "correct-password")

	page := authorityGet(t, handler, "127.0.0.1:7879", "/ui/", cookie)
	pageBody, _ := io.ReadAll(page.Body)
	page.Body.Close()
	for _, want := range []string{"权威端", "注销全部会话", "修改口令", "注销"} {
		if !strings.Contains(string(pageBody), want) {
			t.Fatalf("权威端页头应包含 %q：\n%s", want, pageBody)
		}
	}
	match := regexp.MustCompile(`<meta name="csrf-token" content="([^"]+)"`).FindStringSubmatch(string(pageBody))
	if match == nil {
		t.Fatalf("服务器列表页应包含防跨站请求伪造令牌：\n%s", pageBody)
	}

	logout := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/logout", url.Values{
		"csrf_token": {match[1]},
	}, cookie)
	logout.Body.Close()
	if logout.StatusCode != http.StatusSeeOther || logout.Header.Get("Location") != loginPath {
		t.Fatalf("注销状态码=%d 跳转=%q，应为 303 与登录页", logout.StatusCode, logout.Header.Get("Location"))
	}
	if len(logout.Cookies()) == 0 || logout.Cookies()[0].Value != "" {
		t.Fatalf("注销应清除会话 cookie：%v", logout.Cookies())
	}
	afterLogout := authorityGet(t, handler, "127.0.0.1:7879", "/ui/", cookie)
	afterLogout.Body.Close()
	if afterLogout.StatusCode != http.StatusSeeOther {
		t.Fatalf("注销后的会话状态码 = %d，应为 303", afterLogout.StatusCode)
	}

	cookie = authoritySuccessfulLogin(t, handler, "127.0.0.1:7879", "correct-password")
	otherCookie := authoritySuccessfulLogin(t, handler, "127.0.0.1:7879", "correct-password")
	page = authorityGet(t, handler, "127.0.0.1:7879", "/ui/", cookie)
	pageBody, _ = io.ReadAll(page.Body)
	page.Body.Close()
	match = regexp.MustCompile(`<meta name="csrf-token" content="([^"]+)"`).FindStringSubmatch(string(pageBody))
	if match == nil {
		t.Fatalf("服务器列表页应包含防跨站请求伪造令牌：\n%s", pageBody)
	}
	logoutAll := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/logout-all", url.Values{
		"csrf_token": {match[1]},
	}, cookie)
	logoutAll.Body.Close()
	if logoutAll.StatusCode != http.StatusSeeOther || logoutAll.Header.Get("Location") != loginPath {
		t.Fatalf("注销全部会话状态码=%d 跳转=%q，应为 303 与登录页", logoutAll.StatusCode, logoutAll.Header.Get("Location"))
	}
	otherPage := authorityGet(t, handler, "127.0.0.1:7879", "/ui/", otherCookie)
	otherPage.Body.Close()
	if otherPage.StatusCode != http.StatusSeeOther {
		t.Fatalf("另一个会话也应被注销，状态码=%d，应为 303", otherPage.StatusCode)
	}
	var logoutCount, logoutAllCount int
	for _, event := range auth.audits {
		switch event.Action {
		case "admin.logout":
			logoutCount++
		case "admin.logout_all":
			logoutAllCount++
		}
	}
	if logoutCount != 1 || logoutAllCount != 1 {
		t.Fatalf("注销审计数量不正确：单次=%d 全部=%d，应为 1 与 1", logoutCount, logoutAllCount)
	}
}

func authorityPostForm(t *testing.T, handler http.Handler, host, target string, form url.Values, cookie *http.Cookie) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Host = host
	req.RemoteAddr = "198.51.100.9:45678"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://"+host)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Result()
}

func TestAuthorityHostOriginAndCSRFGuards(t *testing.T) {
	auth := &fakeAuthorityAuth{passwordSet: true, password: "correct-password", now: time.Unix(1800000000, 0)}
	handler, control, err := NewAuthority(Config{
		Role: RoleServer, Servers: &stubServerSource{}, Access: &stubAccessStore{}, Auth: auth,
		CertificateFingerprint: "sha256:0123456789abcdef",
		AllowedHosts:           []string{"127.0.0.1"}, Port: 7879,
		Now:   func() time.Time { return auth.now },
		Sleep: func(time.Duration) {},
	})
	if err != nil {
		t.Fatalf("构造权威端 Web UI 失败：%v", err)
	}

	allowed := authorityLoginPage(t, handler, "127.0.0.1:7879")
	allowed.Body.Close()
	if allowed.StatusCode != http.StatusOK {
		t.Fatalf("允许的 Host 状态码 = %d，应为 200", allowed.StatusCode)
	}
	rejectedHost := authorityLoginPage(t, handler, "192.0.2.20:7879")
	rejectedHost.Body.Close()
	if rejectedHost.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("未监听地址的 Host 状态码 = %d，应为 421", rejectedHost.StatusCode)
	}
	for _, host := range []string{"127.0.0.1:18000", "localhost:18000", "[::1]:18000"} {
		forwarded := authorityLoginPage(t, handler, host)
		forwarded.Body.Close()
		if forwarded.StatusCode != http.StatusOK {
			t.Fatalf("回环地址经 SSH 端口转发应允许 Host %s，状态码 = %d", host, forwarded.StatusCode)
		}
	}
	wrongPort := authorityLoginPage(t, handler, "192.0.2.20:18000")
	wrongPort.Body.Close()
	if wrongPort.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("非回环地址仍应校验配置端口，状态码 = %d", wrongPort.StatusCode)
	}
	control.SetAllowedHosts([]string{"192.0.2.20"})
	allowed = authorityLoginPage(t, handler, "192.0.2.20:7879")
	allowed.Body.Close()
	if allowed.StatusCode != http.StatusOK {
		t.Fatalf("监听管理者更新后的 Host 状态码 = %d，应为 200", allowed.StatusCode)
	}
	control.SetAllowedHosts([]string{"127.0.0.1"})

	page := authorityLoginPage(t, handler, "127.0.0.1:7879")
	cookies := page.Cookies()
	page.Body.Close()
	form := url.Values{"password": {"correct-password"}}
	for _, cookie := range cookies {
		if cookie.Name == authorityCSRFCookieName {
			form.Set("csrf_token", cookie.Value)
		}
	}
	missingOrigin := httptest.NewRequest(http.MethodPost, "/ui/login", strings.NewReader(form.Encode()))
	missingOrigin.Host = "127.0.0.1:7879"
	missingOrigin.RemoteAddr = "198.51.100.9:45678"
	missingOrigin.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, cookie := range cookies {
		missingOrigin.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, missingOrigin)
	missingOriginResp := rec.Result()
	missingOriginResp.Body.Close()
	if missingOriginResp.StatusCode != http.StatusForbidden {
		t.Fatalf("缺少 Origin 的登录状态码 = %d，应为 403", missingOriginResp.StatusCode)
	}

	form.Del("csrf_token")
	missingToken := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/login", form, nil)
	missingToken.Body.Close()
	if missingToken.StatusCode != http.StatusForbidden {
		t.Fatalf("缺少防跨站请求伪造令牌的登录状态码 = %d，应为 403", missingToken.StatusCode)
	}
	if len(auth.audits) != 0 || len(auth.failures) != 0 {
		t.Fatalf("写请求防护拒绝不应进入口令校验：%+v %+v", auth.audits, auth.failures)
	}
}

func TestAuthorityPasswordChangeRequiresRecentReverification(t *testing.T) {
	auth := &fakeAuthorityAuth{passwordSet: true, password: "old-password", now: time.Unix(1800000000, 0)}
	handler, _, err := NewAuthority(Config{
		Role: RoleServer, Servers: &stubServerSource{}, Access: &stubAccessStore{}, Auth: auth,
		CertificateFingerprint: "sha256:0123456789abcdef",
		AllowedHosts:           []string{"127.0.0.1"}, Port: 7879,
		Now: func() time.Time { return auth.now },
	})
	if err != nil {
		t.Fatalf("构造权威端 Web UI 失败：%v", err)
	}
	cookie := authoritySuccessfulLogin(t, handler, "127.0.0.1:7879", "old-password")

	direct := authorityGet(t, handler, "127.0.0.1:7879", "/ui/password", cookie)
	directBody, _ := io.ReadAll(direct.Body)
	direct.Body.Close()
	if direct.StatusCode != http.StatusOK || !strings.Contains(string(directBody), "新管理员口令") {
		t.Fatalf("登录后 5 分钟内应直接显示修改口令表单，状态码=%d 页面：\n%s", direct.StatusCode, directBody)
	}

	auth.now = auth.now.Add(6 * time.Minute)
	expired := authorityGet(t, handler, "127.0.0.1:7879", "/ui/password", cookie)
	expired.Body.Close()
	if expired.StatusCode != http.StatusSeeOther || expired.Header.Get("Location") != "/ui/reverify?next=/ui/password" {
		t.Fatalf("超过 5 分钟应先重验，状态码=%d 跳转=%q", expired.StatusCode, expired.Header.Get("Location"))
	}

	reverifyPage := authorityGet(t, handler, "127.0.0.1:7879", "/ui/reverify?next=/ui/password", cookie)
	reverifyBody, _ := io.ReadAll(reverifyPage.Body)
	reverifyPage.Body.Close()
	if reverifyPage.StatusCode != http.StatusOK || !strings.Contains(string(reverifyBody), "重新验证") {
		t.Fatalf("重验页状态码=%d，页面：\n%s", reverifyPage.StatusCode, reverifyBody)
	}
	match := regexp.MustCompile(`<input type="hidden" name="csrf_token" value="([^"]+)"`).FindStringSubmatch(string(reverifyBody))
	if match == nil {
		t.Fatalf("重验表单缺少防跨站请求伪造令牌：\n%s", reverifyBody)
	}
	wrong := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/reverify", url.Values{
		"password": {"wrong-password"}, "csrf_token": {match[1]}, "next": {"/ui/password"},
	}, cookie)
	wrong.Body.Close()
	if wrong.StatusCode != http.StatusUnauthorized || auth.failures["198.51.100.9"] != 1 {
		t.Fatalf("重验失败应计入登录失败，状态码=%d failures=%d", wrong.StatusCode, auth.failures["198.51.100.9"])
	}
	reverified := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/reverify", url.Values{
		"password": {"old-password"}, "csrf_token": {match[1]}, "next": {"/ui/password"},
	}, cookie)
	reverified.Body.Close()
	if reverified.StatusCode != http.StatusSeeOther || reverified.Header.Get("Location") != "/ui/password" {
		t.Fatalf("重验成功应回到修改口令，状态码=%d 跳转=%q", reverified.StatusCode, reverified.Header.Get("Location"))
	}

	passwordPage := authorityGet(t, handler, "127.0.0.1:7879", "/ui/password", cookie)
	passwordBody, _ := io.ReadAll(passwordPage.Body)
	passwordPage.Body.Close()
	match = regexp.MustCompile(`<input type="hidden" name="csrf_token" value="([^"]+)"`).FindStringSubmatch(string(passwordBody))
	if match == nil {
		t.Fatalf("修改口令表单缺少防跨站请求伪造令牌：\n%s", passwordBody)
	}
	changed := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/password", url.Values{
		"new_password": {"new-password"}, "confirm_password": {"new-password"}, "csrf_token": {match[1]},
	}, cookie)
	changed.Body.Close()
	if changed.StatusCode != http.StatusSeeOther || changed.Header.Get("Location") != loginPath {
		t.Fatalf("修改口令后应回登录页，状态码=%d 跳转=%q", changed.StatusCode, changed.Header.Get("Location"))
	}
	if auth.password != "new-password" || auth.generation == 0 {
		t.Fatalf("修改口令应写入新口令并推进会话代：%+v", auth)
	}
	oldPage := authorityGet(t, handler, "127.0.0.1:7879", "/ui/", cookie)
	oldPage.Body.Close()
	if oldPage.StatusCode != http.StatusSeeOther {
		t.Fatalf("修改口令后旧会话应失效，状态码=%d", oldPage.StatusCode)
	}
	foundChange := false
	for _, event := range auth.audits {
		if event.Action == "admin.password_change" && event.SourceIP == "198.51.100.9" {
			foundChange = true
		}
	}
	if !foundChange {
		t.Fatalf("修改口令应写入 Web 审计：%+v", auth.audits)
	}
}

func TestAuthorityLoginFailuresLockIPAndDelayAllRequests(t *testing.T) {
	auth := &fakeAuthorityAuth{passwordSet: true, password: "correct-password", now: time.Unix(1800000000, 0)}
	var sleeps []time.Duration
	handler, _, err := NewAuthority(Config{
		Role:                   RoleServer,
		Servers:                &stubServerSource{},
		Access:                 &stubAccessStore{},
		Auth:                   auth,
		CertificateFingerprint: "sha256:0123456789abcdef",
		AllowedHosts:           []string{"127.0.0.1"},
		Port:                   7879,
		Now:                    func() time.Time { return auth.now },
		Sleep:                  func(delay time.Duration) { sleeps = append(sleeps, delay) },
	})
	if err != nil {
		t.Fatalf("构造权威端 Web UI 失败：%v", err)
	}

	for i := 0; i < 4; i++ {
		page := authorityLoginPage(t, handler, "127.0.0.1:7879")
		cookies := page.Cookies()
		page.Body.Close()
		resp := authorityLoginPOST(t, handler, "127.0.0.1:7879", "wrong", cookies...)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("第 %d 次失败状态码 = %d，应为 401", i+1, resp.StatusCode)
		}
	}
	if want := []time.Duration{time.Second, 2 * time.Second, 3 * time.Second}; !reflect.DeepEqual(sleeps, want) {
		t.Fatalf("前四次登录延迟 = %v，应为 %v", sleeps, want)
	}

	page := authorityLoginPage(t, handler, "127.0.0.1:7879")
	cookies := page.Cookies()
	page.Body.Close()
	fifth := authorityLoginPOST(t, handler, "127.0.0.1:7879", "wrong", cookies...)
	body, _ := io.ReadAll(fifth.Body)
	fifth.Body.Close()
	if fifth.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "来源 IP 已锁定") {
		t.Fatalf("第五次失败应显示锁定，状态码 = %d，页面：\n%s", fifth.StatusCode, body)
	}
	if len(sleeps) != 4 || sleeps[3] != 4*time.Second {
		t.Fatalf("第五次登录延迟 = %v，应追加 4 秒", sleeps)
	}

	lockedPage := authorityLoginPage(t, handler, "127.0.0.1:7879")
	lockedCookies := lockedPage.Cookies()
	lockedPage.Body.Close()
	locked := authorityLoginPOST(t, handler, "127.0.0.1:7879", "correct-password", lockedCookies...)
	locked.Body.Close()
	if locked.StatusCode != http.StatusForbidden || auth.failures["198.51.100.9"] != 5 {
		t.Fatalf("锁定期间不应校验口令或增加失败次数：状态码=%d failures=%d", locked.StatusCode, auth.failures["198.51.100.9"])
	}
	if len(sleeps) != 5 || sleeps[4] != 5*time.Second {
		t.Fatalf("锁定请求仍应等待全局延迟：%v", sleeps)
	}

	var failed, lockEvents int
	for _, event := range auth.audits {
		if event.Action == "admin.login_fail" {
			failed++
		}
		if event.Action == "admin.lockout" {
			lockEvents++
		}
		if event.Detail != nil {
			t.Fatalf("登录失败审计不得携带摘要字段：%+v", event)
		}
	}
	if failed != 5 || lockEvents != 1 {
		t.Fatalf("审计失败行 = %d、锁定行 = %d，应为 5 与 1：%+v", failed, lockEvents, auth.audits)
	}
}

func TestAuthorityLoginGlobalDelayIsCappedAtTenSeconds(t *testing.T) {
	auth := &fakeAuthorityAuth{
		passwordSet: true, password: "correct-password",
		failures: map[string]int{"203.0.113.10": 12},
		now:      time.Unix(1800000000, 0),
	}
	var slept time.Duration
	handler, _, err := NewAuthority(Config{
		Role: RoleServer, Servers: &stubServerSource{}, Access: &stubAccessStore{}, Auth: auth,
		CertificateFingerprint: "sha256:0123456789abcdef",
		AllowedHosts:           []string{"127.0.0.1"}, Port: 7879,
		Now:   func() time.Time { return auth.now },
		Sleep: func(delay time.Duration) { slept = delay },
	})
	if err != nil {
		t.Fatalf("构造权威端 Web UI 失败：%v", err)
	}
	page := authorityLoginPage(t, handler, "127.0.0.1:7879")
	cookies := page.Cookies()
	page.Body.Close()
	resp := authorityLoginPOST(t, handler, "127.0.0.1:7879", "correct-password", cookies...)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("其他 IP 的失败只应延迟成功登录，状态码 = %d，应为 303", resp.StatusCode)
	}
	if slept != 10*time.Second {
		t.Fatalf("全局登录延迟 = %v，应封顶 10 秒", slept)
	}
}
