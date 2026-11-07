package webui

import (
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// stubServerSource 是测试用的服务器列表来源，返回固定展示行。
type stubServerSource struct{ rows []ServerRow }

func (s *stubServerSource) Servers() ([]ServerRow, error) { return s.rows, nil }

// newTestHandler 构造一个固定端口的测试 Handler。所有请求都带正确的回环
// Host 头，与真实浏览器访问一致。
func newTestHandler(t *testing.T, role Role, rows []ServerRow) (http.Handler, Startup) {
	t.Helper()
	h, startup, err := New(Config{
		Role:        role,
		Servers:     &stubServerSource{rows: rows},
		VersionHint: func() string { return "" },
		Port:        18080,
	})
	if err != nil {
		t.Fatalf("构造 Web UI Handler 失败：%v", err)
	}
	return h, startup
}

// doGet 用指定 Host 与路径发起请求。
func doGet(t *testing.T, h http.Handler, host, target string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = host
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

// exchangeStartupToken 用启动网址换取会话 cookie。
func exchangeStartupToken(t *testing.T, h http.Handler, startupURL string) *http.Response {
	t.Helper()
	u, err := url.Parse(startupURL)
	if err != nil {
		t.Fatalf("解析启动网址失败：%v", err)
	}
	return doGet(t, h, u.Host, u.RequestURI())
}

func TestStartupTokenExchangesSessionAndRedirects(t *testing.T) {
	h, startup := newTestHandler(t, RoleStandalone, nil)
	startupURL := startup.URL("127.0.0.1:18080")
	if !strings.Contains(startupURL, "/ui/start?token=") {
		t.Fatalf("启动网址应指向启动入口并携带令牌：%s", startupURL)
	}

	resp := exchangeStartupToken(t, h, startupURL)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("有效令牌首次使用状态码 = %d，应为 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/ui/" {
		t.Fatalf("重定向地址 = %q，应为不带令牌的 /ui/", loc)
	}
	cookies := resp.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("应签发一个会话 cookie，实际 %d 个", len(cookies))
	}
	c := cookies[0]
	wantName := "sshmgr_ui_18080"
	if c.Name != wantName {
		t.Fatalf("cookie 名 = %q，应为包含端口的 %q", c.Name, wantName)
	}
	if !c.HttpOnly {
		t.Fatal("会话 cookie 必须是 HttpOnly")
	}
	if c.SameSite != http.SameSiteStrictMode {
		t.Fatalf("SameSite = %v，应为 Strict", c.SameSite)
	}
	if c.Path != "/ui/" {
		t.Fatalf("cookie Path = %q，应为 /ui/", c.Path)
	}
	if c.Domain != "" {
		t.Fatalf("cookie 不应设置 Domain，实际 %q", c.Domain)
	}
	if c.Secure {
		t.Fatal("回环模式是明文地址，cookie 不应设置 Secure")
	}
}

func TestStartupTokenIsOneTime(t *testing.T) {
	h, startup := newTestHandler(t, RoleStandalone, nil)
	startupURL := startup.URL("127.0.0.1:18080")

	first := exchangeStartupToken(t, h, startupURL)
	first.Body.Close()
	if first.StatusCode != http.StatusSeeOther {
		t.Fatalf("首次使用状态码 = %d，应为 303", first.StatusCode)
	}

	second := exchangeStartupToken(t, h, startupURL)
	defer second.Body.Close()
	if second.StatusCode != http.StatusForbidden {
		t.Fatalf("同一令牌第二次使用状态码 = %d，应为 403", second.StatusCode)
	}
	body, _ := io.ReadAll(second.Body)
	if !strings.Contains(string(body), "重新运行 `sshmgr ui`") {
		t.Fatalf("已用令牌的错误页应提示重新运行 `sshmgr ui`，实际：%s", body)
	}
}

func TestStartupTokenHEADDoesNotConsumeToken(t *testing.T) {
	h, startup := newTestHandler(t, RoleStandalone, nil)
	u, err := url.Parse(startup.URL("127.0.0.1:18080"))
	if err != nil {
		t.Fatalf("解析启动网址失败：%v", err)
	}

	head := httptest.NewRequest(http.MethodHead, u.RequestURI(), nil)
	head.Host = u.Host
	headRec := httptest.NewRecorder()
	h.ServeHTTP(headRec, head)
	headResp := headRec.Result()
	headResp.Body.Close()
	if headResp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("HEAD 访问启动入口状态码 = %d，应为 405", headResp.StatusCode)
	}
	if len(headResp.Cookies()) != 0 {
		t.Fatal("HEAD 访问启动入口不得签发会话 cookie")
	}

	get := doGet(t, h, u.Host, u.RequestURI())
	defer get.Body.Close()
	if get.StatusCode != http.StatusSeeOther {
		t.Fatalf("HEAD 之后启动令牌仍应可用，GET 状态码 = %d，应为 303", get.StatusCode)
	}
}

func TestStartupTokenSurvivesSessionGenerationFailure(t *testing.T) {
	h, startup := newTestHandler(t, RoleStandalone, nil)
	u, err := url.Parse(startup.URL("127.0.0.1:18080"))
	if err != nil {
		t.Fatalf("解析启动网址失败：%v", err)
	}

	prev := generateSessionToken
	calls := 0
	generateSessionToken = func() (string, error) {
		calls++
		if calls == 1 {
			return "", errors.New("第一段随机数生成失败")
		}
		return prev()
	}
	t.Cleanup(func() { generateSessionToken = prev })

	failed := doGet(t, h, u.Host, u.RequestURI())
	failed.Body.Close()
	if failed.StatusCode != http.StatusInternalServerError {
		t.Fatalf("会话生成失败时状态码 = %d，应为 500", failed.StatusCode)
	}
	if len(failed.Cookies()) != 0 {
		t.Fatal("会话生成失败时不得签发会话 cookie")
	}

	retried := doGet(t, h, u.Host, u.RequestURI())
	defer retried.Body.Close()
	if retried.StatusCode != http.StatusSeeOther {
		t.Fatalf("会话生成失败不得作废启动令牌，重试状态码 = %d，应为 303", retried.StatusCode)
	}
}

func TestStartupTokenWrongTokenRejected(t *testing.T) {
	h, _ := newTestHandler(t, RoleStandalone, nil)
	resp := doGet(t, h, "127.0.0.1:18080", "/ui/start?token=wrong-token")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("错误令牌状态码 = %d，应为 403", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "重新运行 `sshmgr ui`") {
		t.Fatalf("错误令牌的错误页应提示重新运行 `sshmgr ui`，实际：%s", body)
	}
}

func TestRejectedRequestsDoNotReadVersionHintOrRevealRole(t *testing.T) {
	versionHintCalls := 0
	h, startup, err := New(Config{
		Role: RoleClient,
		Servers: &stubServerSource{rows: []ServerRow{
			{Name: "secret-server", User: "u", Host: "192.0.2.1", Port: 22},
		}},
		VersionHint: func() string {
			versionHintCalls++
			return "客户端 1.0.0 落后于权威端 1.1.0，运行 `sshmgr update` 升级"
		},
		Port: 18080,
	})
	if err != nil {
		t.Fatalf("构造 Web UI Handler 失败：%v", err)
	}

	responses := []*http.Response{
		doGet(t, h, "evil.example:18080", "/ui/"),
		doGet(t, h, "127.0.0.1:18080", "/ui/"),
		doGet(t, h, "127.0.0.1:18080", "/ui/start?token=wrong-token"),
	}
	for _, resp := range responses {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		page := string(body)
		for _, leaked := range []string{
			"客户端 · 只读",
			"单机 · 全功能可写",
			"客户端 1.0.0 落后于权威端 1.1.0",
			"secret-server",
		} {
			if strings.Contains(page, leaked) {
				t.Fatalf("被拒绝的请求不得在错误页泄露 %q：\n%s", leaked, page)
			}
		}
	}
	if versionHintCalls != 0 {
		t.Fatalf("被拒绝的请求不得读取版本提示来源，实际读取 %d 次", versionHintCalls)
	}

	resp := exchangeStartupToken(t, h, startup.URL("127.0.0.1:18080"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("有效启动令牌状态码 = %d，应为 303", resp.StatusCode)
	}
}

func TestStartupTokenURLUsesGivenListenerAddress(t *testing.T) {
	_, startup := newTestHandler(t, RoleClient, nil)
	got := startup.URL("127.0.0.1:19527")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("启动网址非法：%v", err)
	}
	if u.Scheme != "http" || u.Host != "127.0.0.1:19527" {
		t.Fatalf("启动网址 scheme/host = %s/%s，应为 http/127.0.0.1:19527", u.Scheme, u.Host)
	}
	if u.Path != "/ui/start" {
		t.Fatalf("启动网址路径 = %q，应为 /ui/start", u.Path)
	}
	if u.Query().Get("token") == "" {
		t.Fatal("启动网址必须携带启动令牌")
	}
}

// doClientGet 经真实客户端发起请求，但把 Host 头固定为构造 Handler 时声明
// 的监听地址，等价于浏览器访问 127.0.0.1:18080。
func doClientGet(t *testing.T, client *http.Client, serverURL, pathAndQuery string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, serverURL+pathAndQuery, nil)
	if err != nil {
		t.Fatalf("构造请求失败：%v", err)
	}
	req.Host = "127.0.0.1:18080"
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("访问 %s 失败：%v", pathAndQuery, err)
	}
	return resp
}

