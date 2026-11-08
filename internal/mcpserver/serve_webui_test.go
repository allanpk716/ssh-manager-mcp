package mcpserver

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
)

func reserveLocalPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
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
	if err := st.SetSetting(settingWebUIPort, "17879"); err != nil {
		t.Fatal(err)
	}

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
	go func() { done <- RunServe(ctx, st, "127.0.0.1:0", "", "", ServeOpts{}) }()

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // 测试只验证分离监听，不校对自签证书。
		},
		Timeout: 2 * time.Second,
	}
	var body string
	var succeeded bool
	for attempt := 0; attempt < 50; attempt++ {
		resp, err := client.Get("https://127.0.0.1:17879/ui/login")
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
	webPort := reserveLocalPort(t)
	if err := st.SetSetting(settingWebUIPort, strconv.Itoa(webPort)); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(settingWebUIListen, "not-an-ip"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- RunServe(ctx, st, "127.0.0.1:0", "", "", ServeOpts{}) }()

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
		resp, err := client.Get(fmt.Sprintf("https://127.0.0.1:%d/ui/login", webPort))
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
	webPort := reserveLocalPort(t)
	if err := st.SetSetting(settingWebUIPort, strconv.Itoa(webPort)); err != nil {
		t.Fatal(err)
	}
	mainPort := reserveLocalPort(t)
	mainAddress := fmt.Sprintf("127.0.0.1:%d", mainPort)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- RunServe(ctx, st, mainAddress, "", "", ServeOpts{}) }()

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
	if connection, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", webPort), 100*time.Millisecond); err == nil {
		connection.Close()
		cancel()
		<-done
		t.Fatal("单机角色不应打开权威端 Web UI 监听")
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
