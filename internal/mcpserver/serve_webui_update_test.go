package mcpserver

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ssh-manager-mcp/internal/ops"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
	"ssh-manager-mcp/internal/webui"
)

type recordingServiceRestarter struct {
	mu    sync.Mutex
	calls int
}

func (r *recordingServiceRestarter) Restart() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return nil
}

func (r *recordingServiceRestarter) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func TestServeWebUIWiresAuthorityUpdateAndRestart(t *testing.T) {
	st := prepareServeWebUITest(t, roles.RoleServer)
	t.Setenv("SSHMGR_UPDATE_BASE", "http://127.0.0.1:1")
	if _, err := (vault.NewWebUIAuthStore(st)).SetAdminPasswordWithAudit(
		[]byte("correct-password"),
		webui.AuditEvent{Action: "admin.password_set", Status: "ok"},
	); err != nil {
		t.Fatal(err)
	}
	webPort := reserveLocalPort(t)
	if err := st.SetSetting(settingWebUIPort, strconv.Itoa(webPort)); err != nil {
		t.Fatal(err)
	}
	mainPort := reserveLocalPort(t)
	serviceManager := &recordingServiceRestarter{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- RunServe(ctx, st, fmt.Sprintf("127.0.0.1:%d", mainPort), "", "", ServeOpts{
			ServiceRestarterFactory: func() (ops.ServiceRestarter, error) {
				return serviceManager, nil
			},
		})
	}()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	base := fmt.Sprintf("https://127.0.0.1:%d", webPort)
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, // 测试只验证接线，不校对自签证书。
		Jar:       jar,
		Timeout:   2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	var loginCSRF string
	deadline := time.Now().Add(3 * time.Second)
	for loginCSRF == "" && time.Now().Before(deadline) {
		response, err := client.Get(base + "/ui/login")
		if err == nil {
			raw, _ := io.ReadAll(response.Body)
			response.Body.Close()
			loginCSRF = hiddenCSRFValue(string(raw))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if loginCSRF == "" {
		t.Fatal("等待权威端登录页超时")
	}
	login, err := postAuthorityForm(client, base+"/ui/login", url.Values{
		"csrf_token": {loginCSRF}, "password": {"correct-password"},
	})
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(login.Body)
	login.Body.Close()
	if login.StatusCode != http.StatusSeeOther {
		t.Fatalf("登录状态码=%d，应为 303", login.StatusCode)
	}

	updatePage, err := client.Get(base + "/ui/ops/update")
	if err != nil {
		t.Fatal(err)
	}
	updateBody, _ := io.ReadAll(updatePage.Body)
	updatePage.Body.Close()
	if updatePage.StatusCode != http.StatusOK || !strings.Contains(string(updateBody), "重启服务") {
		t.Fatalf("权威端升级页应包含两步重启，状态码=%d 页面：\n%s", updatePage.StatusCode, updateBody)
	}
	csrf := hiddenCSRFValue(string(updateBody))
	if csrf == "" {
		t.Fatal("重启表单缺少防跨站请求伪造令牌")
	}

	restart, err := postAuthorityForm(client, base+"/ui/ops/update/restart", url.Values{
		"csrf_token": {csrf},
	})
	if err != nil {
		t.Fatal(err)
	}
	restartBody, _ := io.ReadAll(restart.Body)
	restart.Body.Close()
	if restart.StatusCode != http.StatusOK || !strings.Contains(string(restartBody), "正在重启，请稍后刷新") {
		t.Fatalf("重启响应应先返回，状态码=%d 页面：\n%s", restart.StatusCode, restartBody)
	}
	if restart.ContentLength != int64(len(restartBody)) {
		t.Fatalf("重启响应应声明完整长度，头部=%d 实际=%d", restart.ContentLength, len(restartBody))
	}
	for range 100 {
		if serviceManager.count() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if serviceManager.count() != 1 {
		t.Fatalf("响应后应调用一次服务管理器，实际 %d 次", serviceManager.count())
	}

	var rows []store.AuditRow
	auditDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(auditDeadline) {
		var err error
		rows, err = st.QueryAudit(store.AuditFilter{Actions: []string{"service.restart"}, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(rows) != 1 || rows[0].Source != store.AuditSourceWebUI || rows[0].SourceIP != "127.0.0.1" ||
		rows[0].Status != "ok" {
		t.Fatalf("服务重启审计不符：%+v", rows)
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
