package webui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func newOpsBackupHandler(t *testing.T, ops OpsStore) (http.Handler, *http.Cookie) {
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

func opsBackupStatusFixture() OpsBackupStatus {
	return OpsBackupStatus{
		ConfigPath:           "/vault/backup.json",
		ConfigDir:            "/backups",
		ConfigKeep:           7,
		ConfigPassphraseFile: "/vault/backup.pass",
		TaskInstalled:        true,
		TaskProbed:           true,
		TotalBackups:         3,
		LatestBackup:         "vault-20260102-000000.sme",
		LatestTime:           time.Unix(1800000000, 0),
		HasLatest:            true,
		Verify: &OpsBackupVerifyState{
			File: "vault-20260102-000000.sme", OK: true, At: time.Unix(1800000600, 0),
		},
	}
}

func TestOpsBackupPageRendersStatusAndActions(t *testing.T) {
	ops := &stubOpsStore{backupStatus: opsBackupStatusFixture()}
	handler, cookie := newOpsBackupHandler(t, ops)

	resp := doGetWithCookie(t, handler, cookie, "/ui/ops/backup")
	body := readResponseBody(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "<h1>备份</h1>") {
		t.Fatalf("单机应打开备份页，状态码=%d 页面：\n%s", resp.StatusCode, body)
	}
	for _, want := range []string{
		`href="/ui/ops/backup"`, "/backups", "7", "/vault/backup.pass", "vault-20260102-000000.sme",
		"已校验", `action="/ui/ops/backup/create"`, `action="/ui/ops/backup/verify"`,
		`name="csrf_token"`, "立即备份", "校验最新备份", "计划任务", "已安装",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("备份页缺少 %q：\n%s", want, body)
		}
	}
	if ops.createCalls != 0 || ops.verifyCalls != 0 {
		t.Fatalf("状态页不应触发写操作：%+v", ops)
	}
}

func TestOpsBackupPageRendersAnomalySuggestions(t *testing.T) {
	cases := []struct {
		name   string
		status OpsBackupStatus
		wants  []string
	}{
		{
			name:   "配置缺失",
			status: OpsBackupStatus{ConfigPath: "/vault/backup.json", ConfigMissing: true, ConfigError: "file not found"},
			wants:  []string{"尚未配置备份", "backup-config", "备份配置"},
		},
		{
			name: "配置有误",
			status: OpsBackupStatus{
				ConfigPath: "/vault/backup.json", ConfigDir: "/backups", ConfigKeep: 7,
				ConfigPassphraseFile: "/vault/backup.pass", ConfigError: "not valid JSON",
			},
			wants: []string{"备份配置有误", "not valid JSON", "备份配置"},
		},
		{
			name: "尚无备份",
			status: OpsBackupStatus{
				ConfigDir: "/backups", ConfigKeep: 7, ConfigPassphraseFile: "/vault/backup.pass",
				TaskInstalled: true, TaskProbed: true,
			},
			wants: []string{"尚无备份", "立即备份"},
		},
		{
			name: "备份过期且计划任务未安装",
			status: OpsBackupStatus{
				ConfigDir: "/backups", ConfigKeep: 7, ConfigPassphraseFile: "/vault/backup.pass",
				TaskProbed: true, TotalBackups: 2, LatestBackup: "vault-20260101-000000.sme",
				LatestTime: time.Unix(1800000000, 0), HasLatest: true, Stale: true,
			},
			wants: []string{"过期", "25 小时", "计划任务未安装", "检查计划任务"},
		},
		{
			name: "最新备份尚未校验",
			status: OpsBackupStatus{
				ConfigDir: "/backups", ConfigKeep: 7, ConfigPassphraseFile: "/vault/backup.pass",
				TaskInstalled: true, TaskProbed: true, TotalBackups: 1,
				LatestBackup: "vault-20260102-000000.sme", LatestTime: time.Unix(1800000000, 0),
				HasLatest: true,
			},
			wants: []string{"最新备份尚未校验", "校验最新备份"},
		},
		{
			name: "最新备份无法解密",
			status: OpsBackupStatus{
				ConfigDir: "/backups", ConfigKeep: 7, ConfigPassphraseFile: "/vault/backup.pass",
				TaskInstalled: true, TaskProbed: true, TotalBackups: 1,
				LatestBackup: "vault-20260102-000000.sme", LatestTime: time.Unix(1800000000, 0),
				HasLatest: true,
				Verify: &OpsBackupVerifyState{
					File: "vault-20260102-000000.sme", FailureKind: "passphrase",
					At: time.Unix(1800000600, 0),
				},
			},
			wants: []string{"最新备份无法解密或已损坏", "口令不符", "重新备份"},
		},
		{
			name: "文件不完整",
			status: OpsBackupStatus{
				ConfigDir: "/backups", ConfigKeep: 7, ConfigPassphraseFile: "/vault/backup.pass",
				TaskInstalled: true, TaskProbed: true, TotalBackups: 1,
				LatestBackup: "vault-20260102-000000.sme", LatestTime: time.Unix(1800000000, 0),
				HasLatest: true,
				Verify: &OpsBackupVerifyState{
					File: "vault-20260102-000000.sme", FailureKind: "incomplete",
					At: time.Unix(1800000600, 0),
				},
			},
			wants: []string{"文件不完整", "重新备份"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ops := &stubOpsStore{backupStatus: tc.status}
			handler, cookie := newOpsBackupHandler(t, ops)
			resp := doGetWithCookie(t, handler, cookie, "/ui/ops/backup")
			body := readResponseBody(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("备份页状态码=%d 页面：\n%s", resp.StatusCode, body)
			}
			for _, want := range tc.wants {
				if !strings.Contains(body, want) {
					t.Fatalf("%s 的备份页缺少 %q：\n%s", tc.name, want, body)
				}
			}
		})
	}
}

