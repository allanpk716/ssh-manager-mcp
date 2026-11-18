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
				ID: "pair-1", Name: "笔记本", TargetURL: "https://192.0.2.50:7878",
				ShortCode: "314159", SourceAddress: "192.0.2.9", ProfileHint: "profile-ops",
				ReplaceInactive: true, State: "pending", ForeignTarget: false,
				RemainingSeconds: 570, HasShortCode: true,
			},
			{
				ID: "pair-foreign", Name: "陌生地址客户端", TargetURL: "https://203.0.113.9:7878",
				ShortCode: "271828", SourceAddress: "192.0.2.12", State: "pending",
				ForeignTarget: true, RemainingSeconds: 540, HasShortCode: true,
			},
			{
				ID: "pair-2", Name: "旧客户端", TargetURL: "https://192.0.2.20:7878",
				SourceAddress: "192.0.2.10", State: "pending", ForeignTarget: true,
				RemainingSeconds: 540,
			},
			{
				ID: "pair-3", Name: "已批准客户端", TargetURL: "https://192.0.2.20:7878",
				ShortCode: "654321", SourceAddress: "192.0.2.11", ProfileName: "运维",
				State: "approved", RemainingSeconds: 96, HasShortCode: true,
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

func TestAuthorityPairingListApprovesOnOneScreen(t *testing.T) {
	store := pairingTestStore()
	handler, _ := newAuthorityForPairingTest(t, store)
	cookie := authoritySuccessfulLogin(t, handler, "192.0.2.50:7879", "correct-password")

	page := authorityGet(t, handler, "192.0.2.50:7879", "/ui/access/pairing", cookie)
	body := readResponseBody(t, page)
	if page.StatusCode != http.StatusOK {
		t.Fatalf("配对页状态码=%d 页面：\n%s", page.StatusCode, body)
	}
	for _, want := range []string{
		`<h2 class="pairing-instance">笔记本</h2>`,
		`<span class="pairing-target">https://192.0.2.50:7878</span>`,
		`<code class="pairing-short-code">314159</code>`,
		`action="/ui/access/pairing/pair-1/approve"`,
		`name="profile"`, `name="compared"`,
		"我已逐位比对三项一致", "剩余 570 秒",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("配对列表应包含 %q：\n%s", want, body)
		}
	}
	if strings.Contains(body, `name="override"`) || strings.Contains(body, "OVERRIDE") {
		t.Fatalf("配对列表不应要求输入覆盖文字：\n%s", body)
	}

	foreignArticle := body[strings.Index(body, "pair-foreign"):]
	if !strings.Contains(foreignArticle, "这条请求连接的地址不是本机，也不在已登记的对外地址里") ||
		!strings.Contains(foreignArticle, "serve.public_addresses") ||
		strings.Contains(foreignArticle, "pair-foreign/approve") {
		t.Fatalf("异常地址请求应只给拒绝并提示登记设置项：\n%s", foreignArticle)
	}

	fragmentRequest, _ := http.NewRequest(http.MethodGet, "/ui/access/pairing/list", nil)
	fragmentRequest.Host = "192.0.2.50:7879"
	fragmentRequest.Header.Set("HX-Request", "true")
	fragmentRequest.AddCookie(cookie)
	fragmentRecorder := httptest.NewRecorder()
	handler.ServeHTTP(fragmentRecorder, fragmentRequest)
	fragment := fragmentRecorder.Body.String()
	if fragmentRecorder.Code != http.StatusOK || strings.Contains(fragment, "<html") || !strings.Contains(fragment, "笔记本") {
		t.Fatalf("配对列表片段状态码=%d 片段：\n%s", fragmentRecorder.Code, fragment)
	}
}

func TestAuthorityPairingListSelectsOnlyProfileByDefault(t *testing.T) {
	store := pairingTestStore()
	store.profiles = []AccessProfileSummary{{ID: "profile-only", Name: "唯一"}}
	handler, _ := newAuthorityForPairingTest(t, store)
	cookie := authoritySuccessfulLogin(t, handler, "192.0.2.50:7879", "correct-password")

	page := authorityGet(t, handler, "192.0.2.50:7879", "/ui/access/pairing", cookie)
	body := readResponseBody(t, page)
	if !strings.Contains(body, `value="profile-only" selected`) {
		t.Fatalf("只有一个 profile 时应默认选中：\n%s", body)
	}
}