// loginWithStartup 完成启动令牌换会话；客户端 cookieJar 会保留会话 cookie。
func loginWithStartup(t *testing.T, client *http.Client, serverURL string, startup Startup) {
	t.Helper()
	resp := doClientGet(t, client, serverURL, "/ui/start?token="+url.QueryEscape(startup.Token()))
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("启动令牌换会话状态码 = %d，应为 303", resp.StatusCode)
	}
}

func TestListPageRequiresSession(t *testing.T) {
	rows := []ServerRow{
		{ID: "s1", Name: "web-1", User: "root", Host: "192.0.2.10", Port: 22},
		{ID: "s2", Name: "数据库", User: "admin", Host: "192.0.2.20", Port: 22022},
	}
	h, startup := newTestHandler(t, RoleStandalone, rows)

	noCookie := doGet(t, h, "127.0.0.1:18080", "/ui/")
	defer noCookie.Body.Close()
	if noCookie.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无会话访问列表页状态码 = %d，应为 401", noCookie.StatusCode)
	}
	body, _ := io.ReadAll(noCookie.Body)
	if !strings.Contains(string(body), "重新运行 `sshmgr ui`") {
		t.Fatalf("无会话应得到重新运行提示，实际：%s", body)
	}
	for _, name := range []string{"web-1", "数据库", "192.0.2.10"} {
		if strings.Contains(string(body), name) {
			t.Fatalf("无会话响应不得泄露服务器信息：%s", body)
		}
	}

	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("构造 cookie 保存器失败：%v", err)
	}
	client := &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	loginWithStartup(t, client, server.URL, startup)

	resp := doClientGet(t, client, server.URL, "/ui/")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("有效会话访问列表页状态码 = %d，应为 200", resp.StatusCode)
	}
	page, _ := io.ReadAll(resp.Body)
	text := string(page)
	for _, want := range []string{"web-1", "root@192.0.2.10", "数据库", "admin@192.0.2.20", "22022"} {
		if !strings.Contains(text, want) {
			t.Fatalf("列表页缺少 %q：\n%s", want, text)
		}
	}
}

