package cli

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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

	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
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

func TestUICommandUninitializedRefusesWithGuidance(t *testing.T) {
	prepareUninitializedUIEnv(t)
	urlCh, stdout, stderr, finish := startUICommand(t, nil)
	err := finish(t)
	if err == nil {
		t.Fatal("未初始化机器运行 ui 应以错误退出")
	}
	select {
	case <-urlCh:
		t.Fatal("未初始化角色不应打开浏览器（不启动监听）")
	default:
	}
	combined := stdout.String() + stderr.String() + err.Error()
	if !strings.Contains(combined, "初始化") {
		t.Fatalf("未初始化拒绝说明应指向初始化路径，实际：%s", combined)
	}
}
