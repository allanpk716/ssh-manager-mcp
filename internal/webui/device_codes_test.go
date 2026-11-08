package webui

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// newAuthorityForDeviceCodeTest 构造带设备码测试数据的权威端处理器。
func newAuthorityForDeviceCodeTest(t *testing.T, auth *fakeAuthorityAuth, store *stubAccessStore) http.Handler {
	t.Helper()
	handler, _, err := NewAuthority(Config{
		Role: RoleServer, Servers: &stubServerSource{}, Access: store, Auth: auth,
		AllowedHosts: []string{"192.0.2.50"}, Port: 7879, ServePort: 8443,
		CertificateFingerprint: "sha256:0123456789abcdef",
		Now:                    func() time.Time { return auth.now },
	})
	if err != nil {
		t.Fatalf("构造权威端 Web UI Handler 失败：%v", err)
	}
	return handler
}

func deviceCodeTestStore() *stubAccessStore {
	return &stubAccessStore{
		profiles: []AccessProfileSummary{
			{ID: "profile-1", Name: "运维"},
			{ID: "profile-2", Name: "开发"},
		},
		deviceCodes: []AccessDeviceCodeSummary{
			{
				ID: "device-1", Name: "笔记本", TokenPrefix: "abcd1234", Status: "active",
				ProfileName: "运维", LastPullAt: time.Unix(1799990000, 0), ClientVersion: "v1.2.3",
			},
			{
				ID: "device-2", Name: "台式机", TokenPrefix: "wxyz9876", Status: "revoked",
				ProfileName: "开发",
			},
		},
	}
}

