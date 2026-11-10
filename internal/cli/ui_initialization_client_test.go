package cli

import (
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

func TestUICommandClientBranchGuidesExitsAndWritesNothing(t *testing.T) {
	prepareUninitializedUIEnv(t)
	urlCh, stdout, stderr, finish := startUICommand(t, nil)
	startupURL := waitUIStartupURL(t, urlCh)
	entriesBefore := initializationMachineEntries(t)

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     &http.Transport{DisableKeepAlives: true},
	}
	start, err := client.Get(startupURL.String())
	if err != nil {
		t.Fatal(err)
	}
	start.Body.Close()
	if start.StatusCode != http.StatusSeeOther || len(start.Cookies()) != 1 {
		t.Fatalf("向导启动链接应换会话，状态码=%d cookie=%d", start.StatusCode, len(start.Cookies()))
	}
	cookie := start.Cookies()[0]

	getPage := func(path string) string {
		request, requestErr := http.NewRequest(http.MethodGet, "http://"+startupURL.Host+path, nil)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		request.AddCookie(cookie)
		response, requestErr := client.Do(request)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		body, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("访问 %s 状态码=%d，页面：\n%s", path, response.StatusCode, body)
		}
		return html.UnescapeString(string(body))
	}
	postForm := func(path, csrfToken, role string) *http.Response {
		form := url.Values{"csrf_token": {csrfToken}}
		if role != "" {
			form.Set("role", role)
		}
		request, requestErr := http.NewRequest(http.MethodPost, "http://"+startupURL.Host+path, strings.NewReader(form.Encode()))
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Origin", "http://"+startupURL.Host)
		request.AddCookie(cookie)
		response, requestErr := client.Do(request)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		return response
	}

	choice := getPage("/ui/")
	selected := postForm("/ui/init/role", hiddenInputValueForUITest(t, choice), "client")
	selectedBody, _ := io.ReadAll(selected.Body)
	selected.Body.Close()
	if selected.StatusCode != http.StatusSeeOther || selected.Header.Get("Location") != "/ui/init/client" {
		t.Fatalf("选客户端应进入配对指引，状态码=%d 跳转=%q 页面：\n%s", selected.StatusCode, selected.Header.Get("Location"), selectedBody)
	}
	page := getPage("/ui/init/client")
	for _, want := range webui.ClientInitializationGuideLines() {
		if !strings.Contains(page, want) {
			t.Fatalf("客户端指引页应包含 %q：\n%s", want, page)
		}
	}

	waitUIListenerClosed(t, startupURL.Host)
	if err := finish(t); err != nil {
		t.Fatalf("客户端指引返回后 ui 命令应自行成功退出：%v", err)
	}
	terminal := stdout.String() + stderr.String()
	if !strings.Contains(terminal, "客户端入网指引") {
		t.Fatalf("终端应打印客户端入网指引，实际输出：\n%s", terminal)
	}
	for _, want := range webui.ClientInitializationGuideLines() {
		if !strings.Contains(terminal, want) {
			t.Fatalf("终端指引应与页面一致，缺少 %q，实际输出：\n%s", want, terminal)
		}
	}

	state, err := roles.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state != nil {
		t.Fatalf("客户端分支不应写 role.json，实际：%+v", state)
	}
	entriesAfter := initializationMachineEntries(t)
	if !reflect.DeepEqual(entriesBefore, entriesAfter) {
		t.Fatalf("客户端分支不应创建或修改文件。\n操作前：%v\n操作后：%v", entriesBefore, entriesAfter)
	}

	urlCh, _, _, finish = startUICommand(t, nil)
	secondStartupURL := waitUIStartupURL(t, urlCh)
	secondStart, err := client.Get(secondStartupURL.String())
	if err != nil {
		t.Fatal(err)
	}
	secondStart.Body.Close()
	if secondStart.StatusCode != http.StatusSeeOther || len(secondStart.Cookies()) != 1 {
		t.Fatalf("第二次启动仍应进入初始化向导，状态码=%d cookie=%d", secondStart.StatusCode, len(secondStart.Cookies()))
	}
	request, err := http.NewRequest(http.MethodGet, "http://"+secondStartupURL.Host+"/ui/", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(secondStart.Cookies()[0])
	secondRoot, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	secondRootBody, _ := io.ReadAll(secondRoot.Body)
	secondRoot.Body.Close()
	if secondRoot.StatusCode != http.StatusOK || !strings.Contains(string(secondRootBody), "选择本机角色") {
		t.Fatalf("未配对时再次运行应进入初始化向导，状态码=%d 页面：\n%s", secondRoot.StatusCode, secondRootBody)
	}
	if err := finish(t); err != nil {
		t.Fatalf("第二次 ui 命令退出失败：%v", err)
	}
}

