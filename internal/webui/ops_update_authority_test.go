package webui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type stubAuthorityOpsUpdateStore struct {
	*stubOpsUpdateStore

	restartErr        error
	restartSourceIP   string
	restartScheduled  bool
	restarts          int
	restartAtSchedule func()
	restartDone       chan struct{}
}

func TestAuthorityOpsUpdateRestartRunsAfterCompleteResponse(t *testing.T) {
	ops := &stubAuthorityOpsUpdateStore{stubOpsUpdateStore: newStubOpsUpdateStore()}
	ops.updateStatus = upgradeTestStatus()
	ops.restartDone = make(chan struct{})
	handler, _, cookie := newAuthorityOpsUpdateTestHandler(t, ops)
	page := authorityGet(t, handler, "127.0.0.1:7879", "/ui/ops/update", cookie)
	csrf := hiddenInputValue(t, readResponseBody(t, page), "csrf_token")

	recorder := httptest.NewRecorder()
	ops.restartAtSchedule = func() {
		if !strings.Contains(recorder.Body.String(), "正在重启，请稍后刷新") {
			t.Fatalf("重启必须在完整响应写出后发起，当前响应：\n%s", recorder.Body.String())
		}
	}
	form := url.Values{"csrf_token": {csrf}}
	request := httptest.NewRequest(http.MethodPost, "/ui/ops/update/restart", strings.NewReader(form.Encode()))
	request.Host = "127.0.0.1:7879"
	request.RemoteAddr = "198.51.100.9:45678"
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://127.0.0.1:7879")
	request.AddCookie(cookie)
	handler.ServeHTTP(recorder, request)
	response := recorder.Result()
	body := readResponseBody(t, response)

	if response.StatusCode != http.StatusOK || !strings.Contains(body, "正在重启，请稍后刷新") {
		t.Fatalf("重启响应应完整返回，状态码=%d 页面：\n%s", response.StatusCode, body)
	}
	if response.ContentLength != int64(len(body)) {
		t.Fatalf("重启响应应声明完整长度，头部=%d 实际=%d", response.ContentLength, len(body))
	}
	select {
	case <-ops.restartDone:
	case <-time.After(2 * time.Second):
		t.Fatal("等待响应后的重启动作超时")
	}
	if ops.restarts != 1 {
		t.Fatalf("完整响应返回后应发起一次重启，实际 %d 次", ops.restarts)
	}
	if ops.restartSourceIP != "198.51.100.9" {
		t.Fatalf("重启应带来源地址，实际 %q", ops.restartSourceIP)
	}
}