func TestListPageRejectsWrongCookie(t *testing.T) {
	h, _ := newTestHandler(t, RoleStandalone, []ServerRow{{Name: "web-1"}})
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)

	req, _ := http.NewRequest(http.MethodGet, server.URL+"/ui/", nil)
	req.Host = "127.0.0.1:18080"
	req.AddCookie(&http.Cookie{Name: "sshmgr_ui_18080", Value: "not-a-session"})
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("访问列表页失败：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错误会话访问列表页状态码 = %d，应为 401", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "web-1") {
		t.Fatalf("错误会话响应不得泄露服务器信息：%s", body)
	}
}

func TestSessionCookiesOfDifferentPortsDoNotCollide(t *testing.T) {
	if sessionCookieName(18080) == sessionCookieName(18081) {
		t.Fatal("两个端口的会话 cookie 名必须不同，否则同机实例互相覆盖")
	}
}

func TestHostValidationRejectsNonLoopbackHosts(t *testing.T) {
	h, startup := newTestHandler(t, RoleStandalone, []ServerRow{{Name: "web-1"}})

	for _, host := range []string{
		"127.0.0.1.evil.example",
		"127.0.0.1.evil.example:18080",
		"127.0.0.1",
		"127.0.0.1:18081",
		"localhost:18081",
		"[::1]:18081",
		"192.168.1.5:18080",
		"example.com:18080",
	} {
		resp := doGet(t, h, host, "/ui/")
		resp.Body.Close()
		if resp.StatusCode != http.StatusMisdirectedRequest && resp.StatusCode != http.StatusForbidden {
			t.Fatalf("Host %q 访问列表页状态码 = %d，应被拒绝", host, resp.StatusCode)
		}
	}

	// 启动入口同样必须校验 Host：有效令牌也不能经非回环 Host 换到会话。
	startupURL := startup.URL("127.0.0.1:18080")
	resp := exchangeStartupTokenWithHost(t, h, "127.0.0.1.evil.example:18080", startupURL)
	resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest && resp.StatusCode != http.StatusForbidden {
		t.Fatalf("非回环 Host 访问启动入口状态码 = %d，应被拒绝", resp.StatusCode)
	}
	if len(resp.Cookies()) != 0 {
		t.Fatal("非回环 Host 的启动入口请求不得签发会话 cookie")
	}
}

