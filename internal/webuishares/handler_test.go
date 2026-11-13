package webuishares

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/access"
	"ssh-manager-mcp/internal/models"
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
	st, err := store.Open(t.TempDir()+"/store.db", key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

type sessionHandler struct {
	handler http.Handler
	cookie  http.Cookie
	host    string
}

func (s sessionHandler) do(method, target string, form url.Values) *http.Response {
	body := strings.NewReader("")
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, target, body)
	req.Host = s.host
	req.RemoteAddr = "192.0.2.1:45678"
	req.AddCookie(&s.cookie)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "http://"+s.host)
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

func openStoreForTest(st *store.Store) func() (*store.Store, func(), error) {
	return func() (*store.Store, func(), error) {
		return st, func() {}, nil
	}
}

func newIntegrationHandler(t *testing.T, st *store.Store) sessionHandler {
	t.Helper()
	handler, startup, err := webui.New(webui.Config{
		Role: webui.RoleStandalone,
		Servers: func() webui.ServerSource {
			return webuiservers.FromManager(servermanage.New(st))
		}(),
		ServerWriter: webuiservers.WriterFromManager(servermanage.New(st)),
		Access:       webuiaccess.FromManager(access.New(st)),
		Shares:       FromStore(openStoreForTest(st)),
		SharesWriter: WriterFromStore(openStoreForTest(st)),
		Port:         18086,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, startup.URL("127.0.0.1:18086"), nil)
	req.Host = "127.0.0.1:18086"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("应签发会话 cookie，实际 %d", len(cookies))
	}
	return sessionHandler{handler: handler, cookie: *cookies[0], host: "127.0.0.1:18086"}
}

func csrfToken(t *testing.T, h sessionHandler) string {
	t.Helper()
	page := body(t, h.do(http.MethodGet, "/ui/shares/", nil))
	start := strings.Index(page, `name="csrf-token" content="`)
	if start < 0 {
		t.Fatalf("页面缺少防跨站请求伪造令牌：\n%s", page)
	}
	start += len(`name="csrf-token" content="`)
	end := strings.Index(page[start:], `"`)
	if end < 0 {
		t.Fatalf("页面令牌格式不正确：\n%s", page)
	}
	return page[start : start+end]
}

func mustServer(t *testing.T, st *store.Store, name string, shell models.ShellType) string {
	t.Helper()
	credentialID, err := st.SetCredential(&models.Credential{
		Type: models.CredPassword, Secret: []byte("server-password"),
	})
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.AddServer(&models.Server{
		Name: name, Host: "192.0.2.10", Port: 22, User: "owner",
		AuthMethod: models.AuthPassword, CredentialID: credentialID, ShellType: shell,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustSecret(t *testing.T, st *store.Store, name string) {
	t.Helper()
	if err := st.AddSecret(name, "", []byte(name+"-value-不回显"), store.CLISource()); err != nil {
		t.Fatal(err)
	}
}

func seedShare(t *testing.T, st *store.Store, name, device, secret, drive string, servers ...string) string {
	t.Helper()
	if _, err := st.AddDevice(device, "", ""); err != nil {
		t.Fatal(err)
	}
	mustSecret(t, st, secret)
	if len(servers) > 0 {
		if err := st.AllowSecrets([]string{secret}, servers, store.CLISource()); err != nil {
			t.Fatal(err)
		}
	}
	share, err := st.AddSharedStorage(store.SharedStorageInput{
		Name: name, Device: device, User: "mount-user", PasswordSecret: secret,
		Drive:       drive,
		Directories: []models.SharedStorageDirectory{{Purpose: "临时数据", Path: "tmp"}},
		Note:        name + " 的备注",
	}, store.CLISource())
	if err != nil {
		t.Fatal(err)
	}
	return share.ID
}

func TestSharesPageListsRowsAndFilters(t *testing.T) {
	st := newIntegrationStore(t)
	mustServer(t, st, "linux-web", models.ShellPOSIX)
	mustServer(t, st, "db-1", models.ShellPOSIX)
	datasetsID := seedShare(t, st, "datasets", "NAS", "NAS_MOUNT_PASSWORD", "Z", "linux-web")
	seedShare(t, st, "backup", "备份NAS", "BACKUP_PASSWORD", "Y", "db-1")
	h := newIntegrationHandler(t, st)

	page := h.do(http.MethodGet, "/ui/shares/", nil)
	pageBody := body(t, page)
	if page.StatusCode != http.StatusOK {
		t.Fatalf("共享存储列表状态码 = %d，页面：\n%s", page.StatusCode, pageBody)
	}
	for _, want := range []string{
		"共享存储", "datasets", "NAS", "Z", "备份NAS", "backup",
		"版本号", "可挂载服务器数量", "新增共享存储",
		`href="/ui/shares/` + datasetsID + `"`,
	} {
		if !strings.Contains(pageBody, want) {
			t.Fatalf("列表缺少 %q：\n%s", want, pageBody)
		}
	}
	if strings.Contains(pageBody, "NAS_MOUNT_PASSWORD-value-不回显") {
		t.Fatalf("列表不得显示挂载密码机密项的值：\n%s", pageBody)
	}

	byDevice := h.do(http.MethodGet, "/ui/shares/?device=备份NAS", nil)
	byDeviceBody := body(t, byDevice)
	if byDevice.StatusCode != http.StatusOK || strings.Contains(byDeviceBody, "datasets") ||
		!strings.Contains(byDeviceBody, "backup") {
		t.Fatalf("提供方设备筛选结果不正确，状态码=%d：\n%s", byDevice.StatusCode, byDeviceBody)
	}

	byServer := h.do(http.MethodGet, "/ui/shares/?server=linux-web", nil)
	byServerBody := body(t, byServer)
	if byServer.StatusCode != http.StatusOK || strings.Contains(byServerBody, "backup") ||
		!strings.Contains(byServerBody, "datasets") {
		t.Fatalf("服务器条目筛选结果不正确，状态码=%d：\n%s", byServer.StatusCode, byServerBody)
	}
}