func TestAuthorityOpsUpdateWritesRequireRecentReverification(t *testing.T) {
	ops := &stubAuthorityOpsUpdateStore{stubOpsUpdateStore: newStubOpsUpdateStore()}
	ops.updateStatus = upgradeTestStatus()
	ops.updateResult = OpsUpdateApplyResult{OldVersion: "0.1.0", NewVersion: "v0.2.0"}
	ops.restartDone = make(chan struct{})
	handler, auth, cookie := newAuthorityOpsUpdateTestHandler(t, ops)
	auth.now = auth.now.Add(6 * time.Minute)

	status := authorityGet(t, handler, "127.0.0.1:7879", "/ui/ops/update", cookie)
	statusBody := readResponseBody(t, status)
	if status.StatusCode != http.StatusOK || !strings.Contains(statusBody, "当前版本") {
		t.Fatalf("查看升级状态不应要求重新验证，状态码=%d 页面：\n%s", status.StatusCode, statusBody)
	}
	csrf := hiddenInputValue(t, statusBody, "csrf_token")

	apply := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/ops/update", url.Values{
		"csrf_token": {csrf},
	}, cookie)
	applyBody := readResponseBody(t, apply)
	if apply.StatusCode != http.StatusSeeOther || apply.Header.Get("Location") !=
		"/ui/reverify?next=%2Fui%2Fops%2Fupdate" {
		t.Fatalf("未重新验证的升级应跳转重验页，状态码=%d 跳转=%q 页面：\n%s",
			apply.StatusCode, apply.Header.Get("Location"), applyBody)
	}
	restart := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/ops/update/restart", url.Values{
		"csrf_token": {csrf},
	}, cookie)
	restartBody := readResponseBody(t, restart)
	if restart.StatusCode != http.StatusSeeOther || restart.Header.Get("Location") !=
		"/ui/reverify?next=%2Fui%2Fops%2Fupdate" {
		t.Fatalf("未重新验证的重启应跳转重验页，状态码=%d 跳转=%q 页面：\n%s",
			restart.StatusCode, restart.Header.Get("Location"), restartBody)
	}
	if ops.updateApplySourceIP != "" || ops.restartSourceIP != "" || ops.restarts != 0 {
		t.Fatalf("未重新验证时不应有副作用：升级来源=%q 重启来源=%q 重启次数=%d",
			ops.updateApplySourceIP, ops.restartSourceIP, ops.restarts)
	}

	reverifyPage := authorityGet(t, handler, "127.0.0.1:7879", "/ui/reverify?next=/ui/ops/update", cookie)
	reverifyBody := readResponseBody(t, reverifyPage)
	reverifyCSRF := hiddenInputValue(t, reverifyBody, "csrf_token")
	reverified := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/reverify", url.Values{
		"csrf_token": {reverifyCSRF}, "password": {"correct-password"}, "next": {"/ui/ops/update"},
	}, cookie)
	reverified.Body.Close()
	if reverified.StatusCode != http.StatusSeeOther {
		t.Fatalf("重新验证应回到升级页，状态码=%d", reverified.StatusCode)
	}

	verifiedApply := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/ops/update", url.Values{
		"csrf_token": {csrf},
	}, cookie)
	verifiedApplyBody := readResponseBody(t, verifiedApply)
	if verifiedApply.StatusCode != http.StatusOK || ops.updateApplySourceIP != "198.51.100.9" {
		t.Fatalf("重新验证后升级应通过，状态码=%d 页面：\n%s", verifiedApply.StatusCode, verifiedApplyBody)
	}
	verifiedRestart := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/ops/update/restart", url.Values{
		"csrf_token": {csrf},
	}, cookie)
	verifiedRestartBody := readResponseBody(t, verifiedRestart)
	select {
	case <-ops.restartDone:
	case <-time.After(2 * time.Second):
		t.Fatal("等待重新验证后的重启动作超时")
	}
	if verifiedRestart.StatusCode != http.StatusOK || ops.restartSourceIP != "198.51.100.9" {
		t.Fatalf("重新验证后重启应通过，状态码=%d 页面：\n%s", verifiedRestart.StatusCode, verifiedRestartBody)
	}
}

func TestAuthorityOpsUpdateRestartReverifyReturnsToGetUpdatePage(t *testing.T) {
	ops := &stubAuthorityOpsUpdateStore{stubOpsUpdateStore: newStubOpsUpdateStore()}
	ops.updateStatus = upgradeTestStatus()
	handler, auth, cookie := newAuthorityOpsUpdateTestHandler(t, ops)
	auth.now = auth.now.Add(6 * time.Minute)
	page := authorityGet(t, handler, "127.0.0.1:7879", "/ui/ops/update", cookie)
	csrf := hiddenInputValue(t, readResponseBody(t, page), "csrf_token")

	rejected := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/ops/update/restart", url.Values{
		"csrf_token": {csrf},
	}, cookie)
	rejectedBody := readResponseBody(t, rejected)
	wantRedirect := "/ui/reverify?next=" + url.QueryEscape("/ui/ops/update")
	if rejected.StatusCode != http.StatusSeeOther || rejected.Header.Get("Location") != wantRedirect ||
		ops.restartScheduled || ops.restarts != 0 {
		t.Fatalf("未验证重启应回跳可 GET 的升级页且无副作用，状态码=%d 跳转=%q 已调度=%v 重启=%d 页面：\n%s",
			rejected.StatusCode, rejected.Header.Get("Location"), ops.restartScheduled, ops.restarts, rejectedBody)
	}
}