func TestOpsBackupCreateAndVerifyActions(t *testing.T) {
	ops := &stubOpsStore{
		backupStatus: opsBackupStatusFixture(),
		createResult: OpsBackupCreateResult{File: "vault-20260304-000000.sme"},
		verifyResult: OpsBackupVerifyResult{File: "vault-20260304-000000.sme", OK: true},
	}
	handler, cookie := newOpsBackupHandler(t, ops)

	created := postStandaloneForm(t, handler, cookie, "/ui/ops/backup/create", url.Values{})
	createdBody := readResponseBody(t, created)
	if created.StatusCode != http.StatusSeeOther || created.Header.Get("Location") != "/ui/ops/backup" {
		t.Fatalf("立即备份后应回到备份页，状态码=%d 跳转=%q 页面：\n%s",
			created.StatusCode, created.Header.Get("Location"), createdBody)
	}
	if ops.createCalls != 1 || ops.createSource.RemoteAddress != "198.51.100.9" {
		t.Fatalf("立即备份应把来源地址传给窄接口：%+v", ops.createSource)
	}

	page := doGetWithCookie(t, handler, cookie, "/ui/ops/backup")
	pageBody := readResponseBody(t, page)
	if !strings.Contains(pageBody, "已备份") || !strings.Contains(pageBody, "vault-20260304-000000.sme") {
		t.Fatalf("立即备份后的页面应显示成功提示：\n%s", pageBody)
	}
	verified := postStandaloneForm(t, handler, cookie, "/ui/ops/backup/verify", url.Values{})
	verifiedBody := readResponseBody(t, verified)
	if verified.StatusCode != http.StatusSeeOther || ops.verifyCalls != 1 ||
		ops.verifySource.RemoteAddress != "198.51.100.9" {
		t.Fatalf("一键校验应提交并回到备份页，状态码=%d 调用=%d 页面：\n%s",
			verified.StatusCode, ops.verifyCalls, verifiedBody)
	}
	page = doGetWithCookie(t, handler, cookie, "/ui/ops/backup")
	pageBody = readResponseBody(t, page)
	if !strings.Contains(pageBody, "已校验") || !strings.Contains(pageBody, "vault-20260304-000000.sme") {
		t.Fatalf("校验后的页面应显示成功提示：\n%s", pageBody)
	}
}

