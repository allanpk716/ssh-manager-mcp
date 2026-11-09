package cli

import (
	"bytes"
	"crypto/sha256"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/access"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
)

func TestUICommandStandaloneProvidesUpgradeServerPage(t *testing.T) {
	createVaultStore(t)
	urlCh, _, _, finish := startUICommand(t, nil)
	startupURL := waitUIStartupURL(t, urlCh)

	noRedirect := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     &http.Transport{DisableKeepAlives: true},
	}
	startupResponse, err := noRedirect.Get(startupURL.String())
	if err != nil {
		t.Fatalf("访问启动网址失败：%v", err)
	}
	startupResponse.Body.Close()
	if startupResponse.StatusCode != http.StatusSeeOther || len(startupResponse.Cookies()) == 0 {
		t.Fatalf("启动网址应换取会话，状态码=%d cookie=%d", startupResponse.StatusCode, len(startupResponse.Cookies()))
	}

	homeRequest, err := http.NewRequest(http.MethodGet, "http://"+startupURL.Host+"/ui/", nil)
	if err != nil {
		t.Fatal(err)
	}
	homeRequest.AddCookie(startupResponse.Cookies()[0])
	homeResponse, err := noRedirect.Do(homeRequest)
	if err != nil {
		t.Fatalf("访问首页失败：%v", err)
	}
	homeBody, _ := io.ReadAll(homeResponse.Body)
	homeResponse.Body.Close()
	if !strings.Contains(string(homeBody), `href="/ui/ops/upgrade-server"`) {
		t.Fatalf("单机首页导航应提供升级为权威端入口：\n%s", homeBody)
	}

	upgradeRequest, err := http.NewRequest(http.MethodGet, "http://"+startupURL.Host+"/ui/ops/upgrade-server", nil)
	if err != nil {
		t.Fatal(err)
	}
	upgradeRequest.AddCookie(startupResponse.Cookies()[0])
	upgradeResponse, err := noRedirect.Do(upgradeRequest)
	if err != nil {
		t.Fatalf("访问升级为权威端确认页失败：%v", err)
	}
	upgradeBody, _ := io.ReadAll(upgradeResponse.Body)
	upgradeResponse.Body.Close()
	if upgradeResponse.StatusCode != http.StatusOK || !strings.Contains(string(upgradeBody), "升级为权威端") {
		t.Fatalf("单机应提供升级为权威端确认页，状态码=%d 页面：\n%s", upgradeResponse.StatusCode, upgradeBody)
	}
	if !regexp.MustCompile(`<input type="hidden" name="csrf_token" value="[^"]+">`).Match(upgradeBody) {
		t.Fatalf("升级确认页应包含防跨站请求伪造令牌：\n%s", upgradeBody)
	}

	if err := finish(t); err != nil {
		t.Fatalf("ui 命令退出失败：%v", err)
	}
}

