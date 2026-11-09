package webui

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestStandaloneOpsUpgradeServerRequiresConfirmationAndShutsDownAfterPage(t *testing.T) {
	initializer := &upgradeServerInitializer{}
	shutdown := make(chan struct{}, 1)
	handler, startup, err := New(Config{
		Role:        RoleStandalone,
		Servers:     &stubServerSource{rows: []ServerRow{{ID: "srv-1", Name: "web"}}},
		Access:      &nextStepAccessStore{},
		MachineInit: initializer,
		RequestShutdown: func() {
			shutdown <- struct{}{}
		},
		Port: 18080,
	})
	if err != nil {
		t.Fatalf("构造单机 Web UI 失败：%v", err)
	}
	session := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookie := session.Cookies()[0]
	session.Body.Close()

	page := doGetWithCookie(t, handler, cookie, "/ui/ops/upgrade-server")
	pageBody := readResponseBody(t, page)
	if page.StatusCode != http.StatusOK {
		t.Fatalf("升级确认页状态码 = %d，应为 200：\n%s", page.StatusCode, pageBody)
	}
	for _, want := range []string{
		"升级为权威端", "已有服务器、Profile、项目令牌与机密项保持不变",
		"需要设置管理员口令", "提权安装 serve", "正式 Web UI 改由 serve 提供",
		`action="/ui/ops/upgrade-server/confirm"`,
	} {
		if !strings.Contains(pageBody, want) {
			t.Fatalf("升级确认页应包含 %q：\n%s", want, pageBody)
		}
	}
	if initializer.calls != 0 {
		t.Fatalf("打开确认页不应改写角色，实际调用 %d 次", initializer.calls)
	}

	form := url.Values{
		"csrf_token": {hiddenInputValue(t, pageBody, "csrf_token")},
		"confirm":    {"1"},
	}
	response := postAccessWrite(t, handler, cookie, "/ui/ops/upgrade-server/confirm", form,
		"http://127.0.0.1:18080", "")
	responseBody := readResponseBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("确认升级状态码 = %d，应为 200：\n%s", response.StatusCode, responseBody)
	}
	if !strings.Contains(responseBody, "`sshmgr ui` 即将退出，再次运行它进入权威端初始化向导") {
		t.Fatalf("升级成功页应提示退出与下一步：\n%s", responseBody)
	}
	if initializer.calls != 1 {
		t.Fatalf("确认升级应调用初始化模块改为权威端，调用=%d", initializer.calls)
	}

	select {
	case <-shutdown:
	case <-time.After(2 * time.Second):
		t.Fatal("升级成功页返回后应触发 Web UI 退出")
	}
}

func TestOpsUpgradeServerRouteAndEntryOnlyForStandalone(t *testing.T) {
	client, clientStartup, err := New(Config{
		Role:        RoleClient,
		Servers:     &stubServerSource{},
		MachineInit: &upgradeServerInitializer{},
		Port:        18080,
	})
	if err != nil {
		t.Fatalf("构造客户端 Web UI 失败：%v", err)
	}
	clientSession := exchangeStartupToken(t, client, clientStartup.URL("127.0.0.1:18080"))
	clientCookie := clientSession.Cookies()[0]
	clientSession.Body.Close()
	clientHome := readResponseBody(t, doGetWithCookie(t, client, clientCookie, "/ui/"))
	if strings.Contains(clientHome, "升级为权威端") {
		t.Fatalf("客户端不应显示升级为权威端入口：\n%s", clientHome)
	}
	clientRoute := doGetWithCookie(t, client, clientCookie, opsUpgradeServerPath)
	clientRoute.Body.Close()
	if clientRoute.StatusCode != http.StatusNotFound {
		t.Fatalf("客户端访问升级为权威端路由状态码 = %d，应为 404", clientRoute.StatusCode)
	}

	authority, _, err := NewAuthority(Config{
		Role:         RoleServer,
		Servers:      &stubServerSource{},
		Access:       &stubAccessStore{},
		Auth:         &fakeAuthorityAuth{passwordSet: true, password: "correct-password"},
		MachineInit:  &upgradeServerInitializer{},
		AllowedHosts: []string{"127.0.0.1"},
		Port:         7879,
	})
	if err != nil {
		t.Fatalf("构造权威端 Web UI 失败：%v", err)
	}
	authorityCookie := authoritySuccessfulLogin(t, authority, "127.0.0.1:7879", "correct-password")
	authorityHome := readResponseBody(t, authorityGet(t, authority, "127.0.0.1:7879", "/ui/", authorityCookie))
	if strings.Contains(authorityHome, "升级为权威端") {
		t.Fatalf("权威端不应显示升级为权威端入口：\n%s", authorityHome)
	}
	authorityRoute := authorityGet(t, authority, "127.0.0.1:7879", opsUpgradeServerPath, authorityCookie)
	authorityRoute.Body.Close()
	if authorityRoute.StatusCode != http.StatusNotFound {
		t.Fatalf("权威端访问升级为权威端路由状态码 = %d，应为 404", authorityRoute.StatusCode)
	}
}