func TestUICommandClientCacheOpensReadOnlyInterface(t *testing.T) {
	seedClientCacheSnapshot(t, &store.Snapshot{Version: 1, Servers: []store.SnapshotServer{
		{ID: "s", Name: "服务器", User: "u", Host: "192.0.2.10", Port: 22},
	}}, "")
	credential := []byte(`{"url":"https://192.0.2.9:7878","token":"device-code"}`)
	if err := os.WriteFile(filepath.Join(os.Getenv("SSHMGR_CACHE_DIR"), "cache.auth.json"), credential, 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := roles.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state != nil {
		t.Fatalf("测试应以客户端缓存推断角色，不应预写 role.json，实际：%+v", state)
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
	start.Body.Close()
	if start.StatusCode != http.StatusSeeOther || len(start.Cookies()) != 1 {
		t.Fatalf("客户端缓存机器应启动 Web UI，状态码=%d cookie=%d", start.StatusCode, len(start.Cookies()))
	}
	request, err := http.NewRequest(http.MethodGet, "http://"+startupURL.Host+"/ui/", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(start.Cookies()[0])
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("客户端正式首页状态码=%d，页面：\n%s", response.StatusCode, body)
	}
	for _, want := range []string{"客户端 · 只读", "这里只读，修改需到权威端 Web UI 去做。"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("完成配对后应进入客户端只读界面，缺少 %q：\n%s", want, body)
		}
	}
	if strings.Contains(string(body), "选择本机角色") {
		t.Fatalf("已有客户端缓存时不应再进入初始化向导：\n%s", body)
	}
	if err := finish(t); err != nil {
		t.Fatalf("客户端 ui 命令退出失败：%v", err)
	}
}

func initializationMachineEntries(t *testing.T) map[string]string {
	t.Helper()
	roots := make([]string, 0, 7)
	for _, name := range []string{
		"HOME", "XDG_CONFIG_HOME", "APPDATA", "SSHMGR_CONFIG_ROOT",
		"SSHMGR_STORE", "SSHMGR_CACHE_DIR", "SSHMGR_FILEKEY_PATH",
	} {
		value := os.Getenv(name)
		if value == "" {
			continue
		}
		if info, err := os.Stat(value); err == nil && !info.IsDir() {
			value = filepath.Dir(value)
		}
		roots = append(roots, value)
	}
	seenRoot := make(map[string]bool)
	entries := make(map[string]string)
	for _, root := range roots {
		if seenRoot[root] {
			continue
		}
		seenRoot[root] = true
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				if os.IsNotExist(walkErr) && path == root {
					return nil
				}
				return walkErr
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			kind := "directory"
			if !entry.IsDir() {
				kind = "file"
			}
			entries[path] = kind + ":" + info.ModTime().UTC().Format(time.RFC3339Nano)
			return nil
		})
		if err != nil {
			t.Fatalf("枚举本机状态文件失败：%v", err)
		}
	}
	return entries
}

func waitUIListenerClosed(t *testing.T, hostPort string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		connection, err := net.DialTimeout("tcp", hostPort, 100*time.Millisecond)
		if err != nil {
			return
		}
		_ = connection.Close()
		if time.Now().After(deadline) {
			t.Fatal("客户端指引返回后 ui 命令应自行退出并关闭监听")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
