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
		"剩余 570 秒",
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

func TestAuthorityPairingApprovalFormLivesOnDetailPage(t *testing.T) {
	store := pairingTestStore()
	handler, _ := newAuthorityForPairingTest(t, store)
	cookie := authoritySuccessfulLogin(t, handler, "192.0.2.50:7879", "correct-password")

	page := authorityGet(t, handler, "192.0.2.50:7879", "/ui/access/pairing", cookie)
	body := readResponseBody(t, page)
	if page.StatusCode != http.StatusOK {
		t.Fatalf("配对页状态码=%d 页面：\n%s", page.StatusCode, body)
	}
	if !strings.Contains(body, `hx-trigger="every 5s"`) {
		t.Fatalf("配对列表应自动刷新：\n%s", body)
	}
	if !strings.Contains(body, `<a class="pairing-approve-link" href="/ui/access/pairing/pair-1">批准</a>`) {
		t.Fatalf("待批准请求应有批准链接：\n%s", body)
	}
	if strings.Contains(body, `name="compared"`) || strings.Contains(body, `name="override"`) {
		t.Fatalf("自动刷新的列表不应包含批准表单：\n%s", body)
	}

	detail := authorityGet(t, handler, "192.0.2.50:7879", "/ui/access/pairing/pair-1", cookie)
	detailBody := readResponseBody(t, detail)
	if detail.StatusCode != http.StatusOK {
		t.Fatalf("配对批准页状态码=%d 页面：\n%s", detail.StatusCode, detailBody)
	}
	if strings.Contains(detailBody, `hx-trigger="every 5s"`) {
		t.Fatalf("配对批准页不应自动刷新：\n%s", detailBody)
	}
	for _, want := range []string{
		`action="/ui/access/pairing/pair-1/approve"`, `name="profile"`,
		`name="compared"`, `name="override"`,
	} {
		if !strings.Contains(detailBody, want) {
			t.Fatalf("配对批准页应包含 %q：\n%s", want, detailBody)
		}
	}
}

func TestAuthorityPairingApprovalErrorKeepsSubmittedForm(t *testing.T) {
	store := pairingTestStore()
	store.approveErr = errors.New("目标地址异常，必须输入精确文本 OVERRIDE")
	handler, _ := newAuthorityForPairingTest(t, store)
	cookie := authoritySuccessfulLogin(t, handler, "192.0.2.50:7879", "correct-password")
	page := authorityGet(t, handler, "192.0.2.50:7879", "/ui/access/pairing/pair-1", cookie)
	body := readResponseBody(t, page)
	csrfToken := hiddenInputValue(t, body, "csrf_token")

	response := authorityPostForm(t, handler, "192.0.2.50:7879", "/ui/access/pairing/pair-1/approve", url.Values{
		"csrf_token": {csrfToken}, "profile": {"profile-dev"},
		"compared": {"1"}, "override": {"KEEP_ME"},
	}, cookie)
	responseBody := readResponseBody(t, response)
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("批准失败应重新渲染批准页，状态码=%d 页面：\n%s", response.StatusCode, responseBody)
	}
	for _, want := range []string{
		`value="profile-dev" selected`, `value="KEEP_ME"`, `checked`,
		"目标地址异常，必须输入精确文本 OVERRIDE",
	} {
		if !strings.Contains(responseBody, want) {
			t.Fatalf("失败后的批准表单应保留已填内容 %q：\n%s", want, responseBody)
		}
	}
}

