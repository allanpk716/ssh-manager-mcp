package webui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// 原桩补齐升级窄接口的零值实现，既有用例无需改动；升级页自己的桩在下方
// 覆盖这两个方法并记录调用。
func (s *stubOpsStore) CheckUpdate(string) (OpsUpdateStatus, error) {
	return OpsUpdateStatus{}, nil
}

func (s *stubOpsStore) ApplyUpdate(string, string) (OpsUpdateApplyResult, error) {
	return OpsUpdateApplyResult{}, nil
}

// stubOpsUpdateStore 在既有运维测试桩上扩展升级页需要的字段；查询行为仍复用
// 原桩。
type stubOpsUpdateStore struct {
	*stubOpsStore

	updateStatus        OpsUpdateStatus
	updateCheckErr      error
	updateCheckInstance string
	updateResult        OpsUpdateApplyResult
	updateApplyErr      error
	updateApplyInstance string
	updateApplySourceIP string
}

func (s *stubOpsUpdateStore) CheckUpdate(instance string) (OpsUpdateStatus, error) {
	s.updateCheckInstance = instance
	if s.updateCheckErr != nil {
		return OpsUpdateStatus{}, s.updateCheckErr
	}
	return s.updateStatus, nil
}

func (s *stubOpsUpdateStore) ApplyUpdate(instance, sourceIP string) (OpsUpdateApplyResult, error) {
	s.updateApplyInstance = instance
	s.updateApplySourceIP = sourceIP
	if s.updateApplyErr != nil {
		return OpsUpdateApplyResult{}, s.updateApplyErr
	}
	return s.updateResult, nil
}

func upgradeTestStatus() OpsUpdateStatus {
	return OpsUpdateStatus{
		CurrentVersion: "0.1.0", RecommendedVersion: "0.1.0",
		LatestVersion: "v0.2.0", TargetVersion: "v0.2.0", AssetName: "sshmgr-example.tar.gz",
		CanUpgrade: true,
	}
}

func newStubOpsUpdateStore() *stubOpsUpdateStore {
	return &stubOpsUpdateStore{stubOpsStore: &stubOpsStore{}}
}

func newUpgradeTestHandler(t *testing.T, ops OpsStore) (http.Handler, *http.Cookie) {
	t.Helper()
	handler, startup, err := New(Config{
		Role: RoleStandalone, Servers: &stubServerSource{}, Access: &stubAccessStore{},
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

func postOpsUpdate(t *testing.T, handler http.Handler, cookie *http.Cookie, csrf, origin string) *http.Response {
	t.Helper()
	form := url.Values{}
	if csrf != "" {
		form.Set("csrf_token", csrf)
	}
	req := httptest.NewRequest(http.MethodPost, "/ui/ops/update", strings.NewReader(form.Encode()))
	req.Host = "127.0.0.1:18080"
	req.RemoteAddr = "198.51.100.9:45678"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", origin)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Result()
}

func TestOpsUpdatePageShowsVersionsAndUpgradeButton(t *testing.T) {
	ops := newStubOpsUpdateStore()
	ops.updateStatus = upgradeTestStatus()
	handler, cookie := newUpgradeTestHandler(t, ops)

	response := doGetWithCookie(t, handler, cookie, "/ui/ops/update")
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "<h1>升级</h1>") {
		t.Fatalf("单机应打开升级页，状态码=%d 页面：\n%s", response.StatusCode, body)
	}
	for _, want := range []string{"当前版本", "0.1.0", "推荐版本", "GitHub 最新版本", "v0.2.0", "目标版本"} {
		if !strings.Contains(body, want) {
			t.Fatalf("升级页缺少 %q：\n%s", want, body)
		}
	}
	if !strings.Contains(body, `href="/ui/ops/update"`) {
		t.Fatalf("导航应显示升级入口：\n%s", body)
	}
	if !strings.Contains(body, `name="csrf_token"`) || !strings.Contains(body, "升级到 v0.2.0") {
		t.Fatalf("可升级时应显示带防跨站请求伪造令牌的升级按钮：\n%s", body)
	}
}

func TestOpsUpdateApplyShowsRerunLoopbackHint(t *testing.T) {
	ops := newStubOpsUpdateStore()
	ops.updateStatus = upgradeTestStatus()
	ops.updateResult = OpsUpdateApplyResult{OldVersion: "0.1.0", NewVersion: "v0.2.0"}
	handler, cookie := newUpgradeTestHandler(t, ops)
	page := doGetWithCookie(t, handler, cookie, "/ui/ops/update")
	csrf := hiddenInputValue(t, readResponseBody(t, page), "csrf_token")

	response := postOpsUpdate(t, handler, cookie, csrf, "http://127.0.0.1:18080")
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("升级提交应渲染结果页，状态码=%d 页面：\n%s", response.StatusCode, body)
	}
	for _, want := range []string{
		"请关闭并重新运行 sshmgr ui 以使用新版本",
		"正在运行的 MCP 桥会在空闲时自动换到新版本",
		"0.1.0", "v0.2.0",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("升级结果页缺少 %q：\n%s", want, body)
		}
	}
	if ops.updateApplySourceIP != "198.51.100.9" {
		t.Fatalf("升级应带来源地址，实际 %q", ops.updateApplySourceIP)
	}
}