func TestAuthorityPairingListErrorKeepsProfileOnSamePage(t *testing.T) {
	store := pairingTestStore()
	store.approveErr = errors.New("必须先与客户端屏幕上的短认证码逐位比对一致，才能批准配对")
	handler, _ := newAuthorityForPairingTest(t, store)
	cookie := authoritySuccessfulLogin(t, handler, "192.0.2.50:7879", "correct-password")
	page := authorityGet(t, handler, "192.0.2.50:7879", "/ui/access/pairing", cookie)
	csrfToken := hiddenInputValue(t, readResponseBody(t, page), "csrf_token")

	response := authorityPostForm(t, handler, "192.0.2.50:7879", "/ui/access/pairing/pair-1/approve", url.Values{
		"csrf_token": {csrfToken}, "profile": {"profile-dev"},
	}, cookie)
	responseBody := readResponseBody(t, response)
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("未勾选比对应在配对列表显示错误，状态码=%d 页面：\n%s", response.StatusCode, responseBody)
	}
	if !strings.Contains(responseBody, `value="profile-dev" selected`) ||
		strings.Contains(responseBody, `name="compared" value="1" checked`) ||
		!strings.Contains(responseBody, "必须先与客户端屏幕上的短认证码逐位比对一致") {
		t.Fatalf("失败后的配对列表应保留 profile 并显示错误：\n%s", responseBody)
	}
}

func TestAuthorityPairingApprovalAfterReverifyTimeoutSucceeds(t *testing.T) {
	store := pairingTestStore()
	handler, auth := newAuthorityForPairingTest(t, store)
	cookie := authoritySuccessfulLogin(t, handler, "192.0.2.50:7879", "correct-password")
	page := authorityGet(t, handler, "192.0.2.50:7879", "/ui/access/pairing", cookie)
	csrfToken := hiddenInputValue(t, readResponseBody(t, page), "csrf_token")
	auth.now = auth.now.Add(6 * time.Minute)

	approved := authorityPostForm(t, handler, "192.0.2.50:7879", "/ui/access/pairing/pair-1/approve", url.Values{
		"csrf_token": {csrfToken}, "profile": {"profile-dev"}, "compared": {"1"},
	}, cookie)
	approvedBody := readResponseBody(t, approved)
	if approved.StatusCode != http.StatusSeeOther || approved.Header.Get("Location") != "/ui/access/pairing?done=approve" {
		t.Fatalf("超过重新验证窗口仍应批准，状态码=%d 跳转=%q 页面：\n%s",
			approved.StatusCode, approved.Header.Get("Location"), approvedBody)
	}
	if store.approvePairingID != "pair-1" || store.approveProfile != "profile-dev" || !store.approveCompared ||
		store.approveAllowForeign || store.approveSrc.RemoteAddress != "198.51.100.9" {
		t.Fatalf("批准参数不正确：id=%q profile=%q compared=%v 命令行显式放行=%v source=%+v",
			store.approvePairingID, store.approveProfile, store.approveCompared, store.approveAllowForeign, store.approveSrc)
	}
	result := authorityGet(t, handler, "192.0.2.50:7879", approved.Header.Get("Location"), cookie)
	resultBody := readResponseBody(t, result)
	if result.StatusCode != http.StatusOK || !strings.Contains(resultBody, "已批准配对请求") {
		t.Fatalf("批准后列表应显示成功提示，状态码=%d 页面：\n%s", result.StatusCode, resultBody)
	}

	rejected := authorityPostForm(t, handler, "192.0.2.50:7879", "/ui/access/pairing/pair-2/reject", url.Values{
		"csrf_token": {csrfToken},
	}, cookie)
	rejectedBody := readResponseBody(t, rejected)
	if rejected.StatusCode != http.StatusSeeOther || rejected.Header.Get("Location") != "/ui/access/pairing?done=reject" ||
		store.rejectPairingID != "pair-2" || store.rejectSrc.RemoteAddress != "198.51.100.9" {
		t.Fatalf("超过重新验证窗口仍应拒绝，状态码=%d 跳转=%q 调用=%q 页面：\n%s",
			rejected.StatusCode, rejected.Header.Get("Location"), store.rejectPairingID, rejectedBody)
	}
}