func TestAuthorityPairingApprovalDetailRequiresReverifyBeforeRendering(t *testing.T) {
	store := pairingTestStore()
	handler, auth := newAuthorityForPairingTest(t, store)
	cookie := authoritySuccessfulLogin(t, handler, "192.0.2.50:7879", "correct-password")
	auth.now = auth.now.Add(6 * time.Minute)

	detail := authorityGet(t, handler, "192.0.2.50:7879", "/ui/access/pairing/pair-1", cookie)
	detailBody := readResponseBody(t, detail)
	detailPath := "/ui/access/pairing/pair-1"
	wantRedirect := `/ui/reverify?next=` + url.QueryEscape(detailPath)
	if detail.StatusCode != http.StatusSeeOther || detail.Header.Get("Location") != wantRedirect {
		t.Fatalf("未重新验证时应先跳转重新验证页，状态码=%d 跳转=%q 页面：\n%s",
			detail.StatusCode, detail.Header.Get("Location"), detailBody)
	}

	reverifyPage := authorityGet(t, handler, "192.0.2.50:7879", wantRedirect, cookie)
	reverifyBody := readResponseBody(t, reverifyPage)
	reverified := authorityPostForm(t, handler, "192.0.2.50:7879", "/ui/reverify", url.Values{
		"csrf_token": {hiddenInputValue(t, reverifyBody, "csrf_token")},
		"password":   {"correct-password"}, "next": {detailPath},
	}, cookie)
	reverifiedBody := readResponseBody(t, reverified)
	if reverified.StatusCode != http.StatusSeeOther || reverified.Header.Get("Location") != detailPath {
		t.Fatalf("重新验证后应回到批准表单，状态码=%d 跳转=%q 页面：\n%s",
			reverified.StatusCode, reverified.Header.Get("Location"), reverifiedBody)
	}

	resumed := authorityGet(t, handler, "192.0.2.50:7879", detailPath, cookie)
	resumedBody := readResponseBody(t, resumed)
	if resumed.StatusCode != http.StatusOK || !strings.Contains(resumedBody, `name="compared"`) {
		t.Fatalf("重新验证后应正常渲染批准表单，状态码=%d 页面：\n%s", resumed.StatusCode, resumedBody)
	}
}

func TestAuthorityPairingApprovalReverifyPreservesSubmittedForm(t *testing.T) {
	store := pairingTestStore()
	handler, auth := newAuthorityForPairingTest(t, store)
	cookie := authoritySuccessfulLogin(t, handler, "192.0.2.50:7879", "correct-password")
	page := authorityGet(t, handler, "192.0.2.50:7879", "/ui/access/pairing", cookie)
	body := readResponseBody(t, page)
	csrfToken := hiddenInputValue(t, body, "csrf_token")

	auth.now = auth.now.Add(6 * time.Minute)
	approved := authorityPostForm(t, handler, "192.0.2.50:7879", "/ui/access/pairing/pair-1/approve", url.Values{
		"csrf_token": {csrfToken}, "profile": {"profile-dev"},
		"compared": {"1"}, "override": {"OVERRIDE"},
	}, cookie)
	approvedBody := readResponseBody(t, approved)
	resumePath := "/ui/access/pairing/pair-1?override=OVERRIDE&profile=profile-dev"
	approveRedirect := `/ui/reverify?next=` + url.QueryEscape(resumePath)
	if approved.StatusCode != http.StatusSeeOther || approved.Header.Get("Location") != approveRedirect {
		t.Fatalf("批准提交过期时应保留填写并先重新验证，状态码=%d 跳转=%q 页面：\n%s",
			approved.StatusCode, approved.Header.Get("Location"), approvedBody)
	}
	if store.approvePairingID != "" {
		t.Fatalf("未重新验证时不应调用批准，实际 id=%q", store.approvePairingID)
	}

	reverifyPage := authorityGet(t, handler, "192.0.2.50:7879", approveRedirect, cookie)
	reverifyBody := readResponseBody(t, reverifyPage)
	reverified := authorityPostForm(t, handler, "192.0.2.50:7879", "/ui/reverify", url.Values{
		"csrf_token": {hiddenInputValue(t, reverifyBody, "csrf_token")},
		"password":   {"correct-password"}, "next": {resumePath},
	}, cookie)
	reverifiedBody := readResponseBody(t, reverified)
	if reverified.StatusCode != http.StatusSeeOther || reverified.Header.Get("Location") != resumePath {
		t.Fatalf("重新验证后应回到保留填写的批准表单，状态码=%d 跳转=%q 页面：\n%s",
			reverified.StatusCode, reverified.Header.Get("Location"), reverifiedBody)
	}

	resumed := authorityGet(t, handler, "192.0.2.50:7879", resumePath, cookie)
	resumedBody := readResponseBody(t, resumed)
	if resumed.StatusCode != http.StatusOK {
		t.Fatalf("回跳后的批准表单状态码=%d 页面：\n%s", resumed.StatusCode, resumedBody)
	}
	for _, want := range []string{
		`value="profile-dev" selected`, `value="OVERRIDE"`,
		`<input type="checkbox" name="compared" value="1" required>`,
		"已重新验证管理员口令，请再次核对三项后勾选确认并批准。",
	} {
		if !strings.Contains(resumedBody, want) {
			t.Fatalf("回跳后的批准表单应包含 %q：\n%s", want, resumedBody)
		}
	}
}

