package mcpserver

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunServeSeparatesWebUIListenerFromMainListener(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SSHMGR_SERVE_CERT", filepath.Join(dir, "serve-cert.pem"))
	t.Setenv("SSHMGR_SERVE_KEY", filepath.Join(dir, "serve-key.pem"))
	t.Setenv("SSHMGR_SERVE_MARKER", filepath.Join(dir, "serve-cert.init"))

	st := newTestStore(t)
	defer st.Close()
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
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // 测试只验证分离监听，不校准自签证书。
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