func TestHostValidationAllowsLoopbackHostsWithConfiguredPort(t *testing.T) {
	// 启动令牌是一次性的：每个回环 Host 用一个全新 Handler 验证。
	for _, host := range []string{"127.0.0.1:18080", "localhost:18080", "[::1]:18080"} {
		h, startup := newTestHandler(t, RoleStandalone, nil)
		u, err := url.Parse(startup.URL(host))
		if err != nil {
			t.Fatalf("解析启动网址失败：%v", err)
		}
		token := u.Query().Get("token")
		resp := doGet(t, h, host, startPath+"?token="+url.QueryEscape(token))
		resp.Body.Close()
		if resp.StatusCode == http.StatusMisdirectedRequest || resp.StatusCode == http.StatusForbidden {
			t.Fatalf("回环 Host %q 不应被拒绝", host)
		}
	}
}

// exchangeStartupTokenWithHost 用指定 Host 打启动网址。
func exchangeStartupTokenWithHost(t *testing.T, h http.Handler, host, startupURL string) *http.Response {
	t.Helper()
	u, err := url.Parse(startupURL)
	if err != nil {
		t.Fatalf("解析启动网址失败：%v", err)
	}
	return doGet(t, h, host, u.RequestURI())
}

// assertSecurityHeaders 断言所有响应都必须携带的安全头。
func assertSecurityHeaders(t *testing.T, resp *http.Response, where string) {
	t.Helper()
	wantCSP := "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; font-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
	if got := resp.Header.Get("Content-Security-Policy"); got != wantCSP {
		t.Fatalf("%s 的内容安全策略 = %q，应为 %q", where, got, wantCSP)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("%s 的 X-Content-Type-Options = %q，应为 nosniff", where, got)
	}
	if got := resp.Header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("%s 的 Referrer-Policy = %q，应为 no-referrer", where, got)
	}
}

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	h, _ := newTestHandler(t, RoleStandalone, []ServerRow{{Name: "web-1"}})

	full := doGet(t, h, "127.0.0.1:18080", "/ui/")
	defer full.Body.Close()
	assertSecurityHeaders(t, full, "整页")
	if got := full.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("整页 Cache-Control = %q，应为 no-store", got)
	}

	fragReq := httptest.NewRequest(http.MethodGet, "/ui/", nil)
	fragReq.Host = "127.0.0.1:18080"
	fragReq.Header.Set("HX-Request", "true")
	fragRec := httptest.NewRecorder()
	h.ServeHTTP(fragRec, fragReq)
	fragment := fragRec.Result()
	fragment.Body.Close()
	assertSecurityHeaders(t, fragment, "片段")

	badToken := doGet(t, h, "127.0.0.1:18080", "/ui/start?token=bad")
	badToken.Body.Close()
	assertSecurityHeaders(t, badToken, "错误页")

	noSession := doGet(t, h, "127.0.0.1:18080", "/ui/unknown")
	noSession.Body.Close()
	assertSecurityHeaders(t, noSession, "无会话错误页")

	badHost := doGet(t, h, "evil.example:18080", "/ui/")
	badHost.Body.Close()
	assertSecurityHeaders(t, badHost, "Host 被拒响应")
}