func TestAuthorityDeviceCodePageListsRowsAndIssueForm(t *testing.T) {
	auth := &fakeAuthorityAuth{passwordSet: true, password: "correct-password", now: time.Unix(1800000000, 0)}
	handler := newAuthorityForDeviceCodeTest(t, auth, deviceCodeTestStore())
	cookie := authoritySuccessfulLogin(t, handler, "192.0.2.50:7879", "correct-password")

	page := authorityGet(t, handler, "192.0.2.50:7879", "/ui/access/device-codes", cookie)
	body := readResponseBody(t, page)
	if page.StatusCode != http.StatusOK {
		t.Fatalf("设备码页状态码=%d 页面：\n%s", page.StatusCode, body)
	}
	for _, want := range []string{
		`<a href="/ui/access/device-codes">设备码</a>`,
		"笔记本", "abcd1234", "活跃", "运维", "v1.2.3",
		"台式机", "wxyz9876", "已撤销", "开发",
		`<option value="profile-1">运维</option>`,
		`<option value="profile-2">开发</option>`,
		`name="name"`, `name="profile"`,
		`<a href="/ui/access/device-codes/device-1/revoke">撤销</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("设备码页应包含 %q：\n%s", want, body)
		}
	}
	if strings.Contains(body, "/ui/access/device-codes/device-2/revoke") {
		t.Fatalf("已撤销的设备码不应再显示撤销入口：\n%s", body)
	}
}

func TestAuthorityDeviceCodeIssueRequiresReverifyAndShowsOnce(t *testing.T) {
	auth := &fakeAuthorityAuth{passwordSet: true, password: "correct-password", now: time.Unix(1800000000, 0)}
	store := deviceCodeTestStore()
	handler := newAuthorityForDeviceCodeTest(t, auth, store)
	cookie := authoritySuccessfulLogin(t, handler, "192.0.2.50:7879", "correct-password")
	auth.now = auth.now.Add(6 * time.Minute)

	page := authorityGet(t, handler, "192.0.2.50:7879", "/ui/access/device-codes", cookie)
	body := readResponseBody(t, page)
	wantReverifyLink := `/ui/reverify?next=` + url.QueryEscape("/ui/access/device-codes")
	if !strings.Contains(body, wantReverifyLink) {
		t.Fatalf("未重新验证的设备码页应链接重新验证页 %q：\n%s", wantReverifyLink, body)
	}
	csrfToken := hiddenInputValue(t, body, "csrf_token")

	rejected := authorityPostForm(t, handler, "192.0.2.50:7879", "/ui/access/device-codes", url.Values{
		"csrf_token": {csrfToken}, "name": {"笔记本"}, "profile": {"profile-1"},
	}, cookie)
	rejectedBody := readResponseBody(t, rejected)
	resumePath := "/ui/access/device-codes?name=" + url.QueryEscape("笔记本") + "&profile=profile-1"
	wantRedirect := `/ui/reverify?next=` + url.QueryEscape(resumePath)
	if rejected.StatusCode != http.StatusSeeOther || rejected.Header.Get("Location") != wantRedirect {
		t.Fatalf("签发设备码应先重新验证，状态码=%d 跳转=%q 页面：\n%s", rejected.StatusCode, rejected.Header.Get("Location"), rejectedBody)
	}
	if store.addDeviceName != "" {
		t.Fatalf("未重新验证时不应产生设备码，实际调用 name=%q", store.addDeviceName)
	}

	reverifyPage := authorityGet(t, handler, "192.0.2.50:7879", wantReverifyLink, cookie)
	reverifyBody := readResponseBody(t, reverifyPage)
	reverifyCSRF := hiddenInputValue(t, reverifyBody, "csrf_token")
	reverified := authorityPostForm(t, handler, "192.0.2.50:7879", "/ui/reverify", url.Values{
		"csrf_token": {reverifyCSRF}, "password": {"correct-password"}, "next": {resumePath},
	}, cookie)
	reverifiedBody := readResponseBody(t, reverified)
	if reverified.StatusCode != http.StatusSeeOther || reverified.Header.Get("Location") != resumePath {
		t.Fatalf("重新验证后应回到设备码页，状态码=%d 跳转=%q 页面：\n%s", reverified.StatusCode, reverified.Header.Get("Location"), reverifiedBody)
	}

	resumed := authorityGet(t, handler, "192.0.2.50:7879", resumePath, cookie)
	resumedBody := readResponseBody(t, resumed)
	if !strings.Contains(resumedBody, `value="笔记本"`) || !strings.Contains(resumedBody, `<option value="profile-1" selected>`) {
		t.Fatalf("重新验证后应保留设备名与 profile 选择：\n%s", resumedBody)
	}
	resumedCSRF := hiddenInputValue(t, resumedBody, "csrf_token")

	store.addDeviceResult = AccessDeviceCodeResult{
		Name: "笔记本", Code: "one-time-code", Fingerprint: "sha256:abcdef",
		PullCommand: "sshmgr cache pull --url https://192.0.2.50:8443 --token 'one-time-code:sha256:abcdef'",
	}
	issued := authorityPostForm(t, handler, "192.0.2.50:7879", "/ui/access/device-codes", url.Values{
		"csrf_token": {resumedCSRF}, "name": {" 笔记本 "}, "profile": {"profile-1"},
	}, cookie)
	issuedBody := readResponseBody(t, issued)
	if issued.StatusCode != http.StatusOK {
		t.Fatalf("签发设备码应在本次响应里显示一次性页面，状态码=%d 页面：\n%s", issued.StatusCode, issuedBody)
	}
	if cache := issued.Header.Get("Cache-Control"); cache != "no-store" {
		t.Fatalf("一次性页面应带 Cache-Control: no-store，实际 %q", cache)
	}
	for _, want := range []string{
		"one-time-code", "sha256:abcdef",
		"cache pull --url https://192.0.2.50:8443",
		"one-time-code:sha256:abcdef",
		"只显示这一次",
	} {
		if !strings.Contains(issuedBody, want) {
			t.Fatalf("一次性页面应包含 %q：\n%s", want, issuedBody)
		}
	}
	if store.addDeviceName != "笔记本" || store.addDeviceProfile != "profile-1" ||
		store.addDeviceURL != "https://192.0.2.50:8443" || store.addDeviceSrc.RemoteAddress != "198.51.100.9" {
		t.Fatalf("签发参数不正确：name=%q profile=%q url=%q source=%+v",
			store.addDeviceName, store.addDeviceProfile, store.addDeviceURL, store.addDeviceSrc)
	}

	after := authorityGet(t, handler, "192.0.2.50:7879", "/ui/access/device-codes", cookie)
	afterBody := readResponseBody(t, after)
	if strings.Contains(afterBody, "one-time-code") {
		t.Fatalf("设备码不应再出现在普通列表页：\n%s", afterBody)
	}
}

func TestAuthorityDeviceCodeRevokeUsesConfirmationWithoutReverify(t *testing.T) {
	auth := &fakeAuthorityAuth{passwordSet: true, password: "correct-password", now: time.Unix(1800000000, 0)}
	store := deviceCodeTestStore()
	handler := newAuthorityForDeviceCodeTest(t, auth, store)
	cookie := authoritySuccessfulLogin(t, handler, "192.0.2.50:7879", "correct-password")
	auth.now = auth.now.Add(6 * time.Minute)

	confirmPage := authorityGet(t, handler, "192.0.2.50:7879", "/ui/access/device-codes/device-1/revoke", cookie)
	confirmBody := readResponseBody(t, confirmPage)
	if confirmPage.StatusCode != http.StatusOK || !strings.Contains(confirmBody, "确认撤销") || !strings.Contains(confirmBody, "笔记本") {
		t.Fatalf("撤销应先显示确认页，状态码=%d 页面：\n%s", confirmPage.StatusCode, confirmBody)
	}
	csrfToken := hiddenInputValue(t, confirmBody, "csrf_token")

	revoked := authorityPostForm(t, handler, "192.0.2.50:7879", "/ui/access/device-codes/device-1/revoke", url.Values{
		"csrf_token": {csrfToken},
	}, cookie)
	revokedBody := readResponseBody(t, revoked)
	if revoked.StatusCode != http.StatusSeeOther || revoked.Header.Get("Location") != "/ui/access/device-codes" {
		t.Fatalf("撤销后应回到设备码页，状态码=%d 跳转=%q 页面：\n%s", revoked.StatusCode, revoked.Header.Get("Location"), revokedBody)
	}
	if store.revokeDeviceID != "device-1" || store.revokeDeviceSrc.RemoteAddress != "198.51.100.9" {
		t.Fatalf("撤销参数不正确：id=%q source=%+v", store.revokeDeviceID, store.revokeDeviceSrc)
	}

	unknown := authorityGet(t, handler, "192.0.2.50:7879", "/ui/access/device-codes/no-such/revoke", cookie)
	unknownBody := readResponseBody(t, unknown)
	if unknown.StatusCode != http.StatusNotFound {
		t.Fatalf("撤销不存在的设备码应 404，状态码=%d 页面：\n%s", unknown.StatusCode, unknownBody)
	}
}

func TestDeviceCodeRoutesMissingOnNonAuthorityRoles(t *testing.T) {
	standalone, standaloneStartup, err := New(Config{
		Role: RoleStandalone, Servers: &stubServerSource{}, Access: &stubAccessStore{}, Port: 18080,
	})
	if err != nil {
		t.Fatalf("构造单机 Web UI Handler 失败：%v", err)
	}
	standaloneSession := exchangeStartupToken(t, standalone, standaloneStartup.URL("127.0.0.1:18080"))
	standaloneCookie := standaloneSession.Cookies()[0]
	standaloneSession.Body.Close()

	client, clientStartup, err := New(Config{Role: RoleClient, Servers: &stubServerSource{}, Port: 18080})
	if err != nil {
		t.Fatalf("构造客户端 Web UI Handler 失败：%v", err)
	}
	clientSession := exchangeStartupToken(t, client, clientStartup.URL("127.0.0.1:18080"))
	clientCookie := clientSession.Cookies()[0]
	clientSession.Body.Close()

	for _, item := range []struct {
		handler http.Handler
		cookie  *http.Cookie
	}{{standalone, standaloneCookie}, {client, clientCookie}} {
		for _, target := range []string{
			"/ui/access/device-codes", "/ui/access/device-codes/device-1/revoke",
		} {
			resp := doGetWithCookie(t, item.handler, item.cookie, target)
			resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("%s 不应存在，状态码=%d", target, resp.StatusCode)
			}
		}
	}
	clientNotice := doGetWithCookie(t, client, clientCookie, "/ui/access/")
	clientNoticeBody := readResponseBody(t, clientNotice)
	clientCSRF := hiddenInputMetaOrHiddenValue(t, clientNoticeBody)
	for _, target := range []string{"/ui/access/device-codes", "/ui/access/device-codes/device-1/revoke"} {
		resp := postAccessWrite(t, client, clientCookie, target, url.Values{}, "http://127.0.0.1:18080", clientCSRF)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("客户端设备码写路由 %s 应返回 404，状态码=%d", target, resp.StatusCode)
		}
	}
}
