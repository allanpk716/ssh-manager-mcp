package webui

import (
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

type stubOpsStore struct {
	query OpsAuditQuery
	rows  []OpsAuditRow
	err   error
	// 备份页测试桩：状态、立即备份与一键校验的结果和调用记录。
	backupStatus OpsBackupStatus
	statusErr    error
	createResult OpsBackupCreateResult
	createErr    error
	createSource OpsWriteSource
	createCalls  int
	verifyResult OpsBackupVerifyResult
	verifyErr    error
	verifySource OpsWriteSource
	verifyCalls  int
}

func (s *stubOpsStore) QueryAudit(query OpsAuditQuery) ([]OpsAuditRow, error) {
	s.query = query
	if s.err != nil {
		return nil, s.err
	}
	return s.rows, nil
}

func (s *stubOpsStore) BackupStatus() (OpsBackupStatus, error) {
	if s.statusErr != nil {
		return OpsBackupStatus{}, s.statusErr
	}
	return s.backupStatus, nil
}

func (s *stubOpsStore) CreateBackup(source OpsWriteSource) (OpsBackupCreateResult, error) {
	s.createCalls++
	s.createSource = source
	if s.createErr != nil {
		return OpsBackupCreateResult{}, s.createErr
	}
	return s.createResult, nil
}

func (s *stubOpsStore) VerifyLatestBackup(source OpsWriteSource) (OpsBackupVerifyResult, error) {
	s.verifyCalls++
	s.verifySource = source
	if s.verifyErr != nil {
		return OpsBackupVerifyResult{}, s.verifyErr
	}
	return s.verifyResult, nil
}

func testOpsRows(count int) []OpsAuditRow {
	rows := make([]OpsAuditRow, 0, count)
	for i := 0; i < count; i++ {
		rows = append(rows, OpsAuditRow{
			ID: int64(count - i), TS: time.Unix(1800000000, 0),
			ProjectName: "alpha", ServerName: "db-1",
			Action: "exec", Command: "uptime", Status: "ok",
			ExitCode: 0, DurationMS: 5, Source: "web_ui", SourceIP: "192.0.2.23",
		})
	}
	return rows
}

func TestOpsAuditPageFiltersAndOlderLink(t *testing.T) {
	ops := &stubOpsStore{rows: testOpsRows(101)}
	handler, startup, err := New(Config{
		Role:    RoleStandalone,
		Servers: &stubServerSource{},
		Access:  &stubAccessStore{},
		Ops:     ops,
		Port:    18080,
	})
	if err != nil {
		t.Fatalf("构造单机 Web UI Handler 失败：%v", err)
	}
	session := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookie := session.Cookies()[0]
	session.Body.Close()

	target := "/ui/ops/audit?since=2027-01-02T03:04&action=exec&server=db-1&project=alpha&owner=1&status=ok&source=web_ui&source_ip=192.0.2.23&before=50"
	response := doGetWithCookie(t, handler, cookie, target)
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "<h1>审计</h1>") {
		t.Fatalf("单机应打开审计页，状态码=%d 页面：\n%s", response.StatusCode, body)
	}
	if !strings.Contains(body, `aria-label="运维区域"`) || !strings.Contains(body, `href="/ui/ops/audit"`) {
		t.Fatalf("导航应显示运维区域与审计入口：\n%s", body)
	}

	wantTime, _ := time.ParseInLocation("2006-01-02T15:04", "2027-01-02T03:04", time.Local)
	got := ops.query
	if got.Since != wantTime.Unix() || got.Actions[0] != "exec" || got.Servers[0] != "db-1" ||
		got.Projects[0] != "alpha" || !got.OwnerOnly || got.Statuses[0] != "ok" ||
		got.Source != "web_ui" || got.SourceIP != "192.0.2.23" || got.BeforeID != 50 || got.Limit != 101 {
		t.Fatalf("审计页传给窄接口的过滤条件不符：%+v", got)
	}

	if strings.Count(body, "<tr class=\"audit-row\">") != 100 {
		t.Fatalf("审计页每页应显示 100 行，页面行数不符：\n%s", body)
	}
	match := regexp.MustCompile(`<a class="older-audit" href="([^"]+)"`).FindStringSubmatch(body)
	if match == nil {
		t.Fatalf("审计页缺少「更早的记录」链接：\n%s", body)
	}
	href, err := url.QueryUnescape(html.UnescapeString(match[1]))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.ParseQuery(strings.TrimPrefix(href, "/ui/ops/audit?"))
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"since": "2027-01-02T03:04", "action": "exec", "server": "db-1",
		"project": "alpha", "owner": "1", "status": "ok", "source": "web_ui",
		"source_ip": "192.0.2.23", "before": "2",
	} {
		if parsed.Get(key) != want {
			t.Fatalf("翻页链接里的 %s = %q，应为 %q，链接=%s", key, parsed.Get(key), want, href)
		}
	}
}