func TestOpsBackupActionFailuresRenderFlash(t *testing.T) {
	cases := []struct {
		name   string
		target string
		ops    *stubOpsStore
		want   string
	}{
		{
			name:   "备份失败",
			target: "/ui/ops/backup/create",
			ops:    &stubOpsStore{createErr: errTest{"marker missing"}},
			want:   "立即备份失败",
		},
		{
			name:   "备份被跳过",
			target: "/ui/ops/backup/create",
			ops:    &stubOpsStore{createResult: OpsBackupCreateResult{Skipped: true}},
			want:   "已有备份正在进行",
		},
		{
			name:   "校验失败",
			target: "/ui/ops/backup/verify",
			ops:    &stubOpsStore{verifyErr: errTest{"decrypt failed"}},
			want:   "校验失败",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.ops.backupStatus = opsBackupStatusFixture()
			handler, cookie := newOpsBackupHandler(t, tc.ops)
			resp := postStandaloneForm(t, handler, cookie, tc.target, url.Values{})
			respBody := readResponseBody(t, resp)
			if resp.StatusCode != http.StatusSeeOther {
				t.Fatalf("%s 后应回到备份页，状态码=%d 页面：\n%s", tc.name, resp.StatusCode, respBody)
			}
			page := doGetWithCookie(t, handler, cookie, "/ui/ops/backup")
			body := readResponseBody(t, page)
			if !strings.Contains(body, tc.want) {
				t.Fatalf("%s 的页面应显示 %q：\n%s", tc.name, tc.want, body)
			}
		})
	}
}

type errTest struct{ message string }

func (e errTest) Error() string { return e.message }

func TestOpsBackupWriteRequestsNeedOriginAndCSRFToken(t *testing.T) {
	for _, target := range []string{"/ui/ops/backup/create", "/ui/ops/backup/verify"} {
		ops := &stubOpsStore{backupStatus: opsBackupStatusFixture()}
		handler, cookie := newOpsBackupHandler(t, ops)

		// 缺少防跨站请求伪造令牌。
		req, _ := http.NewRequest(http.MethodPost, target, nil)
		req.Host = "127.0.0.1:18080"
		req.Header.Set("Origin", "http://127.0.0.1:18080")
		req.AddCookie(cookie)
		rec := newOpsBackupRecorder(handler, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s 缺少令牌应返回 403，实际 %d", target, rec.Code)
		}

		// 来源不对。
		form := url.Values{"csrf_token": {"anything"}}
		req2, _ := http.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
		req2.Host = "127.0.0.1:18080"
		req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req2.Header.Set("Origin", "https://evil.example")
		req2.AddCookie(cookie)
		rec2 := newOpsBackupRecorder(handler, req2)
		if rec2.Code != http.StatusForbidden {
			t.Fatalf("%s 来源不对应返回 403，实际 %d", target, rec2.Code)
		}
		if ops.createCalls != 0 || ops.verifyCalls != 0 {
			t.Fatalf("%s 校验失败的写请求不应到达运维来源：%+v", target, ops)
		}
	}
}

