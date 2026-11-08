package vault

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

func TestWebUIAuthStoreAdaptsVaultStateAndAudit(t *testing.T) {
	masterKey, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSHMGR_VAULT_ROOT", t.TempDir())
	t.Setenv("SSHMGR_STORE", t.TempDir()+"/test.db")
	st, err := OpenStore(&fakeKeyProvider{key: masterKey})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var auth webui.AuthStore = NewWebUIAuthStore(st)
	set, err := auth.AdminPasswordIsSet()
	if err != nil {
		t.Fatal(err)
	}
	if set {
		t.Fatal("新 vault 不应已经设置管理员口令")
	}
	generation, err := auth.SetAdminPasswordWithAudit([]byte("correct-password"), webui.AuditEvent{
		Action: "admin.password_change", SourceIP: "192.0.2.10",
	})
	if err != nil || generation != 1 {
		t.Fatalf("设置管理员口令返回会话代 = %d，错误 = %v", generation, err)
	}
	valid, err := auth.VerifyAdminPassword([]byte("correct-password"))
	if err != nil || !valid {
		t.Fatalf("适配器应校验口令：valid=%v err=%v", valid, err)
	}
	now := time.Unix(1800000000, 0)
	for i := 0; i < 5; i++ {
		if _, err := auth.RecordLoginFailure("192.0.2.10", now.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	limit, err := auth.LoginLimit("192.0.2.10", now.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !limit.Locked || limit.Delay != 5*time.Second {
		t.Fatalf("适配器应返回锁定与延迟：%+v", limit)
	}
	if err := auth.WriteAudit(webui.AuditEvent{
		Action: "admin.login", Status: "ok", SourceIP: "192.0.2.10",
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.QueryAudit(store.AuditFilter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Source != store.AuditSourceWebUI ||
		rows[0].SourceIP != "192.0.2.10" || rows[0].Action != "admin.login" {
		t.Fatalf("适配器应写入 Web UI 审计：%+v", rows)
	}
}

func TestAuthorityHandlerUsesVaultAuthStore(t *testing.T) {
	masterKey, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSHMGR_VAULT_ROOT", t.TempDir())
	t.Setenv("SSHMGR_STORE", filepath.Join(t.TempDir(), "store.db"))
	st, err := OpenStore(&fakeKeyProvider{key: masterKey})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	auth := NewWebUIAuthStore(st)
	if _, err := auth.SetAdminPasswordWithAudit([]byte("vault-password"), webui.AuditEvent{
		Action: "admin.password_change",
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1800000000, 0)
	handler, _, err := webui.NewAuthority(webui.Config{
		Role: webui.RoleServer, Port: 7879, Auth: auth,
		Servers: fixedVaultUIRows{}, CertificateFingerprint: "sha256:0123456789abcdef",
		AllowedHosts: []string{"127.0.0.1"}, Now: func() time.Time { return now },
		Sleep: func(time.Duration) {},
	})
	if err != nil {
		t.Fatal(err)
	}

	pageRequest := httptest.NewRequest(http.MethodGet, "/ui/login", nil)
	pageRequest.Host = "127.0.0.1:7879"
	pageRecorder := httptest.NewRecorder()
	handler.ServeHTTP(pageRecorder, pageRequest)
	pageResponse := pageRecorder.Result()
	pageBody, _ := io.ReadAll(pageResponse.Body)
	pageResponse.Body.Close()
	match := regexp.MustCompile(`<input type="hidden" name="csrf_token" value="([^"]+)"`).FindStringSubmatch(string(pageBody))
	if match == nil {
		t.Fatalf("登录页缺少防跨站请求伪造令牌：\n%s", pageBody)
	}
	var csrfCookie *http.Cookie
	for _, cookie := range pageResponse.Cookies() {
		if cookie.Name == "__Host-sshmgr_csrf" {
			csrfCookie = cookie
		}
	}
	if csrfCookie == nil {
		t.Fatal("登录页缺少 __Host- 前缀防跨站请求伪造 cookie")
	}
	form := url.Values{"password": {"vault-password"}, "csrf_token": {match[1]}}
	loginRequest := httptest.NewRequest(http.MethodPost, "/ui/login", strings.NewReader(form.Encode()))
	loginRequest.Host = "127.0.0.1:7879"
	loginRequest.RemoteAddr = "198.51.100.9:45678"
	loginRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginRequest.Header.Set("Origin", "https://127.0.0.1:7879")
	loginRequest.AddCookie(csrfCookie)
	loginRecorder := httptest.NewRecorder()
	handler.ServeHTTP(loginRecorder, loginRequest)
	loginResponse := loginRecorder.Result()
	loginResponse.Body.Close()
	if loginResponse.StatusCode != http.StatusSeeOther {
		t.Fatalf("vault 口令登录状态码 = %d，应为 303", loginResponse.StatusCode)
	}
}

type fixedVaultUIRows struct{}

func (fixedVaultUIRows) Servers() ([]webui.ServerRow, error) {
	return []webui.ServerRow{{Name: "权威服务器"}}, nil
}