func TestAuthorityOpsUpdateWritesRequireOriginAndCSRFToken(t *testing.T) {
	ops := &stubAuthorityOpsUpdateStore{stubOpsUpdateStore: newStubOpsUpdateStore()}
	ops.updateStatus = upgradeTestStatus()
	handler, _, cookie := newAuthorityOpsUpdateTestHandler(t, ops)
	page := authorityGet(t, handler, "127.0.0.1:7879", "/ui/ops/update", cookie)
	csrf := hiddenInputValue(t, readResponseBody(t, page), "csrf_token")

	tests := []struct {
		name   string
		target string
		origin string
		csrf   string
	}{
		{name: "升级缺少令牌", target: "/ui/ops/update", origin: "https://127.0.0.1:7879"},
		{name: "升级来源不对", target: "/ui/ops/update", origin: "https://example.invalid", csrf: csrf},
		{name: "重启缺少令牌", target: "/ui/ops/update/restart", origin: "https://127.0.0.1:7879"},
		{name: "重启来源不对", target: "/ui/ops/update/restart", origin: "https://example.invalid", csrf: csrf},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			form := url.Values{}
			if test.csrf != "" {
				form.Set("csrf_token", test.csrf)
			}
			request := httptest.NewRequest(http.MethodPost, test.target, strings.NewReader(form.Encode()))
			request.Host = "127.0.0.1:7879"
			request.RemoteAddr = "198.51.100.9:45678"
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request.Header.Set("Origin", test.origin)
			request.AddCookie(cookie)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			response := recorder.Result()
			body := readResponseBody(t, response)
			if response.StatusCode != http.StatusForbidden {
				t.Fatalf("防护失败应返回 403，状态码=%d 页面：\n%s", response.StatusCode, body)
			}
		})
	}
	if ops.updateApplySourceIP != "" || ops.restartSourceIP != "" || ops.restarts != 0 {
		t.Fatalf("防护失败不应有副作用：升级来源=%q 重启来源=%q 重启次数=%d",
			ops.updateApplySourceIP, ops.restartSourceIP, ops.restarts)
	}
}

func (s *stubAuthorityOpsUpdateStore) RequestServiceRestart(sourceIP string, afterResponse func(func())) error {
	s.restartSourceIP = sourceIP
	if s.restartErr != nil {
		return s.restartErr
	}
	s.restartScheduled = true
	afterResponse(func() {
		if s.restartAtSchedule != nil {
			s.restartAtSchedule()
		}
		s.restarts++
		close(s.restartDone)
	})
	return nil
}

func newAuthorityOpsUpdateTestHandler(t *testing.T, ops OpsStore) (http.Handler, *fakeAuthorityAuth, *http.Cookie) {
	t.Helper()
	auth := &fakeAuthorityAuth{passwordSet: true, password: "correct-password", now: time.Unix(1800000000, 0)}
	handler, _, err := NewAuthority(Config{
		Role: RoleServer, Servers: &stubServerSource{}, Access: &stubAccessStore{},
		Ops: ops, Auth: auth, AllowedHosts: []string{"127.0.0.1"}, Port: 7879,
		Now: func() time.Time { return auth.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie := authoritySuccessfulLogin(t, handler, "127.0.0.1:7879", "correct-password")
	return handler, auth, cookie
}

func TestAuthorityOpsUpdateApplyShowsPendingRestart(t *testing.T) {
	ops := &stubAuthorityOpsUpdateStore{stubOpsUpdateStore: newStubOpsUpdateStore()}
	ops.updateStatus = upgradeTestStatus()
	ops.updateResult = OpsUpdateApplyResult{OldVersion: "0.1.0", NewVersion: "v0.2.0"}
	handler, _, cookie := newAuthorityOpsUpdateTestHandler(t, ops)

	page := authorityGet(t, handler, "127.0.0.1:7879", "/ui/ops/update", cookie)
	pageBody := readResponseBody(t, page)
	if page.StatusCode != http.StatusOK || !strings.Contains(pageBody, "当前版本") {
		t.Fatalf("权威端应能查看升级状态，状态码=%d 页面：\n%s", page.StatusCode, pageBody)
	}
	csrf := hiddenInputValue(t, pageBody, "csrf_token")

	response := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/ops/update", url.Values{
		"csrf_token": {csrf},
	}, cookie)
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("权威端替换二进制应成功，状态码=%d 页面：\n%s", response.StatusCode, body)
	}
	for _, want := range []string{
		"已把 0.1.0 替换为 v0.2.0",
		"待重启服务",
		"重启会断开活动隧道，待批准的配对请求会作废",
		`action="/ui/ops/update/restart"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("权威端升级结果页缺少 %q：\n%s", want, body)
		}
	}
	if ops.updateApplySourceIP != "198.51.100.9" {
		t.Fatalf("权威端替换二进制应带来源地址，实际 %q", ops.updateApplySourceIP)
	}
}