func TestOpsUpdateApplyRejectsMissingCSRFOrWrongOrigin(t *testing.T) {
	ops := newStubOpsUpdateStore()
	ops.updateStatus = upgradeTestStatus()
	ops.updateResult = OpsUpdateApplyResult{OldVersion: "0.1.0", NewVersion: "v0.2.0"}
	handler, cookie := newUpgradeTestHandler(t, ops)
	page := doGetWithCookie(t, handler, cookie, "/ui/ops/update")
	csrf := hiddenInputValue(t, readResponseBody(t, page), "csrf_token")

	missing := postOpsUpdate(t, handler, cookie, "", "http://127.0.0.1:18080")
	missing.Body.Close()
	if missing.StatusCode != http.StatusForbidden {
		t.Fatalf("缺少防跨站请求伪造令牌应返回 403，实际 %d", missing.StatusCode)
	}
	wrongOrigin := postOpsUpdate(t, handler, cookie, csrf, "https://example.invalid")
	wrongOrigin.Body.Close()
	if wrongOrigin.StatusCode != http.StatusForbidden {
		t.Fatalf("来源不对应返回 403，实际 %d", wrongOrigin.StatusCode)
	}
	if ops.updateApplySourceIP != "" {
		t.Fatalf("被拒请求不应触发升级，实际来源地址 %q", ops.updateApplySourceIP)
	}
}

func TestOpsUpdateWithoutRecommendedVersionCannotTrigger(t *testing.T) {
	ops := newStubOpsUpdateStore()
	ops.updateStatus = OpsUpdateStatus{
		CurrentVersion: "0.1.0", LatestVersion: "v0.3.0", CanUpgrade: false,
		Reason: "读不到推荐版本，请先同步或先升级权威端", IsClient: true,
	}
	handler, cookie := newUpgradeTestHandler(t, ops)

	page := doGetWithCookie(t, handler, cookie, "/ui/ops/update")
	body := readResponseBody(t, page)
	if !strings.Contains(body, "读不到推荐版本，请先同步或先升级权威端") {
		t.Fatalf("升级页应显示拒绝原因：\n%s", body)
	}
	if strings.Contains(body, "name=\"csrf_token\"") || regexp.MustCompile(`<button[^>]*>`).MatchString(body) {
		t.Fatalf("不能升级时不应出现提交按钮：\n%s", body)
	}
}

func TestOpsUpdateApplyReportsInProgress(t *testing.T) {
	ops := newStubOpsUpdateStore()
	ops.updateStatus = upgradeTestStatus()
	ops.updateApplyErr = &OpsUpdateError{Step: OpsUpdateStepInProgress, Message: "升级正在进行"}
	handler, cookie := newUpgradeTestHandler(t, ops)
	page := doGetWithCookie(t, handler, cookie, "/ui/ops/update")
	csrf := hiddenInputValue(t, readResponseBody(t, page), "csrf_token")

	response := postOpsUpdate(t, handler, cookie, csrf, "http://127.0.0.1:18080")
	body := readResponseBody(t, response)
	if !strings.Contains(body, "升级正在进行") {
		t.Fatalf("并发提交应提示升级正在进行：\n%s", body)
	}
}

func TestOpsUpdateApplyReportsFailedStep(t *testing.T) {
	ops := newStubOpsUpdateStore()
	ops.updateStatus = upgradeTestStatus()
	ops.updateApplyErr = &OpsUpdateError{Step: OpsUpdateStepVerify, Message: "校验和不匹配"}
	handler, cookie := newUpgradeTestHandler(t, ops)
	page := doGetWithCookie(t, handler, cookie, "/ui/ops/update")
	csrf := hiddenInputValue(t, readResponseBody(t, page), "csrf_token")

	response := postOpsUpdate(t, handler, cookie, csrf, "http://127.0.0.1:18080")
	body := readResponseBody(t, response)
	if !strings.Contains(body, "校验") || !strings.Contains(body, "校验和不匹配") {
		t.Fatalf("失败时应显示失败在哪一步：\n%s", body)
	}
}

func TestOpsUpdateClientPageUsesStartupInstance(t *testing.T) {
	ops := newStubOpsUpdateStore()
	ops.updateStatus = OpsUpdateStatus{
		CurrentVersion: "0.1.0", RecommendedVersion: "v0.2.0", LatestVersion: "v0.3.0",
		TargetVersion: "v0.2.0", CanUpgrade: true, IsClient: true,
	}
	client, startup, err := New(Config{
		Role: RoleClient, Servers: &stubServerSource{}, Ops: ops, Port: 18080, OpsInstance: "laptop",
	})
	if err != nil {
		t.Fatal(err)
	}
	session := exchangeStartupToken(t, client, startup.URL("127.0.0.1:18080"))
	cookie := session.Cookies()[0]
	session.Body.Close()

	response := doGetWithCookie(t, client, cookie, "/ui/ops/update")
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "v0.2.0") {
		t.Fatalf("客户端应打开升级页，状态码=%d 页面：\n%s", response.StatusCode, body)
	}
	if ops.updateCheckInstance != "laptop" {
		t.Fatalf("客户端升级页应按启动实例读取，实际 %q", ops.updateCheckInstance)
	}
	if strings.Contains(body, `aria-label="运维区域"`) {
		t.Fatalf("客户端本票不应显示运维导航：\n%s", body)
	}
}

func TestOpsUpdateAuthorityRouteNotInThisTicket(t *testing.T) {
	auth := &fakeAuthorityAuth{passwordSet: true, password: "correct-password"}
	authority, _, err := NewAuthority(Config{
		Role: RoleServer, Servers: &stubServerSource{}, Access: &stubAccessStore{},
		Ops: newStubOpsUpdateStore(), Auth: auth, AllowedHosts: []string{"127.0.0.1"}, Port: 7879,
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie := authoritySuccessfulLogin(t, authority, "127.0.0.1:7879", "correct-password")
	response := authorityGet(t, authority, "127.0.0.1:7879", "/ui/ops/update", cookie)
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("权威端两步升级属后续票，本票不应注册路由，实际 %d", response.StatusCode)
	}
}