func TestFragmentVersusFullPage(t *testing.T) {
	h, startup := newTestHandler(t, RoleStandalone, []ServerRow{{Name: "web-1"}})

	full := exchangeStartupToken(t, h, startup.URL("127.0.0.1:18080"))
	var cookie *http.Cookie
	if len(full.Cookies()) == 1 {
		cookie = full.Cookies()[0]
	}
	full.Body.Close()
	if cookie == nil {
		t.Fatal("启动令牌换会话后应得到会话 cookie")
	}

	fullReq := httptest.NewRequest(http.MethodGet, "/ui/", nil)
	fullReq.Host = "127.0.0.1:18080"
	fullReq.AddCookie(cookie)
	fullRec := httptest.NewRecorder()
	h.ServeHTTP(fullRec, fullReq)
	if !strings.Contains(fullRec.Body.String(), "<html") {
		t.Fatalf("普通请求应返回整页：\n%s", fullRec.Body.String())
	}

	fragReq := httptest.NewRequest(http.MethodGet, "/ui/", nil)
	fragReq.Host = "127.0.0.1:18080"
	fragReq.AddCookie(cookie)
	fragReq.Header.Set("HX-Request", "true")
	fragRec := httptest.NewRecorder()
	h.ServeHTTP(fragRec, fragReq)
	fragment := fragRec.Body.String()
	if strings.Contains(fragment, "<html") {
		t.Fatalf("htmx 请求不应包含布局外壳：\n%s", fragment)
	}
	if !strings.Contains(fragment, "web-1") {
		t.Fatalf("片段仍应包含列表内容：\n%s", fragment)
	}
}

// loginAndGetList 换会话后取列表页全文。
func loginAndGetList(t *testing.T, role Role, rows []ServerRow, versionHint string) string {
	t.Helper()
	h, startup, err := New(Config{
		Role:        role,
		Servers:     &stubServerSource{rows: rows},
		VersionHint: func() string { return versionHint },
		Port:        18080,
	})
	if err != nil {
		t.Fatalf("构造 Web UI Handler 失败：%v", err)
	}
	resp := exchangeStartupToken(t, h, startup.URL("127.0.0.1:18080"))
	var cookie *http.Cookie
	if len(resp.Cookies()) == 1 {
		cookie = resp.Cookies()[0]
	}
	resp.Body.Close()
	if cookie == nil {
		t.Fatal("启动令牌换会话后应得到会话 cookie")
	}
	req := httptest.NewRequest(http.MethodGet, "/ui/", nil)
	req.Host = "127.0.0.1:18080"
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Body.String()
}

func TestStandaloneRoleShowsWritableBadgeWithoutReadOnlyNotice(t *testing.T) {
	page := loginAndGetList(t, RoleStandalone, []ServerRow{{Name: "web-1"}}, "")
	if !strings.Contains(page, "单机 · 全功能可写") {
		t.Fatalf("单机角色应显示全功能可写标记：\n%s", page)
	}
	if strings.Contains(page, "只读") {
		t.Fatalf("单机角色不应显示只读提示：\n%s", page)
	}
}

