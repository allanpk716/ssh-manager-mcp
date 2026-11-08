package webui

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func (s *stubOpsStore) QueryClientAudit(instance string, limit int) ([]OpsAuditRow, error) {
	s.clientInstance = instance
	s.clientLimit = limit
	if s.clientErr != nil {
		return nil, s.clientErr
	}
	return s.clientRows, nil
}

func testClientAuditRows() []OpsAuditRow {
	return []OpsAuditRow{
		{
			TS: time.Unix(1800000100, 0), Action: "cache.pull", Command: "同步成功",
			Status: "ok", Source: "web_ui", SourceIP: "127.0.0.1",
		},
		{
			TS: time.Unix(1800000000, 0), ProjectID: "proj_1", ServerID: "srv_1",
			Action: "exec", Command: "uptime", Status: "error", ExitCode: 3,
			DurationMS: 12, Source: "agent",
		},
	}
}

func TestOpsClientAuditPageReadsStartupInstance(t *testing.T) {
	ops := &stubOpsStore{clientRows: testClientAuditRows()}
	handler, startup, err := New(Config{
		Role: RoleClient, Servers: &stubServerSource{}, Ops: ops,
		ClientInstance: "laptop", Port: 18080,
	})
	if err != nil {
		t.Fatalf("构造客户端 Web UI Handler 失败：%v", err)
	}
	session := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookie := session.Cookies()[0]
	session.Body.Close()

	response := doGetWithCookie(t, handler, cookie, "/ui/ops/client-audit")
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "<h1>本机审计</h1>") {
		t.Fatalf("客户端应打开本机审计页，状态码=%d 页面：\n%s", response.StatusCode, body)
	}
	if !strings.Contains(body, `aria-label="运维区域"`) || !strings.Contains(body, `href="/ui/ops/client-audit"`) {
		t.Fatalf("客户端导航应显示运维区域与本机审计入口：\n%s", body)
	}
	if !strings.Contains(body, "laptop") {
		t.Fatalf("本机审计页应显示当前实例名：\n%s", body)
	}
	newest := strings.Index(body, "cache.pull")
	older := strings.Index(body, ">uptime<")
	if newest < 0 || older < 0 || newest > older {
		t.Fatalf("本机审计页应最新记录在前：\n%s", body)
	}
	if ops.clientInstance != "laptop" || ops.clientLimit != 100 {
		t.Fatalf("处理器应把启动实例与每页条数传给窄接口：instance=%q limit=%d", ops.clientInstance, ops.clientLimit)
	}
}

func TestOpsClientAuditRoutesFollowRole(t *testing.T) {
	client, clientStartup, err := New(Config{
		Role: RoleClient, Servers: &stubServerSource{}, Ops: &stubOpsStore{},
		ClientInstance: "laptop", Port: 18080,
	})
	if err != nil {
		t.Fatal(err)
	}
	clientSession := exchangeStartupToken(t, client, clientStartup.URL("127.0.0.1:18080"))
	clientCookie := clientSession.Cookies()[0]
	clientSession.Body.Close()

	root := doGetWithCookie(t, client, clientCookie, "/ui/ops/")
	root.Body.Close()
	if root.StatusCode != http.StatusSeeOther || root.Header.Get("Location") != "/ui/ops/client" {
		t.Fatalf("客户端运维入口应跳转客户端页，状态码=%d 跳转=%q", root.StatusCode, root.Header.Get("Location"))
	}
	for _, path := range []string{"/ui/ops/backup", "/ui/ops/settings"} {
		resp := doGetWithCookie(t, client, clientCookie, path)
		body := readResponseBody(t, resp)
		if resp.StatusCode != http.StatusNotFound || !strings.Contains(body, "页面不存在") {
			t.Fatalf("客户端不应注册 %s，状态码=%d 页面：\n%s", path, resp.StatusCode, body)
		}
	}

	standalone, standaloneStartup, err := New(Config{
		Role: RoleStandalone, Servers: &stubServerSource{}, Access: &stubAccessStore{},
		Ops: &stubOpsStore{}, Port: 18080,
	})
	if err != nil {
		t.Fatal(err)
	}
	standaloneSession := exchangeStartupToken(t, standalone, standaloneStartup.URL("127.0.0.1:18080"))
	standaloneCookie := standaloneSession.Cookies()[0]
	standaloneSession.Body.Close()
	for _, path := range []string{"/ui/ops/client", "/ui/ops/client/sync", "/ui/ops/client/instance"} {
		standalonePage := doGetWithCookie(t, standalone, standaloneCookie, path)
		standaloneBody := readResponseBody(t, standalonePage)
		if standalonePage.StatusCode != http.StatusNotFound {
			t.Fatalf("单机不应注册 %s，状态码=%d", path, standalonePage.StatusCode)
		}
		if strings.Contains(standaloneBody, "客户端页") {
			t.Fatalf("单机导航不应显示客户端页入口：\n%s", standaloneBody)
		}
	}

	auth := &fakeAuthorityAuth{passwordSet: true, password: "correct-password"}
	authority, _, err := NewAuthority(Config{
		Role: RoleServer, Servers: &stubServerSource{}, Access: &stubAccessStore{}, Ops: &stubOpsStore{},
		Auth: auth, AllowedHosts: []string{"127.0.0.1"}, Port: 7879,
	})
	if err != nil {
		t.Fatal(err)
	}
	authorityCookie := authoritySuccessfulLogin(t, authority, "127.0.0.1:7879", "correct-password")
	for _, path := range []string{"/ui/ops/client", "/ui/ops/client/sync", "/ui/ops/client/instance"} {
		authorityPage := authorityGet(t, authority, "127.0.0.1:7879", path, authorityCookie)
		authorityPage.Body.Close()
		if authorityPage.StatusCode != http.StatusNotFound {
			t.Fatalf("权威端不应注册 %s，状态码=%d", path, authorityPage.StatusCode)
		}
	}
}
