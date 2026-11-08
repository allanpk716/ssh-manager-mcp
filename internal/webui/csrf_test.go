package webui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// csrfTestEnv 建立一个带会话的测试环境，并在 Handler 内部路由表上挂一个无
// 副作用的测试写路由，用来验证所有写请求共用的中间件。
type csrfTestEnv struct {
	h         http.Handler
	handler   *handler
	startup   Startup
	cookie    *http.Cookie
	csrfToken string
}

func newCSRFTestEnv(t *testing.T) *csrfTestEnv {
	t.Helper()
	h, startup, err := New(Config{
		Role:        RoleStandalone,
		Servers:     &stubServerSource{},
		Access:      &stubAccessStore{},
		VersionHint: func() string { return "" },
		Port:        18080,
	})
	if err != nil {
		t.Fatalf("构造 Web UI Handler 失败：%v", err)
	}
	env := &csrfTestEnv{h: h, handler: h.(*handler), startup: startup}
	env.handler.mux.HandleFunc("/ui/test-write", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	resp := exchangeStartupToken(t, h, startup.URL("127.0.0.1:18080"))
	if len(resp.Cookies()) != 1 {
		resp.Body.Close()
		t.Fatal("启动令牌换会话后应得到一个会话 cookie")
	}
	env.cookie = resp.Cookies()[0]
	resp.Body.Close()

	page := httptest.NewRequest(http.MethodGet, "/ui/", nil)
	page.Host = "127.0.0.1:18080"
	page.AddCookie(env.cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, page)
	re := regexp.MustCompile(`<meta name="csrf-token" content="([^"]+)"`)
	m := re.FindStringSubmatch(rec.Body.String())
	if m == nil {
		t.Fatalf("整页应包含防跨站请求伪造令牌 meta：\n%s", rec.Body.String())
	}
	env.csrfToken = m[1]
	return env
}

// postWriteRoute 用指定 Origin 与令牌发写请求。
func (e *csrfTestEnv) postWriteRoute(t *testing.T, origin, csrfHeader, csrfForm string) *http.Response {
	t.Helper()
	var body string
	if csrfForm != "" {
		body = url.Values{"csrf_token": {csrfForm}}.Encode()
	}
	req := httptest.NewRequest(http.MethodPost, "/ui/test-write", strings.NewReader(body))
	req.Host = "127.0.0.1:18080"
	req.AddCookie(e.cookie)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if csrfHeader != "" {
		req.Header.Set("X-CSRF-Token", csrfHeader)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec.Result()
}

func TestWriteRouteRequiresMatchingOriginAndCSRFToken(t *testing.T) {
	cases := []struct {
		name       string
		origin     string
		csrfHeader string
		csrfForm   string
		want       int
	}{
		{"缺少 Origin", "", "correct", "", http.StatusForbidden},
		{"Origin 不符", "https://evil.example", "correct", "", http.StatusForbidden},
		{"Origin 端口不符", "http://127.0.0.1:18081", "correct", "", http.StatusForbidden},
		{"缺少令牌", "http://127.0.0.1:18080", "", "", http.StatusForbidden},
		{"令牌错误", "http://127.0.0.1:18080", "wrong", "", http.StatusForbidden},
		{"表单令牌正确", "http://127.0.0.1:18080", "", "correct", http.StatusNoContent},
	}
	for _, tc := range cases {
		env := newCSRFTestEnv(t)
		header, form := tc.csrfHeader, tc.csrfForm
		if header == "correct" {
			header = env.csrfToken
		}
		if form == "correct" {
			form = env.csrfToken
		}
		resp := env.postWriteRoute(t, tc.origin, header, form)
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Fatalf("%s：状态码 = %d，应为 %d", tc.name, resp.StatusCode, tc.want)
		}
	}
}

func TestCSRFTokenBoundToSession(t *testing.T) {
	first := newCSRFTestEnv(t)
	second := newCSRFTestEnv(t)

	resp := first.postWriteRoute(t, "http://127.0.0.1:18080", second.csrfToken, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("另一个会话的令牌状态码 = %d，应为 403", resp.StatusCode)
	}

	resp = first.postWriteRoute(t, "http://127.0.0.1:18080", first.csrfToken, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("本会话令牌状态码 = %d，应为 204", resp.StatusCode)
	}
}

func TestOriginAllowedFromEveryConfiguredLoopbackOrigin(t *testing.T) {
	env := newCSRFTestEnv(t)

	for _, origin := range []string{
		"http://127.0.0.1:18080",
		"http://localhost:18080",
		"http://[::1]:18080",
	} {
		req := httptest.NewRequest(http.MethodPost, "/ui/test-write", nil)
		req.Host = "127.0.0.1:18080"
		req.AddCookie(env.cookie)
		req.Header.Set("Origin", origin)
		req.Header.Set("X-CSRF-Token", env.csrfToken)
		rec := httptest.NewRecorder()
		env.h.ServeHTTP(rec, req)
		resp := rec.Result()
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("配置端口对应的回环来源 %q 应被接受，状态码 = %d", origin, resp.StatusCode)
		}
	}
}

func TestWriteRouteWithoutSessionRejected(t *testing.T) {
	h, _, err := New(Config{
		Role:        RoleStandalone,
		Servers:     &stubServerSource{},
		Access:      &stubAccessStore{},
		VersionHint: func() string { return "" },
		Port:        18080,
	})
	if err != nil {
		t.Fatalf("构造 Web UI Handler 失败：%v", err)
	}
	h.(*handler).mux.HandleFunc("/ui/test-write", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodPost, "/ui/test-write", nil)
	req.Host = "127.0.0.1:18080"
	req.Header.Set("Origin", "http://127.0.0.1:18080")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无会话写请求状态码 = %d，应为 401", rec.Code)
	}
}

func TestAllWriteMethodsRequireOriginAndCSRFToken(t *testing.T) {
	env := newCSRFTestEnv(t)

	cases := []struct {
		name       string
		origin     string
		csrfHeader string
	}{
		{"缺少 Origin", "", "correct"},
		{"缺少防跨站请求伪造令牌", "http://127.0.0.1:18080", ""},
	}
	for _, tc := range cases {
		token := tc.csrfHeader
		if token == "correct" {
			token = env.csrfToken
		}
		req := httptest.NewRequest(http.MethodPost, "/ui/", nil)
		req.Host = "127.0.0.1:18080"
		req.AddCookie(env.cookie)
		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}
		if token != "" {
			req.Header.Set("X-CSRF-Token", token)
		}
		rec := httptest.NewRecorder()
		env.h.ServeHTTP(rec, req)
		resp := rec.Result()
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s：POST /ui/ 状态码 = %d，应为 403", tc.name, resp.StatusCode)
		}
	}
}
