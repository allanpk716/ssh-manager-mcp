package cli

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
)

// startUICommand 在独立 goroutine 里运行 ui 命令，把注入的打开浏览器函数
// 收到的网址写入通道；finish 关闭标准输入（模拟输入结束）并等待命令退出。
func startUICommand(t *testing.T, args []string) (<-chan string, *bytes.Buffer, *bytes.Buffer, func(t *testing.T) error) {
	t.Helper()
	urlCh := make(chan string, 1)
	prev := uiOpenBrowser
	uiOpenBrowser = func(u string) error {
		urlCh <- u
		return nil
	}
	t.Cleanup(func() { uiOpenBrowser = prev })

	root := NewRootCmd()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	stdinReader, stdinWriter := io.Pipe()
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetIn(stdinReader)
	root.SetArgs(append([]string{"ui"}, args...))
	errCh := make(chan error, 1)
	go func() { errCh <- root.Execute() }()

	finish := func(t *testing.T) error {
		t.Helper()
		_ = stdinWriter.Close()
		select {
		case err := <-errCh:
			_ = stdinReader.Close()
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("ui 命令在标准输入结束后未退出")
			return nil
		}
	}
	return urlCh, stdout, stderr, finish
}

// waitUIStartupURL 等待命令打印的启动网址。
func waitUIStartupURL(t *testing.T, urlCh <-chan string) *url.URL {
	t.Helper()
	select {
	case raw := <-urlCh:
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("启动网址非法：%v", err)
		}
		return u
	case <-time.After(5 * time.Second):
		t.Fatal("ui 命令未调用打开浏览器函数")
		return nil
	}
}

// prepareUninitializedUIEnv 让角色判定得到「未初始化」：既没有 vault 也没有
// 客户端缓存。
func prepareUninitializedUIEnv(t *testing.T) {
	t.Helper()
	userDir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", userDir)
	t.Setenv("APPDATA", userDir)
	t.Setenv("SSHMGR_CONFIG_ROOT", userDir)
	t.Setenv("SSHMGR_STORE", filepath.Join(t.TempDir(), "store.db"))
	t.Setenv("SSHMGR_CACHE_DIR", filepath.Join(t.TempDir(), "cache"))
}

// createVaultStore 在测试环境里真实创建 vault 数据库文件，让角色判定的
// 「本机已有 vault」探测成立。
func createVaultStore(t *testing.T) {
	t.Helper()
	dbPath, mk := withCliStoreEnv(t)
	st, err := store.Open(dbPath, mk)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestUICommandStandaloneListensOnLoopbackRandomPort(t *testing.T) {
	createVaultStore(t)
	urlCh, stdout, _, finish := startUICommand(t, nil)

	u := waitUIStartupURL(t, urlCh)
	host, portText, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("启动网址缺少端口：%v", err)
	}
	port, _ := strconv.Atoi(portText)
	if host != "127.0.0.1" {
		t.Fatalf("监听地址 = %q，应只监听 127.0.0.1", host)
	}
	if port <= 0 {
		t.Fatalf("监听端口 = %d，应为随机非零端口", port)
	}

	noRedirect := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     &http.Transport{DisableKeepAlives: true},
	}
	resp, err := noRedirect.Get(u.String())
	if err != nil {
		t.Fatalf("访问启动网址失败：%v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("启动网址应能换取会话，状态码 = %d", resp.StatusCode)
	}
	if len(resp.Cookies()) == 0 {
		t.Fatal("启动网址应签发会话 cookie")
	}

	if err := finish(t); err != nil {
		t.Fatalf("ui 命令退出失败：%v", err)
	}
	if !strings.Contains(stdout.String(), u.String()) {
		t.Fatalf("终端应打印完整启动网址 %q，实际输出：\n%s", u.String(), stdout.String())
	}

	if _, err := http.Get("http://" + u.Host + "/ui/"); err == nil {
		t.Fatal("命令退出后监听应关闭")
	}
}

