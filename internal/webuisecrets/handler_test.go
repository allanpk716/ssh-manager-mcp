package webuisecrets

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/access"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/secretmanage"
	"ssh-manager-mcp/internal/servermanage"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
	"ssh-manager-mcp/internal/webuiaccess"
	"ssh-manager-mcp/internal/webuiservers"
)

func newIntegrationStore(t *testing.T) *store.Store {
	t.Helper()
	key, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func newIntegrationHandler(t *testing.T, st *store.Store) sessionHandler {
	t.Helper()
	handler, startup, err := webui.New(webui.Config{
		Role:          webui.RoleStandalone,
		Servers:       webuiservers.FromManager(servermanage.New(st)),
		ServerWriter:  webuiservers.WriterFromManager(servermanage.New(st)),
		Access:        webuiaccess.FromManager(access.New(st)),
		Secrets:       FromManager(secretmanage.New(st)),
		SecretsWriter: WriterFromManager(secretmanage.New(st)),
		Port:          18082,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, startup.URL("127.0.0.1:18082"), nil)
	req.Host = "127.0.0.1:18082"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("应签发会话 cookie，实际 %d", len(cookies))
	}
	return sessionHandler{handler: handler, cookie: *cookies[0]}
}

type sessionHandler struct {
	handler http.Handler
	cookie  http.Cookie
}

func (s sessionHandler) do(method, target string, form url.Values) *http.Response {
	body := strings.NewReader("")
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, target, body)
	req.Host = "127.0.0.1:18082"
	req.AddCookie(&s.cookie)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "http://127.0.0.1:18082")
	}
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	return rec.Result()
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return string(data)
}

func mustServer(t *testing.T, st *store.Store, name string) string {
	t.Helper()
	credentialID, err := st.SetCredential(&models.Credential{
		Type: models.CredPassword, Secret: []byte("server-password"),
	})
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.AddServer(&models.Server{
		Name: name, Host: "192.0.2.10", Port: 22, User: "owner",
		AuthMethod: models.AuthPassword, CredentialID: credentialID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func csrfToken(t *testing.T, h sessionHandler) string {
	t.Helper()
	page := body(t, h.do(http.MethodGet, "/ui/secrets/", nil))
	start := strings.Index(page, `name="csrf_token" content="`)
	if start < 0 {
		t.Fatalf("页面缺少防跨站请求伪造令牌：\n%s", page)
	}
	start += len(`name="csrf_token" content="`)
	end := strings.Index(page[start:], `"`)
	if end < 0 {
		t.Fatalf("页面令牌格式不正确：\n%s", page)
	}
	return page[start : start+end]
}

func TestSecretsPageListsFiltersAndNeverShowsValue(t *testing.T) {
	st := newIntegrationStore(t)
	mustServer(t, st, "web-1")
	mustServer(t, st, "db-1")
	manager := secretmanage.New(st)
	if err := manager.AddSecret("DEPLOY_TOKEN", "部署网站", "DEPLOY_TOKEN_FILE", []byte("value-must-not-appear"), secretmanage.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := manager.AllowSecrets([]string{"DEPLOY_TOKEN"}, []string{"web-1"}, secretmanage.CLISource()); err != nil {
		t.Fatal(err)
	}
	h := newIntegrationHandler(t, st)

	page := h.do(http.MethodGet, "/ui/secrets/", nil)
	pageBody := body(t, page)
	if page.StatusCode != http.StatusOK {
		t.Fatalf("机密项列表状态码 = %d，页面：\n%s", page.StatusCode, pageBody)
	}
	for _, want := range []string{"机密项", "DEPLOY_TOKEN", "部署网站", "DEPLOY_TOKEN_FILE", "web-1", "创建时间", "值最后更改"} {
		if !strings.Contains(pageBody, want) {
			t.Fatalf("列表缺少 %q：\n%s", want, pageBody)
		}
	}
	if strings.Contains(pageBody, "value-must-not-appear") {
		t.Fatalf("列表不得显示机密项值：\n%s", pageBody)
	}

	filtered := h.do(http.MethodGet, "/ui/secrets/?query=数据库&server=web-1", nil)
	filteredBody := body(t, filtered)
	if filtered.StatusCode != http.StatusOK || strings.Contains(filteredBody, "DEPLOY_TOKEN") {
		t.Fatalf("搜索与服务器筛选应过滤列表，状态码=%d 页面：\n%s", filtered.StatusCode, filteredBody)
	}
	if !strings.Contains(pageBody, `<a href="/ui/secrets/`) {
		t.Fatalf("列表应提供机密项详情链接：\n%s", pageBody)
	}
}
