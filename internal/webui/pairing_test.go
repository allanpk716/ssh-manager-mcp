package webui

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func pairingTestStore() *stubAccessStore {
	return &stubAccessStore{
		profiles: []AccessProfileSummary{
			{ID: "profile-ops", Name: "运维"},
			{ID: "profile-dev", Name: "开发"},
		},
		pairings: []AccessPairingSummary{
			{
				ID: "pair-1", Name: "笔记本", TargetURL: "https://127.0.0.1:7878",
				ShortCode: "314159", SourceAddress: "192.0.2.9", ProfileHint: "profile-ops",
				ProfileName: "", ReplaceInactive: true, State: "pending", ForeignTarget: true,
				RemainingSeconds: 570, HasShortCode: true,
			},
			{
				ID: "pair-2", Name: "旧客户端", TargetURL: "https://192.0.2.20:7878",
				SourceAddress: "192.0.2.10", State: "pending", ForeignTarget: true,
				RemainingSeconds: 540,
			},
			{
				ID: "pair-3", Name: "已批准客户端", TargetURL: "https://192.0.2.20:7878",
				ShortCode: "271828", SourceAddress: "192.0.2.11", ProfileName: "运维",
				State: "approved", RemainingSeconds: 96, HasShortCode: true,
			},
			{
				ID: "pair-4", Name: "已完成客户端", State: "delivered", RemainingSeconds: 240,
			},
		},
	}
}

func newAuthorityForPairingTest(t *testing.T, store *stubAccessStore) (http.Handler, *fakeAuthorityAuth) {
	t.Helper()
	auth := &fakeAuthorityAuth{passwordSet: true, password: "correct-password", now: time.Unix(1800000000, 0)}
	handler, _, err := NewAuthority(Config{
		Role: RoleServer, Servers: &stubServerSource{}, Access: store, Auth: auth,
		AllowedHosts: []string{"192.0.2.50"}, Port: 7879,
		Now: func() time.Time { return auth.now },
	})
	if err != nil {
		t.Fatalf("构造权威端 Web UI Handler 失败：%v", err)
	}
	return handler, auth
}

func TestAuthorityPairingPageAndListFragment(t *testing.T) {
	store := pairingTestStore()
	handler, _ := newAuthorityForPairingTest(t, store)
	cookie := authoritySuccessfulLogin(t, handler, "192.0.2.50:7879", "correct-password")

	page := authorityGet(t, handler, "192.0.2.50:7879", "/ui/access/pairing", cookie)
	body := readResponseBody(t, page)
	if page.StatusCode != http.StatusOK {
		t.Fatalf("配对页状态码=%d 页面：\n%s", page.StatusCode, body)
	}
	for _, want := range []string{
		`<a href="/ui/access/pairing">配对</a>`,
		`<div id="pairing-list" hx-get="/ui/access/pairing/list" hx-trigger="every 5s" hx-swap="outerHTML">`,
		"笔记本", "https://127.0.0.1:7878", "314159", "192.0.2.9", "替换同名设备的未激活设备码",
		"剩余 570 秒", `<option value="profile-ops" selected>运维</option>`,
		"已与客户端屏幕上的短认证码逐位比对一致", `name="override"`,
		"旧客户端", "行缺少短认证码", "只能拒绝",
		"已批准，等待客户端完成（剩余 96 秒）", "已完成",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("配对页应包含 %q：\n%s", want, body)
		}
	}

	fragmentRequest, _ := http.NewRequest(http.MethodGet, "/ui/access/pairing/list", nil)
	fragmentRequest.Host = "192.0.2.50:7879"
	fragmentRequest.Header.Set("HX-Request", "true")
	fragmentRequest.AddCookie(cookie)
	fragmentRecorder := httptest.NewRecorder()
	handler.ServeHTTP(fragmentRecorder, fragmentRequest)
	fragment := fragmentRecorder.Body.String()
	if fragmentRecorder.Code != http.StatusOK {
		t.Fatalf("配对列表片段状态码=%d 片段：\n%s", fragmentRecorder.Code, fragment)
	}
	if strings.Contains(fragment, "<html") || !strings.Contains(fragment, "笔记本") {
		t.Fatalf("配对列表片段应是局部内容：\n%s", fragment)
	}
}