func TestClientRoleShowsReadOnlyNoticeWithoutWriteEntries(t *testing.T) {
	page := loginAndGetList(t, RoleClient, []ServerRow{{Name: "web-1"}}, "")
	if !strings.Contains(page, "客户端 · 只读") {
		t.Fatalf("客户端角色应显示只读角色标记：\n%s", page)
	}
	if !strings.Contains(page, "这里只读，修改需到权威端 Web UI 去做") {
		t.Fatalf("客户端角色应显示权威端指引：\n%s", page)
	}
	for _, word := range []string{"编辑", "删除", "新增", "添加"} {
		if strings.Contains(page, word) {
			t.Fatalf("客户端角色页面不得出现写操作入口 %q：\n%s", word, page)
		}
	}
}

func TestVersionHintShownOnlyWhenPresent(t *testing.T) {
	shown := loginAndGetList(t, RoleClient, nil, "客户端 1.0.0 落后于权威端 1.1.0，运行 `sshmgr update` 升级")
	if !strings.Contains(shown, "客户端 1.0.0 落后于权威端 1.1.0") {
		t.Fatalf("版本提示来源非空时页面应显示该文字：\n%s", shown)
	}
	hidden := loginAndGetList(t, RoleClient, nil, "")
	if strings.Contains(hidden, "落后于") || strings.Contains(hidden, "比权威端新") {
		t.Fatalf("版本提示来源为空串时页面不应显示提示：\n%s", hidden)
	}
}

func TestNeedsAttentionRowsSortFirstAndAreMarked(t *testing.T) {
	page := loginAndGetList(t, RoleStandalone, []ServerRow{
		{ID: "ok", Name: "正常服务器", User: "u", Host: "192.0.2.1", Port: 22},
		{ID: "warn", Name: "待处理服务器", User: "u", Host: "192.0.2.2", Port: 22, NeedsAttention: true},
		{ID: "ok2", Name: "正常服务器二", User: "u", Host: "192.0.2.3", Port: 22},
	}, "")
	warnPos := strings.Index(page, "待处理服务器")
	firstOK := strings.Index(page, "正常服务器")
	secondOK := strings.Index(page, "正常服务器二")
	if warnPos < 0 || firstOK < 0 || secondOK < 0 {
		t.Fatalf("列表页应包含全部服务器名称：\n%s", page)
	}
	if !(warnPos < firstOK && warnPos < secondOK) {
		t.Fatalf("需要注意的服务器应排在最前：\n%s", page)
	}
	if !strings.Contains(page, "需要注意") {
		t.Fatalf("需要注意的服务器应有醒目标记：\n%s", page)
	}
}

func TestServerNamesAreHTMLEscaped(t *testing.T) {
	page := loginAndGetList(t, RoleStandalone, []ServerRow{
		{Name: "<script>alert(1)</script>", User: "u", Host: "192.0.2.1", Port: 22},
	}, "")
	if strings.Contains(page, "<script>alert(1)</script>") {
		t.Fatalf("服务器名称必须转义输出：\n%s", page)
	}
	if !strings.Contains(page, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Fatalf("转义后的服务器名称应出现在页面中：\n%s", page)
	}
}

func TestUnknownUIPathReturnsChinese404WithSession(t *testing.T) {
	h, startup := newTestHandler(t, RoleStandalone, []ServerRow{{Name: "web-1"}})
	resp := exchangeStartupToken(t, h, startup.URL("127.0.0.1:18080"))
	var cookie *http.Cookie
	if len(resp.Cookies()) == 1 {
		cookie = resp.Cookies()[0]
	}
	resp.Body.Close()
	if cookie == nil {
		t.Fatal("启动令牌换会话后应得到会话 cookie")
	}
	req := httptest.NewRequest(http.MethodGet, "/ui/not-exist", nil)
	req.Host = "127.0.0.1:18080"
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("已登录访问未知地址状态码 = %d，应为 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "页面不存在") {
		t.Fatalf("未知地址应返回中文 404 页：%s", rec.Body.String())
	}
}