func newOpsBackupRecorder(handler http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestAuthorityOpsBackupCreateRequiresReverify(t *testing.T) {
	auth := &fakeAuthorityAuth{passwordSet: true, password: "correct-password", now: time.Unix(1800000000, 0)}
	ops := &stubOpsStore{
		backupStatus: opsBackupStatusFixture(),
		createResult: OpsBackupCreateResult{File: "vault-20260304-000000.sme"},
	}
	handler, _, err := NewAuthority(Config{
		Role: RoleServer, Servers: &stubServerSource{}, Access: &stubAccessStore{}, Ops: ops,
		Auth: auth, AllowedHosts: []string{"127.0.0.1"}, Port: 7879,
		Now: func() time.Time { return auth.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie := authoritySuccessfulLogin(t, handler, "127.0.0.1:7879", "correct-password")
	auth.now = auth.now.Add(6 * time.Minute)
	page := authorityGet(t, handler, "127.0.0.1:7879", "/ui/ops/backup", cookie)
	pageBody := readResponseBody(t, page)
	csrfToken := hiddenInputValue(t, pageBody, "csrf_token")

	rejected := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/ops/backup/create", url.Values{
		"csrf_token": {csrfToken},
	}, cookie)
	rejectedBody := readResponseBody(t, rejected)
	wantRedirect := `/ui/reverify?next=` + url.QueryEscape("/ui/ops/backup")
	if rejected.StatusCode != http.StatusSeeOther || rejected.Header.Get("Location") != wantRedirect ||
		ops.createCalls != 0 {
		t.Fatalf("权威端立即备份应先重新验证，状态码=%d 跳转=%q 调用=%d 页面：\n%s",
			rejected.StatusCode, rejected.Header.Get("Location"), ops.createCalls, rejectedBody)
	}

	reverifyPage := authorityGet(t, handler, "127.0.0.1:7879", wantRedirect, cookie)
	reverifyBody := readResponseBody(t, reverifyPage)
	reverifyCSRF := hiddenInputValue(t, reverifyBody, "csrf_token")
	reverified := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/reverify", url.Values{
		"csrf_token": {reverifyCSRF}, "password": {"correct-password"}, "next": {"/ui/ops/backup"},
	}, cookie)
	reverifiedBody := readResponseBody(t, reverified)
	if reverified.StatusCode != http.StatusSeeOther || reverified.Header.Get("Location") != "/ui/ops/backup" {
		t.Fatalf("重新验证后应回到备份页，状态码=%d 跳转=%q 页面：\n%s",
			reverified.StatusCode, reverified.Header.Get("Location"), reverifiedBody)
	}

	accepted := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/ops/backup/create", url.Values{
		"csrf_token": {csrfToken},
	}, cookie)
	acceptedBody := readResponseBody(t, accepted)
	if accepted.StatusCode != http.StatusSeeOther || ops.createCalls != 1 ||
		ops.createSource.RemoteAddress != "198.51.100.9" {
		t.Fatalf("重新验证后立即备份应执行，状态码=%d 调用=%d 页面：\n%s",
			accepted.StatusCode, ops.createCalls, acceptedBody)
	}
	flash := authorityGet(t, handler, "127.0.0.1:7879", "/ui/ops/backup", cookie)
	flashBody := readResponseBody(t, flash)
	if !strings.Contains(flashBody, "已备份") || !strings.Contains(flashBody, "vault-20260304-000000.sme") {
		t.Fatalf("权威端备份成功提示应显示：\n%s", flashBody)
	}
}

func TestAuthorityOpsBackupVerifyDoesNotRequireReverify(t *testing.T) {
	auth := &fakeAuthorityAuth{passwordSet: true, password: "correct-password", now: time.Unix(1800000000, 0)}
	ops := &stubOpsStore{
		backupStatus: opsBackupStatusFixture(),
		verifyResult: OpsBackupVerifyResult{File: "vault-20260102-000000.sme", OK: true},
	}
	handler, _, err := NewAuthority(Config{
		Role: RoleServer, Servers: &stubServerSource{}, Access: &stubAccessStore{}, Ops: ops,
		Auth: auth, AllowedHosts: []string{"127.0.0.1"}, Port: 7879,
		Now: func() time.Time { return auth.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie := authoritySuccessfulLogin(t, handler, "127.0.0.1:7879", "correct-password")
	auth.now = auth.now.Add(6 * time.Minute)
	page := authorityGet(t, handler, "127.0.0.1:7879", "/ui/ops/backup", cookie)
	pageBody := readResponseBody(t, page)
	csrfToken := hiddenInputValue(t, pageBody, "csrf_token")

	resp := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/ops/backup/verify", url.Values{
		"csrf_token": {csrfToken},
	}, cookie)
	respBody := readResponseBody(t, resp)
	if resp.StatusCode != http.StatusSeeOther || ops.verifyCalls != 1 {
		t.Fatalf("校验最新备份不应要求重新验证，状态码=%d 调用=%d 页面：\n%s",
			resp.StatusCode, ops.verifyCalls, respBody)
	}
}

func TestOpsBackupClientRoleGetsNotFound(t *testing.T) {
	client, clientStartup, err := New(Config{
		Role: RoleClient, Servers: &stubServerSource{}, Port: 18080,
	})
	if err != nil {
		t.Fatal(err)
	}
	clientSession := exchangeStartupToken(t, client, clientStartup.URL("127.0.0.1:18080"))
	clientCookie := clientSession.Cookies()[0]
	clientSession.Body.Close()
	for _, target := range []string{"/ui/ops/backup", "/ui/ops/backup/create", "/ui/ops/backup/verify"} {
		resp := doGetWithCookie(t, client, clientCookie, target)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("客户端不应注册备份路由 %s，状态码=%d", target, resp.StatusCode)
		}
	}
}