func TestStandaloneOpsAuditPageShowsUpgradeServerEntry(t *testing.T) {
	handler, startup, err := New(Config{
		Role:        RoleStandalone,
		Servers:     &stubServerSource{},
		Access:      &stubAccessStore{},
		Ops:         &stubOpsStore{},
		MachineInit: &upgradeServerInitializer{},
		Port:        18080,
	})
	if err != nil {
		t.Fatalf("构造单机 Web UI 失败：%v", err)
	}
	session := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookie := session.Cookies()[0]
	session.Body.Close()

	page := readResponseBody(t, doGetWithCookie(t, handler, cookie, opsAuditPath))
	if !strings.Contains(page, `id="ops-upgrade-server-entry"`) ||
		!strings.Contains(page, `href="/ui/ops/upgrade-server"`) {
		t.Fatalf("单机运维页应提供升级为权威端入口：\n%s", page)
	}
}

func TestOpsUpgradeServerWriteKeepsOriginCSRFAndExplicitConfirmation(t *testing.T) {
	tests := []struct {
		name    string
		origin  string
		csrf    string
		confirm string
		want    int
	}{
		{name: "缺少来源", csrf: "correct", confirm: "1", want: http.StatusForbidden},
		{name: "来源不符", origin: "https://evil.example", csrf: "correct", confirm: "1", want: http.StatusForbidden},
		{name: "缺少防跨站请求伪造令牌", origin: "http://127.0.0.1:18080", confirm: "1", want: http.StatusForbidden},
		{name: "未勾选确认", origin: "http://127.0.0.1:18080", csrf: "correct", want: http.StatusBadRequest},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			initializer := &upgradeServerInitializer{}
			handler, startup, err := New(Config{
				Role:        RoleStandalone,
				Servers:     &stubServerSource{},
				Access:      &stubAccessStore{},
				MachineInit: initializer,
				Port:        18080,
			})
			if err != nil {
				t.Fatalf("构造单机 Web UI 失败：%v", err)
			}
			session := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
			cookie := session.Cookies()[0]
			session.Body.Close()
			pageBody := readResponseBody(t, doGetWithCookie(t, handler, cookie, opsUpgradeServerPath))
			csrf := test.csrf
			if csrf == "correct" {
				csrf = hiddenInputValue(t, pageBody, "csrf_token")
			}
			form := url.Values{"confirm": {test.confirm}}
			response := postAccessWrite(t, handler, cookie, opsUpgradeServerConfirmPath, form, test.origin, csrf)
			responseBody := readResponseBody(t, response)
			if response.StatusCode != test.want {
				t.Fatalf("状态码 = %d，应为 %d：\n%s", response.StatusCode, test.want, responseBody)
			}
			if initializer.calls != 0 {
				t.Fatalf("被拒绝的确认请求不得调用初始化模块，实际调用 %d 次", initializer.calls)
			}
		})
	}
}

type upgradeServerInitializer struct {
	calls int
	err   error
}

func (s *upgradeServerInitializer) InitializeServer() error {
	s.calls++
	return s.err
}