func TestAuthorityPairingApprovalResumeIgnoresUnknownProfile(t *testing.T) {
	store := pairingTestStore()
	handler, _ := newAuthorityForPairingTest(t, store)
	cookie := authoritySuccessfulLogin(t, handler, "192.0.2.50:7879", "correct-password")

	response := authorityGet(t, handler, "192.0.2.50:7879",
		"/ui/access/pairing/pair-1?profile=profile-missing&override=KEEP_ME", cookie)
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("带回跳参数的批准表单状态码=%d 页面：\n%s", response.StatusCode, body)
	}
	if !strings.Contains(body, `value="profile-ops" selected`) || strings.Contains(body, `value="profile-missing"`) {
		t.Fatalf("不存在的 profile 不应回填，页面应保留提示 profile：\n%s", body)
	}
	if !strings.Contains(body, `value="KEEP_ME"`) {
		t.Fatalf("覆盖文字应保留：\n%s", body)
	}
}

func TestStandalonePairingApprovalDetailDoesNotRequireReverify(t *testing.T) {
	loopback, _, err := New(Config{
		Role: RoleStandalone, Servers: &stubServerSource{}, Access: pairingTestStore(), Port: 18080,
	})
	if err != nil {
		t.Fatalf("构造单机 Web UI Handler 失败：%v", err)
	}
	pairingHandler := loopback.(*handler)
	request := httptest.NewRequest(http.MethodGet, "/ui/access/pairing/pair-1", nil)
	recorder := httptest.NewRecorder()
	pairingHandler.handleAccessPairingAction(recorder, request)
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || recorder.Header().Get("Location") != "" || !strings.Contains(body, `name="compared"`) {
		t.Fatalf("单机角色打开配对批准表单不应跳转重新验证，状态码=%d 跳转=%q 页面：\n%s",
			recorder.Code, recorder.Header().Get("Location"), body)
	}
}

func TestAuthorityPairingDetailForMissingRowShowsReturnLink(t *testing.T) {
	store := pairingTestStore()
	handler, _ := newAuthorityForPairingTest(t, store)
	cookie := authoritySuccessfulLogin(t, handler, "192.0.2.50:7879", "correct-password")

	response := authorityGet(t, handler, "192.0.2.50:7879", "/ui/access/pairing/pair-missing", cookie)
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("不可处理的请求应说明状态，状态码=%d 页面：\n%s", response.StatusCode, body)
	}
	if !strings.Contains(body, "配对请求已过期或已处理") ||
		!strings.Contains(body, `<a href="/ui/access/pairing">返回配对列表</a>`) {
		t.Fatalf("不可处理的请求应给提示与返回链接：\n%s", body)
	}
}

