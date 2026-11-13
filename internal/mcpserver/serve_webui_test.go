package mcpserver

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
	"ssh-manager-mcp/internal/webui"
)

func openLocalTCPListener(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

func serveOptionsForTest(mainListener, webUIListener net.Listener) ServeOpts {
	return ServeOpts{
		mainListener:     mainListener,
		webUIListener:    webUIListener,
		discoveryAddress: "127.0.0.1:0",
	}
}

func prepareServeWebUITest(t *testing.T, role roles.Role) *store.Store {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SSHMGR_SERVE_CERT", filepath.Join(dir, "serve-cert.pem"))
	t.Setenv("SSHMGR_SERVE_KEY", filepath.Join(dir, "serve-key.pem"))
	t.Setenv("SSHMGR_SERVE_MARKER", filepath.Join(dir, "serve-cert.init"))
	storePath := filepath.Join(dir, "test.db")
	t.Setenv("SSHMGR_STORE", storePath)
	if _, _, _, err := LoadOrCreateServeCert(); err != nil {
		t.Fatal(err)
	}
	st := newStoreAt(t, storePath)
	if err := roles.Save(roles.State{Role: role, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestRunServeSeparatesWebUIListenerFromMainListener(t *testing.T) {
	st := prepareServeWebUITest(t, roles.RoleServer)
	mainListener := openLocalTCPListener(t)
	webUIListener := openLocalTCPListener(t)

	runner, err := NewServeRunner(st)
	if err != nil {
		t.Fatal(err)
	}
	mainHandler := runner.HTTPHandler()
	for _, path := range []string{"/ui", "/ui/", "/ui/login"} {
		req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:7878"+path, nil)
		rec := httptest.NewRecorder()
		mainHandler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("主监听 %s 状态码 = %d，应为 404", path, rec.Code)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- RunServe(ctx, st, mainListener.Addr().String(), "", "", serveOptionsForTest(mainListener, webUIListener))
	}()

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // 测试只验证分离监听，不校对自签证书。
		},
		Timeout: 2 * time.Second,
	}
	var body string
	var succeeded bool
	for attempt := 0; attempt < 50; attempt++ {
		resp, err := client.Get("https://" + webUIListener.Addr().String() + "/ui/login")
		if err == nil {
			raw, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr == nil {
				body = string(raw)
			}
			if resp.StatusCode == http.StatusOK && strings.Contains(body, "管理员登录") {
				succeeded = true
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !succeeded {
		t.Fatalf("Web UI 独立端口未打开登录页，最后一次响应：\n%s", body)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunServe 关闭失败：%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RunServe 关闭超时")
	}
}

func TestRunServeFallsBackToLoopbackWhenListenDerivationFails(t *testing.T) {
	st := prepareServeWebUITest(t, roles.RoleServer)
	mainListener := openLocalTCPListener(t)
	webUIListener := openLocalTCPListener(t)
	if err := st.SetSetting(settingWebUIListen, "not-an-ip"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- RunServe(ctx, st, mainListener.Addr().String(), "", "", serveOptionsForTest(mainListener, webUIListener))
	}()

	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		Timeout:   time.Second,
	}
	var lastErr error
	for attempt := 0; attempt < 100; attempt++ {
		select {
		case err := <-done:
			t.Fatalf("监听推导失败不应停止 serve：%v", err)
		default:
		}
		resp, err := client.Get("https://" + webUIListener.Addr().String() + "/ui/login")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				cancel()
				if err := <-done; err != nil {
					t.Fatalf("RunServe 关闭失败：%v", err)
				}
				return
			}
			lastErr = fmt.Errorf("状态码=%d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	t.Fatalf("监听推导失败后 Web UI 应退化为回环监听，最后错误：%v", lastErr)
}

func TestRunServeStartsWebUIOnlyForServerRole(t *testing.T) {
	st := prepareServeWebUITest(t, roles.RoleStandalone)
	mainListener := openLocalTCPListener(t)
	webUIListener := openLocalTCPListener(t)
	mainAddress := mainListener.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- RunServe(ctx, st, mainAddress, "", "", serveOptionsForTest(mainListener, webUIListener))
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		connection, err := net.DialTimeout("tcp", mainAddress, 100*time.Millisecond)
		if err == nil {
			connection.Close()
			break
		}
		select {
		case err := <-done:
			t.Fatalf("serve 主监听不应退出：%v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	if time.Now().After(deadline) {
		t.Fatal("等待主监听启动超时")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunServe 关闭失败：%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RunServe 关闭超时")
	}
	if replacement, err := net.Listen("tcp", webUIListener.Addr().String()); err == nil {
		replacement.Close()
		t.Fatal("单机角色不应打开权威端 Web UI 监听")
	}
}

func TestRunServeWebUIDeviceCodeIssueUsesServePortAndFingerprint(t *testing.T) {
	st := prepareServeWebUITest(t, roles.RoleServer)
	mainListener := openLocalTCPListener(t)
	webUIListener := openLocalTCPListener(t)
	profileID, err := st.AddProfile("ops")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (vault.NewWebUIAuthStore(st)).SetAdminPasswordWithAudit(
		[]byte("correct-password"),
		webui.AuditEvent{Action: "admin.password_set", Status: "ok"},
	); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- RunServe(ctx, st, mainListener.Addr().String(), "", "", serveOptionsForTest(mainListener, webUIListener))
	}()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	base := "https://" + webUIListener.Addr().String()
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, // 测试只验证接线，不校对自签证书。
		Jar:       jar,
		Timeout:   2 * time.Second,
		// 表单提交后的跳转不自动跟随：登录与签发的 303 是断言点，跟随会
		// 把一次性页面误当成跳转目标的响应。
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	var csrfToken string
	for attempt := 0; attempt < 50; attempt++ {
		resp, err := client.Get(base + "/ui/login")
		if err == nil {
			raw, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr == nil && resp.StatusCode == http.StatusOK {
				csrfToken = hiddenCSRFValue(string(raw))
				if csrfToken != "" {
					break
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if csrfToken == "" {
		t.Fatal("登录页没有返回防跨站请求伪造令牌")
	}

	loginForm := url.Values{"csrf_token": {csrfToken}, "password": {"correct-password"}}
	loginResp, err := postAuthorityForm(client, base+"/ui/login", loginForm)
	if err != nil {
		t.Fatal(err)
	}
	loginBody, _ := io.ReadAll(loginResp.Body)
	loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("登录状态码 = %d，应为 303，页面：\n%s", loginResp.StatusCode, loginBody)
	}

	devicePage, err := client.Get(base + "/ui/access/device-codes")
	if err != nil {
		t.Fatal(err)
	}
	deviceBody, _ := io.ReadAll(devicePage.Body)
	devicePage.Body.Close()
	csrfToken = hiddenCSRFValue(string(deviceBody))
	if devicePage.StatusCode != http.StatusOK || csrfToken == "" {
		t.Fatalf("设备码页状态码 = %d，页面：\n%s", devicePage.StatusCode, deviceBody)
	}

	issueForm := url.Values{"csrf_token": {csrfToken}, "name": {"laptop"}, "profile": {profileID}}
	issueResp, err := postAuthorityForm(client, base+"/ui/access/device-codes", issueForm)
	if err != nil {
		t.Fatal(err)
	}
	issueBody, _ := io.ReadAll(issueResp.Body)
	issueResp.Body.Close()
	if issueResp.StatusCode != http.StatusOK {
		t.Fatalf("签发设备码状态码 = %d，应为 200，页面：\n%s", issueResp.StatusCode, issueBody)
	}
	wantURL := "cache pull --url https://" + mainListener.Addr().String()
	if !strings.Contains(string(issueBody), wantURL) {
		t.Fatalf("拉取命令应带权威端主监听地址 %s：\n%s", mainListener.Addr().String(), issueBody)
	}
	if !strings.Contains(string(issueBody), "sha256:") {
		t.Fatalf("一次性页面应带证书公钥指纹：\n%s", issueBody)
	}
	if issueResp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("一次性页面应带 no-store，实际 %q", issueResp.Header.Get("Cache-Control"))
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunServe 关闭失败：%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RunServe 关闭超时")
	}
}

// postAuthorityForm 提交带来源头的表单：权威端 Web UI 的写校验要求
// Origin 与本次监听一致，与浏览器表单提交的请求形状保持相同。
func postAuthorityForm(client *http.Client, target string, form url.Values) (*http.Response, error) {
	parsed, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", parsed.Scheme+"://"+parsed.Host)
	return client.Do(request)
}

func hiddenCSRFValue(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, `name="csrf_token"`) {
			start := strings.Index(line, `value="`)
			if start < 0 {
				return ""
			}
			rest := line[start+len(`value="`):]
			end := strings.Index(rest, `"`)
			if end < 0 {
				return ""
			}
			return rest[:end]
		}
	}
	return ""
}