func TestUICommandUpgradeServerRewritesRoleAndPreservesVault(t *testing.T) {
	dbPath, masterKey := withCliStoreEnv(t)
	st, err := store.Open(dbPath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddServer(&models.Server{Name: "web", User: "root", Host: "192.0.2.10", Port: 22}); err != nil {
		t.Fatal(err)
	}
	accessManager := access.New(st)
	profileID, err := accessManager.AddProfile("运维", access.CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := accessManager.AddProject("agent", profileID, access.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := roles.Save(roles.State{Role: roles.RoleStandalone, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	databaseBefore, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	urlCh := make(chan string, 1)
	prevOpenBrowser := uiOpenBrowser
	uiOpenBrowser = func(startupURL string) error {
		urlCh <- startupURL
		return nil
	}
	t.Cleanup(func() { uiOpenBrowser = prevOpenBrowser })
	root := NewRootCmd()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	stdinReader, stdinWriter := io.Pipe()
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetIn(stdinReader)
	root.SetArgs([]string{"ui"})
	commandErr := make(chan error, 1)
	go func() { commandErr <- root.Execute() }()
	t.Cleanup(func() { _ = stdinWriter.Close() })

	var startupURL string
	select {
	case startupURL = <-urlCh:
	case <-time.After(5 * time.Second):
		t.Fatal("ui 命令未调用打开浏览器函数")
	}
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     &http.Transport{DisableKeepAlives: true},
	}
	startupResponse, err := client.Get(startupURL)
	if err != nil {
		t.Fatal(err)
	}
	startupResponse.Body.Close()
	if startupResponse.StatusCode != http.StatusSeeOther || len(startupResponse.Cookies()) == 0 {
		t.Fatalf("启动网址应换取会话，状态码=%d cookie=%d", startupResponse.StatusCode, len(startupResponse.Cookies()))
	}

	startup, err := url.Parse(startupURL)
	if err != nil {
		t.Fatal(err)
	}
	pageRequest, err := http.NewRequest(http.MethodGet, "http://"+startup.Host+"/ui/ops/upgrade-server", nil)
	if err != nil {
		t.Fatal(err)
	}
	pageRequest.AddCookie(startupResponse.Cookies()[0])
	pageResponse, err := client.Do(pageRequest)
	if err != nil {
		t.Fatal(err)
	}
	pageBody, _ := io.ReadAll(pageResponse.Body)
	pageResponse.Body.Close()
	if pageResponse.StatusCode != http.StatusOK {
		t.Fatalf("升级确认页状态码=%d，应为 200：\n%s", pageResponse.StatusCode, pageBody)
	}
	csrf := regexp.MustCompile(`<input type="hidden" name="csrf_token" value="([^"]+)"`).FindStringSubmatch(string(pageBody))
	if csrf == nil {
		t.Fatalf("升级确认页缺少防跨站请求伪造令牌：\n%s", pageBody)
	}

	form := url.Values{"csrf_token": {csrf[1]}, "confirm": {"1"}}
	confirmRequest, err := http.NewRequest(http.MethodPost, "http://"+startup.Host+"/ui/ops/upgrade-server/confirm", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	confirmRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	confirmRequest.Header.Set("Origin", "http://"+startup.Host)
	confirmRequest.AddCookie(startupResponse.Cookies()[0])
	confirmResponse, err := client.Do(confirmRequest)
	if err != nil {
		t.Fatal(err)
	}
	confirmBody, _ := io.ReadAll(confirmResponse.Body)
	confirmResponse.Body.Close()
	if confirmResponse.StatusCode != http.StatusOK ||
		!strings.Contains(string(confirmBody), "`sshmgr ui` 即将退出，再次运行它进入权威端初始化向导") {
		t.Fatalf("确认升级应先返回成功页，状态码=%d 页面：\n%s", confirmResponse.StatusCode, confirmBody)
	}

	select {
	case err := <-commandErr:
		if err != nil {
			t.Fatalf("升级后 ui 命令应成功退出：%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("升级成功页返回后 ui 命令应自行退出")
	}
	_ = stdinWriter.Close()
	_ = stdinReader.Close()

	state, err := roles.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.Role != roles.RoleServer || !state.SetupComplete {
		t.Fatalf("升级后 role.json 应为权威端且初始化完成，实际 %+v", state)
	}
	databaseAfter, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(databaseBefore) != sha256.Sum256(databaseAfter) {
		t.Fatal("升级只应改写 role.json，不应修改 vault 数据库文件")
	}

	st, err = store.Open(dbPath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	servers, err := st.ListServers()
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := st.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	projects, err := st.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || servers[0].Name != "web" || len(profiles) != 1 || profiles[0].Name != "运维" ||
		len(projects) != 1 || projects[0].Name != "agent" {
		t.Fatalf("升级后 vault 数据应保持不变：服务器=%+v Profile=%+v 项目=%+v", servers, profiles, projects)
	}
}