func TestAuthorityPairingApproveRejectThroughAccessStore(t *testing.T) {
	store := pairingTestStore()
	handler, auth := newAuthorityForPairingTest(t, store)
	cookie := authoritySuccessfulLogin(t, handler, "192.0.2.50:7879", "correct-password")
	page := authorityGet(t, handler, "192.0.2.50:7879", "/ui/access/pairing", cookie)
	body := readResponseBody(t, page)
	csrfToken := hiddenInputValue(t, body, "csrf_token")

	store.approveErr = errors.New("必须先与客户端屏幕上的短认证码逐位比对一致")
	missingComparison := authorityPostForm(t, handler, "192.0.2.50:7879", "/ui/access/pairing/pair-1/approve", url.Values{
		"csrf_token": {csrfToken}, "profile": {"profile-ops"},
	}, cookie)
	missingBody := readResponseBody(t, missingComparison)
	if missingComparison.StatusCode != http.StatusConflict || !strings.Contains(missingBody, "必须先与客户端屏幕上的短认证码逐位比对一致") {
		t.Fatalf("缺少比对确认应被拒绝，状态码=%d 页面：\n%s", missingComparison.StatusCode, missingBody)
	}
	if store.approveCompared {
		t.Fatal("表单未勾选时不应传“已比对”")
	}

	store.approveErr = errors.New("⚠ 配对声明目标 ≠ 本机地址")
	missingOverride := authorityPostForm(t, handler, "192.0.2.50:7879", "/ui/access/pairing/pair-1/approve", url.Values{
		"csrf_token": {csrfToken}, "profile": {"profile-ops"}, "compared": {"1"},
	}, cookie)
	missingOverrideBody := readResponseBody(t, missingOverride)
	if missingOverride.StatusCode != http.StatusConflict || !strings.Contains(missingOverrideBody, "目标 ≠ 本机地址") {
		t.Fatalf("缺少地址覆盖应被拒绝，状态码=%d 页面：\n%s", missingOverride.StatusCode, missingOverrideBody)
	}
	if store.approveOverride != "" {
		t.Fatalf("缺少覆盖时不应传覆盖文字，实际 %q", store.approveOverride)
	}

	store.approveErr = nil
	auth.now = auth.now.Add(6 * time.Minute)
	approved := authorityPostForm(t, handler, "192.0.2.50:7879", "/ui/access/pairing/pair-1/approve", url.Values{
		"csrf_token": {csrfToken}, "profile": {"profile-ops"}, "compared": {"1"}, "override": {"OVERRIDE"},
	}, cookie)
	approvedBody := readResponseBody(t, approved)
	if approved.StatusCode != http.StatusSeeOther || approved.Header.Get("Location") != "/ui/access/pairing" {
		t.Fatalf("批准应回到配对页，状态码=%d 跳转=%q 页面：\n%s", approved.StatusCode, approved.Header.Get("Location"), approvedBody)
	}
	if store.approvePairingID != "pair-1" || store.approveProfile != "profile-ops" || !store.approveCompared ||
		store.approveOverride != "OVERRIDE" || store.approveSrc.RemoteAddress != "198.51.100.9" {
		t.Fatalf("批准参数不正确：id=%q profile=%q compared=%v override=%q source=%+v",
			store.approvePairingID, store.approveProfile, store.approveCompared, store.approveOverride, store.approveSrc)
	}

	rejected := authorityPostForm(t, handler, "192.0.2.50:7879", "/ui/access/pairing/pair-2/reject", url.Values{
		"csrf_token": {csrfToken},
	}, cookie)
	rejectedBody := readResponseBody(t, rejected)
	if rejected.StatusCode != http.StatusSeeOther || rejected.Header.Get("Location") != "/ui/access/pairing" {
		t.Fatalf("拒绝应回到配对页，状态码=%d 跳转=%q 页面：\n%s", rejected.StatusCode, rejected.Header.Get("Location"), rejectedBody)
	}
	if store.rejectPairingID != "pair-2" || store.rejectSrc.RemoteAddress != "198.51.100.9" {
		t.Fatalf("拒绝参数不正确：id=%q source=%+v", store.rejectPairingID, store.rejectSrc)
	}
}

func TestAuthorityPairingMissingShortCodeCanOnlyReject(t *testing.T) {
	store := pairingTestStore()
	handler, _ := newAuthorityForPairingTest(t, store)
	cookie := authoritySuccessfulLogin(t, handler, "192.0.2.50:7879", "correct-password")
	page := authorityGet(t, handler, "192.0.2.50:7879", "/ui/access/pairing", cookie)
	body := readResponseBody(t, page)
	csrfToken := hiddenInputValue(t, body, "csrf_token")

	response := authorityPostForm(t, handler, "192.0.2.50:7879", "/ui/access/pairing/pair-2/approve", url.Values{
		"csrf_token": {csrfToken}, "profile": {"profile-ops"}, "compared": {"1"}, "override": {"OVERRIDE"},
	}, cookie)
	responseBody := readResponseBody(t, response)
	if response.StatusCode != http.StatusConflict || !strings.Contains(responseBody, "行缺少短认证码") {
		t.Fatalf("缺少短认证码的请求只能拒绝，状态码=%d 页面：\n%s", response.StatusCode, responseBody)
	}
	if store.approvePairingID != "" {
		t.Fatalf("缺少短认证码的请求不应调用批准，实际 id=%q", store.approvePairingID)
	}
}