func TestUICommandStandaloneOpensAccessProfilePage(t *testing.T) {
	createVaultStore(t)
	urlCh, _, _, finish := startUICommand(t, nil)
	u := waitUIStartupURL(t, urlCh)

	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedirect.Get(u.String())
	if err != nil {
		t.Fatalf("访问启动网址失败：%v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || len(resp.Cookies()) == 0 {
		t.Fatalf("启动网址应换取会话，状态码=%d cookie=%d", resp.StatusCode, len(resp.Cookies()))
	}
	profileRequest, err := http.NewRequest(http.MethodGet, "http://"+u.Host+"/ui/access/profiles", nil)
	if err != nil {
		t.Fatal(err)
	}
	profileRequest.AddCookie(resp.Cookies()[0])
	profileResponse, err := noRedirect.Do(profileRequest)
	if err != nil {
		t.Fatalf("访问 Profile 页失败：%v", err)
	}
	body, _ := io.ReadAll(profileResponse.Body)
	profileResponse.Body.Close()
	if profileResponse.StatusCode != http.StatusOK || !strings.Contains(string(body), "Profile") {
		t.Fatalf("单机 ui 应提供 Profile 页，状态码=%d 页面：\n%s", profileResponse.StatusCode, body)
	}

	if err := finish(t); err != nil {
		t.Fatalf("ui 命令退出失败：%v", err)
	}
}

func TestUICommandStandaloneRejectsInstanceFlag(t *testing.T) {
	createVaultStore(t)
	_, _, _, finish := startUICommand(t, []string{"--instance", "laptop"})

	err := finish(t)
	if err == nil || !strings.Contains(err.Error(), "--instance") {
		t.Fatalf("单机角色带 --instance 应明确报错，实际：%v", err)
	}
}

func TestUICommandClientListensOnLoopback(t *testing.T) {
	seedClientCacheSnapshot(t, &store.Snapshot{Version: 1, Servers: []store.SnapshotServer{
		{ID: "s", Name: "服务器", User: "u", Host: "192.0.2.10", Port: 22, CredentialID: "c", Role: "r"},
	}}, "")
	if err := roles.Save(roles.State{Role: roles.RoleClient, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}

	urlCh, _, _, finish := startUICommand(t, nil)
	u := waitUIStartupURL(t, urlCh)
	host, portText, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("启动网址缺少端口：%v", err)
	}
	port, _ := strconv.Atoi(portText)
	if host != "127.0.0.1" || port <= 0 {
		t.Fatalf("客户端角色监听地址 = %s:%s，应只监听 127.0.0.1 的随机端口", host, portText)
	}
	if err := finish(t); err != nil {
		t.Fatalf("ui 命令退出失败：%v", err)
	}
}

func TestUICommandBrowserOpenFailurePrintsURL(t *testing.T) {
	createVaultStore(t)
	prev := uiOpenBrowser
	captured := make(chan string, 1)
	uiOpenBrowser = func(u string) error {
		captured <- u
		return os.ErrDeadlineExceeded
	}
	t.Cleanup(func() { uiOpenBrowser = prev })

	root := NewRootCmd()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	stdinReader, stdinWriter := io.Pipe()
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetIn(stdinReader)
	root.SetArgs([]string{"ui"})
	errCh := make(chan error, 1)
	go func() { errCh <- root.Execute() }()

	var startupURL string
	select {
	case startupURL = <-captured:
	case <-time.After(5 * time.Second):
		t.Fatal("ui 命令未调用打开浏览器函数")
	}
	_ = stdinWriter.Close()
	if err := <-errCh; err != nil {
		t.Fatalf("打开浏览器失败不应让命令失败：%v", err)
	}
	_ = stdinReader.Close()

	out := stdout.String() + stderr.String()
	if !strings.Contains(out, startupURL) {
		t.Fatalf("打开浏览器失败时终端应包含完整启动网址 %q，实际输出：\n%s\n%s", startupURL, stdout.String(), stderr.String())
	}
	if !strings.Contains(out, "手动") {
		t.Fatalf("打开浏览器失败时应提示手动打开，实际输出：\n%s\n%s", stdout.String(), stderr.String())
	}
}

func TestUICommandServerRolePrintsServeListenerWithoutStartingServer(t *testing.T) {
	createVaultStore(t)
	if err := roles.Save(roles.State{Role: roles.RoleServer, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}

	urlCh, stdout, stderr, finish := startUICommand(t, nil)
	err := finish(t)
	if err != nil {
		t.Fatalf("权威端角色运行 ui 应成功退出：%v", err)
	}
	select {
	case <-urlCh:
		t.Fatal("权威端角色不应打开浏览器（不启动监听）")
	default:
	}
	combined := stdout.String() + stderr.String()
	for _, want := range []string{
		"权威端", "https://127.0.0.1:7879/ui/", "尚未生成证书", "sshmgr serve admin set", "sshmgr ui",
	} {
		if !strings.Contains(combined, want) {
			t.Fatalf("权威端 ui 输出应包含 %q，实际：%s", want, combined)
		}
	}
}

func TestUICommandUnopenableVaultKeepsExistingErrorGuidance(t *testing.T) {
	prepareUninitializedUIEnv(t)
	if err := os.WriteFile(os.Getenv("SSHMGR_STORE"), []byte("not a vault"), 0o600); err != nil {
		t.Fatal(err)
	}
	urlCh, stdout, stderr, finish := startUICommand(t, nil)
	err := finish(t)
	if err == nil {
		t.Fatal("vault 打不开时 ui 应以错误退出")
	}
	select {
	case <-urlCh:
		t.Fatal("未初始化角色不应打开浏览器（不启动监听）")
	default:
	}
	combined := stdout.String() + stderr.String() + err.Error()
	for _, want := range []string{"vault", "sshmgr doctor"} {
		if !strings.Contains(combined, want) {
			t.Fatalf("vault 打不开说明应包含 %q，实际：%s", want, combined)
		}
	}
	if strings.Contains(combined, "sshmgr tui") {
		t.Fatalf("ui 错误不应再指向终端图形界面，实际：%s", combined)
	}
}

func TestUICommandUninitializedRunsStandaloneWizard(t *testing.T) {
	prepareUninitializedUIEnv(t)
	urlCh, _, _, finish := startUICommand(t, nil)
	startupURL := waitUIStartupURL(t, urlCh)

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     &http.Transport{DisableKeepAlives: true},
	}
	start, err := client.Get(startupURL.String())
	if err != nil {
		t.Fatal(err)
	}
	startBody, _ := io.ReadAll(start.Body)
	start.Body.Close()
	if start.StatusCode != http.StatusSeeOther || len(start.Cookies()) != 1 {
		t.Fatalf("向导启动链接应换会话，状态码=%d cookie=%d 页面：\n%s", start.StatusCode, len(start.Cookies()), startBody)
	}
	cookie := start.Cookies()[0]

	getPage := func(path string) (int, string) {
		request, err := http.NewRequest(http.MethodGet, "http://"+startupURL.Host+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.AddCookie(cookie)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		return response.StatusCode, string(body)
	}
	postForm := func(path, csrfToken, role string) *http.Response {
		form := url.Values{"csrf_token": {csrfToken}}
		if role != "" {
			form.Set("role", role)
		}
		request, err := http.NewRequest(http.MethodPost, "http://"+startupURL.Host+path, strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		request.AddCookie(cookie)
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Origin", "http://"+startupURL.Host)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}

	status, choice := getPage("/ui/")
	if status != http.StatusOK || !strings.Contains(choice, "选择本机角色") {
		t.Fatalf("向导首页状态码=%d 页面：\n%s", status, choice)
	}
	csrfToken := hiddenInputValueForUITest(t, choice)
	selected := postForm("/ui/init/role", csrfToken, "standalone")
	selected.Body.Close()
	if selected.StatusCode != http.StatusSeeOther || selected.Header.Get("Location") != "/ui/init/standalone" {
		t.Fatalf("选单机应进入确认页，状态码=%d 跳转=%q", selected.StatusCode, selected.Header.Get("Location"))
	}

	state, err := roles.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.Role != roles.RoleStandalone || state.SetupComplete {
		t.Fatalf("选单机应先写 setup_complete 为假的角色文件：%+v", state)
	}

	status, confirmation := getPage("/ui/init/standalone")
	if status != http.StatusOK || !strings.Contains(confirmation, "创建 vault") {
		t.Fatalf("单机确认页状态码=%d 页面：\n%s", status, confirmation)
	}
	created := postForm("/ui/init/standalone/create", hiddenInputValueForUITest(t, confirmation), "")
	createdBody, _ := io.ReadAll(created.Body)
	created.Body.Close()
	if created.StatusCode != http.StatusOK {
		t.Fatalf("创建 vault 状态码=%d 页面：\n%s", created.StatusCode, createdBody)
	}
	for _, want := range []string{os.Getenv("SSHMGR_FILEKEY_PATH"), "请备份此文件"} {
		if !strings.Contains(string(createdBody), want) {
			t.Fatalf("创建完成页应包含 %q：\n%s", want, createdBody)
		}
	}

	finished := postForm("/ui/init/standalone/finish", hiddenInputValueForUITest(t, string(createdBody)), "")
	finished.Body.Close()
	if finished.StatusCode != http.StatusSeeOther || finished.Header.Get("Location") != "/ui/" {
		t.Fatalf("完成页应进入正式首页，状态码=%d 跳转=%q", finished.StatusCode, finished.Header.Get("Location"))
	}
	status, formal := getPage("/ui/")
	if status != http.StatusOK || !strings.Contains(formal, "服务器") || strings.Contains(formal, "选择本机角色") {
		t.Fatalf("完成初始化后应进入单机正式首页，状态码=%d 页面：\n%s", status, formal)
	}

	state, err = roles.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.Role != roles.RoleStandalone || !state.SetupComplete {
		t.Fatalf("建好 vault 后角色文件应完成初始化：%+v", state)
	}
	if err := finish(t); err != nil {
		t.Fatalf("ui 命令退出失败：%v", err)
	}
}

func hiddenInputValueForUITest(t *testing.T, page string) string {
	t.Helper()
	match := regexp.MustCompile(`<input type="hidden" name="csrf_token" value="([^"]+)"`).FindStringSubmatch(page)
	if match == nil {
		t.Fatalf("页面缺少防跨站请求伪造令牌：\n%s", page)
	}
	return match[1]
}

func TestUICommandStandaloneResumeStartsAtVaultCreation(t *testing.T) {
	prepareUninitializedUIEnv(t)
	if err := roles.Save(roles.State{Role: roles.RoleStandalone, SetupComplete: false}); err != nil {
		t.Fatal(err)
	}
	urlCh, _, _, finish := startUICommand(t, nil)
	startupURL := waitUIStartupURL(t, urlCh)

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     &http.Transport{DisableKeepAlives: true},
	}
	start, err := client.Get(startupURL.String())
	if err != nil {
		t.Fatal(err)
	}
	startCookie := start.Cookies()[0]
	start.Body.Close()
	request, err := http.NewRequest(http.MethodGet, "http://"+startupURL.Host+"/ui/", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(startCookie)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/ui/init/standalone" {
		t.Fatalf("单机续做应从创建 vault 开始，状态码=%d 跳转=%q 页面：\n%s", response.StatusCode, response.Header.Get("Location"), body)
	}

	if err := finish(t); err != nil {
		t.Fatalf("ui 命令退出失败：%v", err)
	}
}