func TestOpsAuditRoutesFollowRole(t *testing.T) {
	standalone, startup, err := New(Config{
		Role: RoleStandalone, Servers: &stubServerSource{}, Access: &stubAccessStore{},
		Ops: &stubOpsStore{}, Port: 18080,
	})
	if err != nil {
		t.Fatal(err)
	}
	standaloneSession := exchangeStartupToken(t, standalone, startup.URL("127.0.0.1:18080"))
	standaloneCookie := standaloneSession.Cookies()[0]
	standaloneSession.Body.Close()
	resp := doGetWithCookie(t, standalone, standaloneCookie, "/ui/ops/")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/ui/ops/audit" {
		t.Fatalf("单机运维入口应跳转审计页，状态码=%d 跳转=%q", resp.StatusCode, resp.Header.Get("Location"))
	}

	auth := &fakeAuthorityAuth{passwordSet: true, password: "correct-password"}
	opsRows := []OpsAuditRow{
		{ID: 3, Action: "admin.login", Status: "ok", Source: "web_ui", SourceIP: "198.51.100.9"},
		{ID: 2, Action: "admin.login_fail", Status: "error", Source: "web_ui", SourceIP: "198.51.100.9"},
		{ID: 1, Action: "admin.lockout", Status: "blocked", Source: "web_ui", SourceIP: "198.51.100.9"},
	}
	authority, _, err := NewAuthority(Config{
		Role: RoleServer, Servers: &stubServerSource{}, Access: &stubAccessStore{}, Ops: &stubOpsStore{rows: opsRows},
		Auth: auth, AllowedHosts: []string{"127.0.0.1"}, Port: 7879,
	})
	if err != nil {
		t.Fatal(err)
	}
	authorityCookie := authoritySuccessfulLogin(t, authority, "127.0.0.1:7879", "correct-password")
	authorityPage := authorityGet(t, authority, "127.0.0.1:7879", "/ui/ops/audit", authorityCookie)
	authorityBody := readResponseBody(t, authorityPage)
	if authorityPage.StatusCode != http.StatusOK || !strings.Contains(authorityBody, "<h1>审计</h1>") {
		t.Fatalf("权威端应打开审计页，状态码=%d 页面：\n%s", authorityPage.StatusCode, authorityBody)
	}
	if !strings.Contains(authorityBody, `aria-label="运维区域"`) {
		t.Fatalf("权威端导航应显示运维区域：\n%s", authorityBody)
	}
	for _, action := range []string{"admin.login", "admin.login_fail", "admin.lockout"} {
		if !strings.Contains(authorityBody, action) || !strings.Contains(authorityBody, "198.51.100.9") {
			t.Fatalf("权威端审计页应显示 %s 与来源地址：\n%s", action, authorityBody)
		}
	}

	client, clientStartup, err := New(Config{
		Role: RoleClient, Servers: &stubServerSource{}, Port: 18080,
	})
	if err != nil {
		t.Fatal(err)
	}
	clientSession := exchangeStartupToken(t, client, clientStartup.URL("127.0.0.1:18080"))
	clientCookie := clientSession.Cookies()[0]
	clientSession.Body.Close()
	resp = doGetWithCookie(t, client, clientCookie, "/ui/ops/audit")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("本票未实现客户端本机审计页，客户端不应注册权威 vault 审计页，状态码=%d", resp.StatusCode)
	}
}