func TestAuthorityPairingDetailForProcessedRowShowsReturnLink(t *testing.T) {
	store := pairingTestStore()
	handler, _ := newAuthorityForPairingTest(t, store)
	cookie := authoritySuccessfulLogin(t, handler, "192.0.2.50:7879", "correct-password")

	response := authorityGet(t, handler, "192.0.2.50:7879", "/ui/access/pairing/pair-3", cookie)
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("已处理请求的详情页状态码=%d 页面：\n%s", response.StatusCode, body)
	}
	if !strings.Contains(body, "该配对请求已处理") ||
		!strings.Contains(body, `<a href="/ui/access/pairing">返回配对列表</a>`) {
		t.Fatalf("已处理请求应给提示与返回链接：\n%s", body)
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
	resumePath := "/ui/access/pairing/pair-1?override=OVERRIDE&profile=profile-ops"
	approveRedirect := `/ui/reverify?next=` + url.QueryEscape(resumePath)
	if approved.StatusCode != http.StatusSeeOther || approved.Header.Get("Location") != approveRedirect {
		t.Fatalf("批准配对应先重新验证，状态码=%d 跳转=%q 页面：\n%s",
			approved.StatusCode, approved.Header.Get("Location"), approvedBody)
	}
	approveReverifyPage := authorityGet(t, handler, "192.0.2.50:7879", approveRedirect, cookie)
	approveReverifyBody := readResponseBody(t, approveReverifyPage)
	approveReverified := authorityPostForm(t, handler, "192.0.2.50:7879", "/ui/reverify", url.Values{
		"csrf_token": {hiddenInputValue(t, approveReverifyBody, "csrf_token")},
		"password":   {"correct-password"}, "next": {resumePath},
	}, cookie)
	if approveReverified.StatusCode != http.StatusSeeOther {
		t.Fatalf("批准配对重新验证失败，状态码=%d：%s", approveReverified.StatusCode, readResponseBody(t, approveReverified))
	}

	approved = authorityPostForm(t, handler, "192.0.2.50:7879", "/ui/access/pairing/pair-1/approve", url.Values{
		"csrf_token": {csrfToken}, "profile": {"profile-ops"}, "compared": {"1"}, "override": {"OVERRIDE"},
	}, cookie)
	approvedBody = readResponseBody(t, approved)
	if approved.StatusCode != http.StatusSeeOther || approved.Header.Get("Location") != "/ui/access/pairing" {
		t.Fatalf("重新验证后批准应回到配对页，状态码=%d 跳转=%q 页面：\n%s",
			approved.StatusCode, approved.Header.Get("Location"), approvedBody)
	}
	if store.approvePairingID != "pair-1" || store.approveProfile != "profile-ops" || !store.approveCompared ||
		store.approveOverride != "OVERRIDE" || store.approveSrc.RemoteAddress != "198.51.100.9" {
		t.Fatalf("批准参数不正确：id=%q profile=%q compared=%v override=%q source=%+v",
			store.approvePairingID, store.approveProfile, store.approveCompared, store.approveOverride, store.approveSrc)
	}

	auth.now = auth.now.Add(6 * time.Minute)
	rejected := authorityPostForm(t, handler, "192.0.2.50:7879", "/ui/access/pairing/pair-2/reject", url.Values{
		"csrf_token": {csrfToken},
	}, cookie)
	rejectedBody := readResponseBody(t, rejected)
	rejectRedirect := `/ui/reverify?next=` + url.QueryEscape("/ui/access/pairing/pair-2")
	if rejected.StatusCode != http.StatusSeeOther || rejected.Header.Get("Location") != rejectRedirect || store.rejectPairingID != "" {
		t.Fatalf("拒绝配对应先重新验证，状态码=%d 跳转=%q 调用=%q 页面：\n%s",
			rejected.StatusCode, rejected.Header.Get("Location"), store.rejectPairingID, rejectedBody)
	}
	rejectReverifyPage := authorityGet(t, handler, "192.0.2.50:7879", rejectRedirect, cookie)
	rejectReverifyBody := readResponseBody(t, rejectReverifyPage)
	rejectReverified := authorityPostForm(t, handler, "192.0.2.50:7879", "/ui/reverify", url.Values{
		"csrf_token": {hiddenInputValue(t, rejectReverifyBody, "csrf_token")},
		"password":   {"correct-password"}, "next": {"/ui/access/pairing/pair-2"},
	}, cookie)
	if rejectReverified.StatusCode != http.StatusSeeOther {
		t.Fatalf("拒绝配对重新验证失败，状态码=%d：%s", rejectReverified.StatusCode, readResponseBody(t, rejectReverified))
	}
	rejected = authorityPostForm(t, handler, "192.0.2.50:7879", "/ui/access/pairing/pair-2/reject", url.Values{
		"csrf_token": {csrfToken},
	}, cookie)
	rejectedBody = readResponseBody(t, rejected)
	if rejected.StatusCode != http.StatusSeeOther || rejected.Header.Get("Location") != "/ui/access/pairing" {
		t.Fatalf("重新验证后拒绝应回到配对页，状态码=%d 跳转=%q 页面：\n%s",
			rejected.StatusCode, rejected.Header.Get("Location"), rejectedBody)
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