func TestAuthorityPairingForeignApprovalIsRefusedByBackend(t *testing.T) {
	store := pairingTestStore()
	store.approveErr = errors.New("配对声明目标不属于本机地址，Web UI 只能拒绝；建议拒绝")
	handler, _ := newAuthorityForPairingTest(t, store)
	cookie := authoritySuccessfulLogin(t, handler, "192.0.2.50:7879", "correct-password")
	page := authorityGet(t, handler, "192.0.2.50:7879", "/ui/access/pairing", cookie)
	csrfToken := hiddenInputValue(t, readResponseBody(t, page), "csrf_token")

	response := authorityPostForm(t, handler, "192.0.2.50:7879", "/ui/access/pairing/pair-foreign/approve", url.Values{
		"csrf_token": {csrfToken}, "profile": {"profile-ops"}, "compared": {"1"}, "override": {"OVERRIDE"},
	}, cookie)
	responseBody := readResponseBody(t, response)
	if response.StatusCode != http.StatusConflict || !strings.Contains(responseBody, "建议拒绝") {
		t.Fatalf("异常地址批准应被后端拒绝，状态码=%d 页面：\n%s", response.StatusCode, responseBody)
	}
	if store.approvePairingID != "pair-foreign" || store.approveAllowForeign {
		t.Fatalf("Web UI 后端不得显式放行异常地址：id=%q 显式放行=%v", store.approvePairingID, store.approveAllowForeign)
	}
}

func TestAuthorityPairingDetailIsOptionalAndDoesNotRequireReverify(t *testing.T) {
	store := pairingTestStore()
	handler, auth := newAuthorityForPairingTest(t, store)
	cookie := authoritySuccessfulLogin(t, handler, "192.0.2.50:7879", "correct-password")
	auth.now = auth.now.Add(6 * time.Minute)

	detail := authorityGet(t, handler, "192.0.2.50:7879", "/ui/access/pairing/pair-1", cookie)
	detailBody := readResponseBody(t, detail)
	if detail.StatusCode != http.StatusOK || strings.Contains(detailBody, "/ui/reverify") {
		t.Fatalf("配对详情页超过重新验证窗口也应可直接打开，状态码=%d 页面：\n%s", detail.StatusCode, detailBody)
	}
	for _, want := range []string{
		`action="/ui/access/pairing/pair-1/approve"`, `name="profile"`, `name="compared"`,
	} {
		if !strings.Contains(detailBody, want) {
			t.Fatalf("配对详情页应包含 %q：\n%s", want, detailBody)
		}
	}
	if strings.Contains(detailBody, `name="override"`) {
		t.Fatalf("配对详情页不应要求覆盖文字：\n%s", detailBody)
	}
}

func TestAuthorityPairingMissingShortCodeCanOnlyReject(t *testing.T) {
	store := pairingTestStore()
	handler, _ := newAuthorityForPairingTest(t, store)
	cookie := authoritySuccessfulLogin(t, handler, "192.0.2.50:7879", "correct-password")
	page := authorityGet(t, handler, "192.0.2.50:7879", "/ui/access/pairing", cookie)
	csrfToken := hiddenInputValue(t, readResponseBody(t, page), "csrf_token")

	response := authorityPostForm(t, handler, "192.0.2.50:7879", "/ui/access/pairing/pair-2/approve", url.Values{
		"csrf_token": {csrfToken}, "profile": {"profile-ops"}, "compared": {"1"},
	}, cookie)
	responseBody := readResponseBody(t, response)
	if response.StatusCode != http.StatusConflict || !strings.Contains(responseBody, "行缺少短认证码") {
		t.Fatalf("缺少短认证码的请求只能拒绝，状态码=%d 页面：\n%s", response.StatusCode, responseBody)
	}
	if store.approvePairingID != "" {
		t.Fatalf("缺少短认证码的请求不应调用批准，实际 id=%q", store.approvePairingID)
	}
}
