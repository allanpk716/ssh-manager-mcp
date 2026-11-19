package webui

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func newAuthorityForManualClientTest(t *testing.T, auth *fakeAuthorityAuth, store *stubAccessStore) http.Handler {
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

func manualClientTestStore() *stubAccessStore {
	return &stubAccessStore{profiles: []AccessProfileSummary{
		{ID: "profile-1", Name: "运维"}, {ID: "profile-2", Name: "开发"},
	}}
}

func TestAuthorityManualClientPageAndIssue(t *testing.T) {
	auth := &fakeAuthorityAuth{passwordSet: true, password: "correct-password", now: time.Unix(1800000000, 0)}
	store := manualClientTestStore()
	handler := newAuthorityForManualClientTest(t, auth, store)
	cookie := authoritySuccessfulLogin(t, handler, "192.0.2.50:7879", "correct-password")

	page := authorityGet(t, handler, "192.0.2.50:7879", "/ui/ops/clients/new", cookie)
	body := readResponseBody(t, page)
	if page.StatusCode != http.StatusOK {
		t.Fatalf("手动添加客户端状态码=%d 页面：\n%s", page.StatusCode, body)
	}
	for _, want := range []string{"<h1>手动添加客户端</h1>", `name="name"`, `name="profile"`, "高级操作"} {
		if !strings.Contains(body, want) {
			t.Fatalf("手动添加客户端页应包含 %q：\n%s", want, body)
		}
	}
	for _, forbidden := range []string{"设备码", "项目令牌", "站点", "网络成员", "登录条目", "兜底主机", "cache token"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("手动添加客户端页不应出现 %q：\n%s", forbidden, body)
		}
	}
	csrfToken := hiddenInputValue(t, body, "csrf_token")

	issued := authorityPostForm(t, handler, "192.0.2.50:7879", "/ui/ops/clients/new", url.Values{
		"csrf_token": {csrfToken}, "name": {"笔记本"}, "profile": {"profile-1"},
	}, cookie)
	issuedBody := readResponseBody(t, issued)
	if issued.StatusCode != http.StatusOK || issued.Header.Get("Cache-Control") != "no-store" ||
		!strings.Contains(issuedBody, "客户端接入凭据已生成") {
		t.Fatalf("手动添加客户端结果状态码=%d 缓存=%q 页面：\n%s",
			issued.StatusCode, issued.Header.Get("Cache-Control"), issuedBody)
	}
	if store.addDeviceName != "笔记本" || store.addDeviceProfile != "profile-1" {
		t.Fatalf("签发参数不正确：%+v", store)
	}
}

func TestAuthorityManualClientOldRouteRedirects(t *testing.T) {
	auth := &fakeAuthorityAuth{passwordSet: true, password: "correct-password", now: time.Unix(1800000000, 0)}
	handler := newAuthorityForManualClientTest(t, auth, manualClientTestStore())
	cookie := authoritySuccessfulLogin(t, handler, "192.0.2.50:7879", "correct-password")

	response := authorityGet(t, handler, "192.0.2.50:7879", "/ui/access/device-codes?name=x", cookie)
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/ui/ops/clients/new?name=x" {
		t.Fatalf("旧手动添加路由应跳转新页面，状态码=%d Location=%q",
			response.StatusCode, response.Header.Get("Location"))
	}
}

func TestAuthorityManualClientOldRevokeRouteRedirects(t *testing.T) {
	auth := &fakeAuthorityAuth{passwordSet: true, password: "correct-password", now: time.Unix(1800000000, 0)}
	handler := newAuthorityForManualClientTest(t, auth, manualClientTestStore())
	cookie := authoritySuccessfulLogin(t, handler, "192.0.2.50:7879", "correct-password")

	response := authorityGet(t, handler, "192.0.2.50:7879", "/ui/access/device-codes/client-1/revoke?return_to=x", cookie)
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/ui/clients/client-1/remove?return_to=x" {
		t.Fatalf("旧撤销路由应跳转移除客户端页，状态码=%d Location=%q",
			response.StatusCode, response.Header.Get("Location"))
	}
}
