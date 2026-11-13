package webuishares

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/access"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/secretmanage"
	"ssh-manager-mcp/internal/servermanage"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
	"ssh-manager-mcp/internal/webui"
	"ssh-manager-mcp/internal/webuiaccess"
	"ssh-manager-mcp/internal/webuisecrets"
	"ssh-manager-mcp/internal/webuiservers"
	"ssh-manager-mcp/internal/webuitopology"
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

type clientServerSource struct {
	rows []webui.ServerRow
}

func (s clientServerSource) Servers(string) ([]webui.ServerRow, error) {
	return s.rows, nil
}

func (s clientServerSource) ServerList(string, bool) ([]webui.ServerListEntry, error) {
	return nil, nil
}

func (s clientServerSource) ServerDetail(string, string) (*webui.ServerDetailView, error) {
	return nil, nil
}

func TestClientSharesPageIsReadOnlyFromSnapshotAndWriteRoutesAreAbsent(t *testing.T) {
	st := newIntegrationStore(t)
	linuxID := mustServer(t, st, "linux-web", models.ShellPOSIX)
	dbID := mustServer(t, st, "db-1", models.ShellPOSIX)
	datasetsID := seedShare(t, st, "datasets", "NAS", "NAS_MOUNT_PASSWORD", "Z", "linux-web", "db-1")
	seedShare(t, st, "backup", "备份NAS", "BACKUP_PASSWORD", "Y", "db-1")
	manager := access.New(st)
	profileID, err := manager.AddProfile("team-a", access.CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.GrantServers("team-a", []string{"linux-web"}, access.CLISource()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := st.ExportSnapshotForProfile(profileID, true, "")
	if err != nil {
		t.Fatal(err)
	}
	handler, startup, err := webui.New(webui.Config{
		Role: webui.RoleClient,
		Servers: clientServerSource{
			rows: []webui.ServerRow{{ID: linuxID, Name: "linux-web"}, {ID: dbID, Name: "db-1"}},
		},
		Shares: FromClientSnapshot(func(string) (*store.Snapshot, error) {
			return snapshot, nil
		}),
		Topology: webuitopology.FromClientSnapshot(func(string) (*store.Snapshot, error) {
			return snapshot, nil
		}),
		Port: 18088,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, startup.URL("127.0.0.1:18088"), nil)
	req.Host = "127.0.0.1:18088"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("应签发客户端会话 cookie，实际 %d", len(cookies))
	}
	h := sessionHandler{handler: handler, cookie: *cookies[0], host: "127.0.0.1:18088"}

	page := body(t, h.do(http.MethodGet, "/ui/shares/", nil))
	for _, want := range []string{
		"共享存储", "datasets", "NAS", "Z", "版本号", "可挂载服务器数量", "1",
		"只显示本客户端可用的部分，修改请到权威端。",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("客户端共享存储列表缺少 %q：\n%s", want, page)
		}
	}
	if strings.Contains(page, "backup") || strings.Contains(page, "新增共享存储") {
		t.Fatalf("客户端列表不得显示范围外共享或写入口：\n%s", page)
	}

	detail := body(t, h.do(http.MethodGet, "/ui/shares/"+datasetsID, nil))
	for _, want := range []string{
		"datasets", "mount-user", "NAS_MOUNT_PASSWORD", "共享存储版本号",
		"linux-web", "/mnt/sshmgr/datasets",
	} {
		if !strings.Contains(detail, want) {
			t.Fatalf("客户端共享存储详情缺少 %q：\n%s", want, detail)
		}
	}
	if strings.Contains(detail, "db-1") || strings.Contains(detail, "编辑") || strings.Contains(detail, "删除") ||
		strings.Contains(detail, "桌面手动添加") ||
		strings.Contains(detail, "可挂载范围勾选清单") || strings.Contains(detail, "预览可挂载范围变更") ||
		strings.Contains(detail, "现场运维") || strings.Contains(detail, "现场探测") ||
		strings.Contains(detail, "单台卸载") ||
		strings.Contains(detail, "重选地址") || strings.Contains(detail, "残留检查") ||
		strings.Contains(detail, "NAS_MOUNT_PASSWORD-value-不回显") {
		t.Fatalf("客户端详情不得显示写入口或机密项值：\n%s", detail)
	}

	csrf := csrfToken(t, h)
	writeTargets := []string{
		"/ui/shares/new", "/ui/shares/" + datasetsID + "/edit", "/ui/shares/" + datasetsID + "/delete",
		"/ui/shares/" + datasetsID + "/check", "/ui/shares/" + datasetsID + "/mount",
		"/ui/shares/" + datasetsID + "/task/missing",
		"/ui/shares/" + datasetsID + "/unmount",
		"/ui/shares/" + datasetsID + "/reselect",
		"/ui/shares/" + datasetsID + "/residue",
		"/ui/shares/" + datasetsID + "/residue-cleanup",
		"/ui/secrets/authorization/preview", "/ui/secrets/authorization/confirm",
	}
	for _, target := range writeTargets {
		resp := h.do(http.MethodPost, target, url.Values{"csrf_token": {csrf}})
		respBody := body(t, resp)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("客户端写路由 %s 不应存在，状态码=%d：\n%s", target, resp.StatusCode, respBody)
		}
	}
	unmountPage := h.do(http.MethodGet, "/ui/shares/"+datasetsID+"/unmount?server="+linuxID, nil)
	unmountBody := body(t, unmountPage)
	if unmountPage.StatusCode != http.StatusNotFound || strings.Contains(unmountBody, "确认卸载") {
		t.Fatalf("客户端不应提供单台卸载页，状态码=%d：\n%s", unmountPage.StatusCode, unmountBody)
	}
	reselectPage := h.do(http.MethodGet, "/ui/shares/"+datasetsID+"/reselect?server="+linuxID, nil)
	reselectBody := body(t, reselectPage)
	if reselectPage.StatusCode != http.StatusNotFound || strings.Contains(reselectBody, "确认重选") {
		t.Fatalf("客户端不应提供重选地址页，状态码=%d：\n%s", reselectPage.StatusCode, reselectBody)
	}
	residuePage := h.do(http.MethodGet, "/ui/shares/"+datasetsID+"/residue", nil)
	residueBody := body(t, residuePage)
	if residuePage.StatusCode != http.StatusNotFound || strings.Contains(residueBody, "检查本共享残留") {
		t.Fatalf("客户端不应提供残留检查页，状态码=%d：\n%s", residuePage.StatusCode, residueBody)
	}
	guide := h.do(http.MethodGet, "/ui/shares/"+datasetsID+"/desktop", nil)
	guideBody := body(t, guide)
	if guide.StatusCode != http.StatusNotFound || strings.Contains(guideBody, "桌面手动添加") {
		t.Fatalf("客户端不应提供桌面手动添加引导，状态码=%d：\n%s", guide.StatusCode, guideBody)
	}
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

func newIntegrationHandler(t *testing.T, st *store.Store, remoteTasks ...webui.ShareRemoteTasks) sessionHandler {
	return startIntegrationHandler(t, st, func(config *webui.Config) {
		if len(remoteTasks) > 0 {
			config.SharesRemoteTasks = remoteTasks[0]
		}
	})
}

func startIntegrationHandler(
	t *testing.T, st *store.Store, configure func(*webui.Config),
) sessionHandler {
	t.Helper()
	config := webui.Config{
		Role: webui.RoleStandalone,
		Servers: func() webui.ServerSource {
			return webuiservers.FromManager(servermanage.New(st))
		}(),
		ServerWriter:  webuiservers.WriterFromManager(servermanage.New(st)),
		Access:        webuiaccess.FromManager(access.New(st)),
		Secrets:       webuisecrets.FromManager(secretmanage.New(st)),
		SecretsWriter: webuisecrets.WriterFromManager(secretmanage.New(st)),
		Shares:        FromStore(openStoreForTest(st)),
		SharesWriter:  WriterFromStore(openStoreForTest(st)),
		Topology:      webuitopology.FromStore(st),
		Port:          18086,
	}
	if configure != nil {
		configure(&config)
	}
	handler, startup, err := webui.New(config)
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

func TestShareDetailMountScopeChecklistPreviewsAndConfirmsAuthorization(t *testing.T) {
	st := newIntegrationStore(t)
	for _, device := range []string{"web 服务器", "windows 服务器"} {
		if _, err := st.AddDevice(device, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	linuxWebID := mustServerToDevice(t, st, "linux-web", models.ShellPOSIX, "web 服务器")
	linuxDBID := mustServerToDevice(t, st, "linux-db", models.ShellPOSIX, "web 服务器")
	windowsWebID := mustServerToDevice(t, st, "windows-web", models.ShellPowerShell, "windows 服务器")
	datasetsID := seedShare(t, st, "datasets", "NAS", "NAS_MOUNT_PASSWORD", "Z", "linux-web")
	views, err := st.ListSecretsInScope(store.ScopeAllServers())
	if err != nil || len(views) != 1 || views[0].Name != "NAS_MOUNT_PASSWORD" {
		t.Fatalf("准备挂载密码机密项失败：%v %+v", err, views)
	}
	secretID := views[0].ID
	h := newIntegrationHandler(t, st)

	detail := body(t, h.do(http.MethodGet, "/ui/shares/"+datasetsID, nil))
	for _, want := range []string{
		"可挂载范围勾选清单", "linux-web", "web 服务器", "posix",
		"linux-db", "windows-web", "powershell",
		"同设备的其他条目可能也能读到挂载内容", "预览可挂载范围变更",
	} {
		if !strings.Contains(detail, want) {
			t.Fatalf("共享存储详情的可挂载范围勾选清单缺少 %q：\n%s", want, detail)
		}
	}
	if !strings.Contains(detail, `name="cell__`+secretID+`__`+linuxWebID+`" value="1" checked`) ||
		strings.Contains(detail, `name="cell__`+secretID+`__`+linuxDBID+`" value="1" checked`) ||
		strings.Contains(detail, `name="cell__`+secretID+`__`+windowsWebID+`" value="1" checked`) {
		t.Fatalf("可挂载范围勾选清单的初始状态不正确：\n%s", detail)
	}

	preview := h.do(http.MethodPost, "/ui/secrets/authorization/preview", url.Values{
		"csrf_token":                            {csrfToken(t, h)},
		"return_to":                             {"/ui/shares/" + datasetsID},
		"secret_id":                             {secretID},
		"server_id":                             {linuxWebID, linuxDBID, windowsWebID},
		"cell__" + secretID + "__" + linuxWebID: {"0"},
		"cell__" + secretID + "__" + linuxDBID:  {"1"},
		"cell__" + secretID + "__" + windowsWebID: {"1"},
	})
	previewBody := body(t, preview)
	if preview.StatusCode != http.StatusOK {
		t.Fatalf("可挂载范围预览状态码=%d：\n%s", preview.StatusCode, previewBody)
	}
	for _, want := range []string{
		"机密项授权预览", "NAS_MOUNT_PASSWORD", "新增 2 个授权，移除 1 个授权。",
		"linux-db", "windows-web", "linux-web",
	} {
		if !strings.Contains(previewBody, want) {
			t.Fatalf("可挂载范围预览缺少 %q：\n%s", want, previewBody)
		}
	}
	summary := pageHiddenValue(t, previewBody, "summary")
	targets := pageHiddenValues(t, previewBody, "target")

	confirmed := h.do(http.MethodPost, "/ui/secrets/authorization/confirm", url.Values{
		"csrf_token": {csrfToken(t, h)},
		"summary":    {summary},
		"return_to":  {"/ui/shares/" + datasetsID},
		"target":     targets,
	})
	confirmedBody := body(t, confirmed)
	if confirmed.StatusCode != http.StatusOK || !strings.Contains(confirmedBody, "已保存授权变更") {
		t.Fatalf("确认可挂载范围变更失败，状态码=%d：\n%s", confirmed.StatusCode, confirmedBody)
	}

	after, err := st.ListSecretsInScope(store.ScopeAllServers())
	if err != nil || len(after) != 1 || strings.Join(after[0].AllowedServers, ",") != "linux-db,windows-web" {
		t.Fatalf("确认后的可挂载范围不正确：%v %+v", err, after)
	}
	share, err := st.GetSharedStorage("datasets")
	if err != nil || share.Version != 4 {
		t.Fatalf("授权变化应让共享存储版本号按既有触发器递增：%+v %v", share, err)
	}
	auditRows, err := st.QueryAudit(store.AuditFilter{
		OwnerOnly: true, Source: store.AuditSourceWebUI, Actions: []string{"secret-allow-change"},
	})
	if err != nil || len(auditRows) != 1 || auditRows[0].SourceIP != "192.0.2.1" {
		t.Fatalf("可挂载范围确认应写一条机密项授权审计：%v %+v", err, auditRows)
	}
}

func TestShareMountScopeEmptyDifferenceDoesNotWriteDataOrAudit(t *testing.T) {
	st := newIntegrationStore(t)
	linuxWebID := mustServer(t, st, "linux-web", models.ShellPOSIX)
	windowsWebID := mustServer(t, st, "windows-web", models.ShellPowerShell)
	datasetsID := seedShare(t, st, "datasets", "NAS", "NAS_MOUNT_PASSWORD", "Z", "linux-web")
	views, err := st.ListSecretsInScope(store.ScopeAllServers())
	if err != nil || len(views) != 1 {
		t.Fatalf("准备挂载密码机密项失败：%v %+v", err, views)
	}
	secretID := views[0].ID
	h := newIntegrationHandler(t, st)
	target := []string{
		secretID + " " + linuxWebID + " 1",
		secretID + " " + windowsWebID + " 0",
	}

	preview := h.do(http.MethodPost, "/ui/secrets/authorization/preview", url.Values{
		"csrf_token":                            {csrfToken(t, h)},
		"return_to":                             {"/ui/shares/" + datasetsID},
		"secret_id":                             {secretID},
		"server_id":                             {linuxWebID, windowsWebID},
		"cell__" + secretID + "__" + linuxWebID: {"1"},
		"cell__" + secretID + "__" + windowsWebID: {"0"},
	})
	previewBody := body(t, preview)
	if preview.StatusCode != http.StatusOK || !strings.Contains(previewBody, "没有变化") {
		t.Fatalf("空差异应显示没有变化，状态码=%d：\n%s", preview.StatusCode, previewBody)
	}
	summary := pageHiddenValue(t, previewBody, "summary")
	confirmed := h.do(http.MethodPost, "/ui/secrets/authorization/confirm", url.Values{
		"csrf_token": {csrfToken(t, h)}, "summary": {summary},
		"return_to": {"/ui/shares/" + datasetsID}, "target": target,
	})
	confirmedBody := body(t, confirmed)
	if confirmed.StatusCode != http.StatusOK || !strings.Contains(confirmedBody, "没有变化，未写入数据或审计。") {
		t.Fatalf("空差异确认不应写入，状态码=%d：\n%s", confirmed.StatusCode, confirmedBody)
	}
	after, err := st.ListSecretsInScope(store.ScopeAllServers())
	if err != nil || len(after) != 1 || strings.Join(after[0].AllowedServers, ",") != "linux-web" {
		t.Fatalf("空差异后的可挂载范围不正确：%v %+v", err, after)
	}
	if rows, err := st.QueryAudit(store.AuditFilter{
		OwnerOnly: true, Actions: []string{"secret-allow-change"},
	}); err != nil || len(rows) != 0 {
		t.Fatalf("空差异不应写审计：%v %+v", err, rows)
	}
}

func TestShareMountScopeStaleSummaryReturnsConflictWithoutWriting(t *testing.T) {
	st := newIntegrationStore(t)
	linuxWebID := mustServer(t, st, "linux-web", models.ShellPOSIX)
	windowsWebID := mustServer(t, st, "windows-web", models.ShellPowerShell)
	datasetsID := seedShare(t, st, "datasets", "NAS", "NAS_MOUNT_PASSWORD", "Z", "linux-web")
	views, err := st.ListSecretsInScope(store.ScopeAllServers())
	if err != nil || len(views) != 1 {
		t.Fatalf("准备挂载密码机密项失败：%v %+v", err, views)
	}
	secretID := views[0].ID
	h := newIntegrationHandler(t, st)
	target := []string{
		secretID + " " + linuxWebID + " 0",
		secretID + " " + windowsWebID + " 1",
	}
	preview := h.do(http.MethodPost, "/ui/secrets/authorization/preview", url.Values{
		"csrf_token":                            {csrfToken(t, h)},
		"return_to":                             {"/ui/shares/" + datasetsID},
		"secret_id":                             {secretID},
		"server_id":                             {linuxWebID, windowsWebID},
		"cell__" + secretID + "__" + linuxWebID: {"0"},
		"cell__" + secretID + "__" + windowsWebID: {"1"},
	})
	previewBody := body(t, preview)
	if preview.StatusCode != http.StatusOK {
		t.Fatalf("可挂载范围预览失败，状态码=%d：\n%s", preview.StatusCode, previewBody)
	}
	summary := pageHiddenValue(t, previewBody, "summary")

	if err := st.AllowSecrets([]string{"NAS_MOUNT_PASSWORD"}, []string{"windows-web"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	conflict := h.do(http.MethodPost, "/ui/secrets/authorization/confirm", url.Values{
		"csrf_token": {csrfToken(t, h)}, "summary": {summary},
		"return_to": {"/ui/shares/" + datasetsID}, "target": target,
	})
	conflictBody := body(t, conflict)
	if conflict.StatusCode != http.StatusConflict ||
		!strings.Contains(conflictBody, "授权状态已被其他入口修改") ||
		!strings.Contains(conflictBody, "移除 1 个授权") {
		t.Fatalf("过期摘要应返回冲突与最新预览，状态码=%d：\n%s", conflict.StatusCode, conflictBody)
	}
	after, err := st.ListSecretsInScope(store.ScopeAllServers())
	if err != nil || len(after) != 1 || strings.Join(after[0].AllowedServers, ",") != "linux-web,windows-web" {
		t.Fatalf("冲突确认不应改变可挂载范围：%v %+v", err, after)
	}
	if rows, err := st.QueryAudit(store.AuditFilter{
		OwnerOnly: true, Source: store.AuditSourceWebUI, Actions: []string{"secret-allow-change"},
	}); err != nil || len(rows) != 0 {
		t.Fatalf("冲突确认不应写 Web UI 审计：%v %+v", err, rows)
	}
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

func mustServerToDevice(t *testing.T, st *store.Store, name string, shell models.ShellType, device string) string {
	t.Helper()
	credential := &models.Credential{
		Type: models.CredPassword, Secret: []byte("server-password"),
	}
	added, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: name, Host: "192.0.2.10", Port: 22, User: "owner",
		AuthMethod: models.AuthPassword, ShellType: shell,
	}, credential, nil, device)
	if err != nil {
		t.Fatal(err)
	}
	return added.ServerID
}

func mustSecret(t *testing.T, st *store.Store, name string) {
	t.Helper()
	if err := st.AddSecret(name, "", []byte(name+"-value-不回显"), store.CLISource()); err != nil {
		t.Fatal(err)
	}
}

func seedShare(t *testing.T, st *store.Store, name, device, secret, drive string, servers ...string) string {
	t.Helper()
	if _, err := st.GetDevice(device); errors.Is(err, store.ErrDeviceNotFound) {
		if _, err := st.AddDevice(device, "", ""); err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
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

func TestShareDetailShowsRegistrationFieldsWithoutSecretValue(t *testing.T) {
	st := newIntegrationStore(t)
	mustServer(t, st, "linux-web", models.ShellPOSIX)
	mustServer(t, st, "windows-web", models.ShellPowerShell)
	if _, err := st.AddDevice("NAS", "", ""); err != nil {
		t.Fatal(err)
	}
	mustSecret(t, st, "NAS_MOUNT_PASSWORD")
	if err := st.AllowSecrets([]string{"NAS_MOUNT_PASSWORD"}, []string{"linux-web", "windows-web"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	share, err := st.AddSharedStorage(store.SharedStorageInput{
		Name: "datasets", Device: "NAS", User: "mount-user",
		PasswordSecret: "NAS_MOUNT_PASSWORD", Drive: "Z",
		Directories: []models.SharedStorageDirectory{
			{Purpose: "临时数据", Path: "tmp"},
			{Purpose: "长期数据", Path: "datasets"},
		},
		Note: "模型数据集",
	}, store.CLISource())
	if err != nil {
		t.Fatal(err)
	}
	h := newIntegrationHandler(t, st)

	page := h.do(http.MethodGet, "/ui/shares/"+share.ID, nil)
	pageBody := body(t, page)
	if page.StatusCode != http.StatusOK {
		t.Fatalf("共享存储详情状态码 = %d，页面：\n%s", page.StatusCode, pageBody)
	}
	for _, want := range []string{
		"datasets", "提供方设备", "NAS", "挂载账号用户名", "mount-user",
		"挂载密码机密项", "NAS_MOUNT_PASSWORD", "固定盘符", "Z:", "目录约定",
		"临时数据=tmp", "长期数据=datasets", "共享存储版本号", "1",
		"变更说明", "可挂载服务器", "linux-web", "/mnt/sshmgr/datasets",
		"windows-web", "Z:",
	} {
		if !strings.Contains(pageBody, want) {
			t.Fatalf("详情页缺少 %q：\n%s", want, pageBody)
		}
	}
	if strings.Contains(pageBody, "NAS_MOUNT_PASSWORD-value-不回显") ||
		strings.Contains(pageBody, "换值") || strings.Contains(pageBody, "读取值") {
		t.Fatalf("详情页不得显示机密项值或提供读回入口：\n%s", pageBody)
	}
}

// TestShareDesktopGuideShowsStepsWithoutPassword 走共享存储详情页的公开
// HTTP 接口验证桌面手动添加引导：推荐地址来自共同物理局域网，Windows 与
// macOS 步骤完整，页面没有任何共享密码入口。
func TestShareDesktopGuideShowsStepsWithoutPassword(t *testing.T) {
	st := newIntegrationStore(t)
	mustServer(t, st, "linux-web", models.ShellPOSIX)
	shareID := seedShare(t, st, "datasets", "NAS", "NAS_MOUNT_PASSWORD", "Z", "linux-web")
	passwordValue := `<密码>&'"不回显`
	if err := st.SetSecretValue("NAS_MOUNT_PASSWORD", []byte(passwordValue), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSite("家A", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{
		Name: "家A局域网", Type: models.NetworkPhysical, SiteName: "家A",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{
		Name: "虚拟网", Type: models.NetworkVirtual,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership("NAS", "家A局域网", "192.168.1.10"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership("NAS", "虚拟网", "100.64.0.10"); err != nil {
		t.Fatal(err)
	}
	desktop, err := st.AddDevice("我的电脑", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership("我的电脑", "家A局域网", "192.168.1.20"); err != nil {
		t.Fatal(err)
	}
	cloudDesktop, err := st.AddDevice("云电脑", "", "")
	if err != nil {
		t.Fatal(err)
	}
	h := newIntegrationHandler(t, st)

	detailBody := body(t, h.do(http.MethodGet, "/ui/shares/"+shareID, nil))
	if !strings.Contains(detailBody, `href="/ui/shares/`+shareID+`/desktop"`) ||
		!strings.Contains(detailBody, "桌面手动添加") {
		t.Fatalf("共享存储详情页应提供桌面手动添加入口：\n%s", detailBody)
	}

	page := h.do(http.MethodGet, "/ui/shares/"+shareID+"/desktop?device="+desktop.ID, nil)
	pageBody := body(t, page)
	if page.StatusCode != http.StatusOK {
		t.Fatalf("桌面手动添加引导状态码 = %d，页面：\n%s", page.StatusCode, pageBody)
	}
	for _, want := range []string{
		"桌面手动添加", "这台电脑是拓扑里哪台设备", "我的电脑", "这台电脑不在拓扑里",
		"推荐地址", "192.168.1.10", "家A局域网",
		"Windows", "Z:", `\\192.168.1.10\datasets`,
		`\\192.168.1.10\datasets\tmp`,
		`net use Z: \\192.168.1.10\datasets /user:mount-user * /persistent:yes`,
		"资源管理器", "映射网络驱动器", "macOS", `smb://192.168.1.10/datasets`, "/Volumes/datasets",
		"笔记本更换网络后，映射可能失效", "请输入你自己保存在密码管理器里的共享密码",
	} {
		if !strings.Contains(pageBody, want) {
			t.Fatalf("桌面手动添加引导缺少 %q：\n%s", want, pageBody)
		}
	}
	if strings.Contains(pageBody, "NAS_MOUNT_PASSWORD-value-不回显") ||
		strings.Contains(pageBody, passwordValue) ||
		strings.Contains(pageBody, "显示密码") || strings.Contains(pageBody, "复制密码") ||
		strings.Contains(pageBody, "下载密码") || strings.Contains(pageBody, "下载脚本") ||
		strings.Contains(pageBody, `type="password"`) {
		t.Fatalf("桌面手动添加引导不得显示、复制或下载共享密码：\n%s", pageBody)
	}

	notInTopology := body(t, h.do(http.MethodGet, "/ui/shares/"+shareID+"/desktop?device=none", nil))
	for _, want := range []string{
		"这台电脑不在拓扑里", "100.64.0.10", "虚拟网", "192.168.1.10", "家A局域网",
	} {
		if !strings.Contains(notInTopology, want) {
			t.Fatalf("电脑不在拓扑里时引导缺少 %q：\n%s", want, notInTopology)
		}
	}
	if strings.Contains(notInTopology, "<h2>推荐地址</h2>") {
		t.Fatalf("电脑不在拓扑里时不得显示推荐地址区块：\n%s", notInTopology)
	}
	if strings.Contains(notInTopology, passwordValue) {
		t.Fatalf("电脑不在拓扑里的引导不得显示共享密码：\n%s", notInTopology)
	}

	noCommon := body(t, h.do(
		http.MethodGet, "/ui/shares/"+shareID+"/desktop?device="+cloudDesktop.ID, nil,
	))
	if !strings.Contains(noCommon, "这台电脑与共享存储提供方设备没有共同网络，因此不给出推荐地址。") {
		t.Fatalf("没有共同网络时引导应说明原因：\n%s", noCommon)
	}
	if strings.Contains(noCommon, "<h2>推荐地址</h2>") {
		t.Fatalf("没有共同网络时不得显示推荐地址区块：\n%s", noCommon)
	}
	if strings.Contains(noCommon, passwordValue) {
		t.Fatalf("没有共同网络的引导不得显示共享密码：\n%s", noCommon)
	}
	rows, err := st.QueryAudit(store.AuditFilter{Source: store.AuditSourceWebUI})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("只读引导不应写 Web UI 审计：%+v", rows)
	}
}

func TestShareAddValidatesAndAuditsWebUISource(t *testing.T) {
	st := newIntegrationStore(t)
	mustServer(t, st, "linux-web", models.ShellPOSIX)
	seedShare(t, st, "datasets", "NAS", "NAS_MOUNT_PASSWORD", "Z", "linux-web")
	h := newIntegrationHandler(t, st)
	csrf := csrfToken(t, h)

	formPage := h.do(http.MethodGet, "/ui/shares/new", nil)
	formPageBody := body(t, formPage)
	if formPage.StatusCode != http.StatusOK {
		t.Fatalf("新增表单状态码 = %d，页面：\n%s", formPage.StatusCode, formPageBody)
	}
	for _, want := range []string{
		"共享名", "提供方设备", "挂载账号用户名", "挂载密码机密项", "固定盘符", "目录约定", "备注",
		"首版只支持 SMB", "禁止同一 IP 多个连接",
		`name="name"`, `name="device"`, `name="user"`, `name="password_secret"`,
		`name="drive"`, `name="directories"`, `name="note"`,
	} {
		if !strings.Contains(formPageBody, want) {
			t.Fatalf("新增表单缺少 %q：\n%s", want, formPageBody)
		}
	}

	added := h.do(http.MethodPost, "/ui/shares/new", url.Values{
		"csrf_token": {csrf}, "name": {"scratch"}, "device": {"NAS"},
		"user": {"mount-user"}, "password_secret": {"NAS_MOUNT_PASSWORD"},
		"drive": {"y"}, "directories": {"临时数据=tmp"}, "note": {"网页登记"},
	})
	addedBody := body(t, added)
	if added.StatusCode != http.StatusSeeOther {
		t.Fatalf("新增共享存储应跳转，状态码=%d：\n%s", added.StatusCode, addedBody)
	}
	share, err := st.GetSharedStorage("scratch")
	if err != nil || share.Drive != "Y" || share.Version != 1 ||
		len(share.Directories) != 1 || share.Directories[0].Purpose != "临时数据" {
		t.Fatalf("vault 最终状态不正确：%+v %v", share, err)
	}
	detail := h.do(http.MethodGet, added.Header.Get("Location"), nil)
	detailBody := body(t, detail)
	if detail.StatusCode != http.StatusOK || !strings.Contains(detailBody, "scratch") ||
		!strings.Contains(detailBody, "共享存储版本号") || !strings.Contains(detailBody, "1") {
		t.Fatalf("新增后应进入详情页，状态码=%d：\n%s", detail.StatusCode, detailBody)
	}
	rows, err := st.QueryAudit(store.AuditFilter{
		OwnerOnly: true, Source: store.AuditSourceWebUI, Actions: []string{"share-add"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Source != store.AuditSourceWebUI || rows[0].SourceIP != "192.0.2.1" {
		t.Fatalf("新增审计来源不正确：%+v", rows)
	}

	invalid := h.do(http.MethodPost, "/ui/shares/new", url.Values{
		"csrf_token": {csrf}, "name": {"DATASETS"}, "device": {"NAS"},
		"user": {"mount-user"}, "password_secret": {"NAS_MOUNT_PASSWORD"},
		"drive": {"D"}, "directories": {"缺等号的目录"},
	})
	invalidBody := body(t, invalid)
	if invalid.StatusCode != http.StatusBadRequest {
		t.Fatalf("校验失败状态码 = %d，页面：\n%s", invalid.StatusCode, invalidBody)
	}
	if !strings.Contains(invalidBody, "缺少等号，格式为 用途=子路径") {
		t.Fatalf("目录约定错误应原样展示存储层文案：\n%s", invalidBody)
	}
	for _, want := range []string{`value="DATASETS"`, `value="NAS"`, `value="mount-user"`, `value="NAS_MOUNT_PASSWORD"`, `value="D"`} {
		if !strings.Contains(invalidBody, want) {
			t.Fatalf("校验失败页面应保留输入 %q：\n%s", want, invalidBody)
		}
	}

	duplicate := h.do(http.MethodPost, "/ui/shares/new", url.Values{
		"csrf_token": {csrf}, "name": {"datasets"}, "device": {"NAS"},
		"user": {"mount-user"}, "password_secret": {"NAS_MOUNT_PASSWORD"},
		"drive": {"E"},
	})
	duplicateBody := body(t, duplicate)
	if duplicate.StatusCode != http.StatusBadRequest || !strings.Contains(duplicateBody, "共享名已存在") {
		t.Fatalf("共享名唯一校验应原样展示存储层文案，状态码=%d：\n%s", duplicate.StatusCode, duplicateBody)
	}

	badDrive := h.do(http.MethodPost, "/ui/shares/new", url.Values{
		"csrf_token": {csrf}, "name": {"scratch2"}, "device": {"NAS"},
		"user": {"mount-user"}, "password_secret": {"NAS_MOUNT_PASSWORD"},
		"drive": {"C"},
	})
	badDriveBody := body(t, badDrive)
	if badDrive.StatusCode != http.StatusBadRequest || !strings.Contains(badDriveBody, "盘符不合法") {
		t.Fatalf("盘符校验应原样展示存储层文案，状态码=%d：\n%s", badDrive.StatusCode, badDriveBody)
	}
}

func TestShareEditUsesPartialUpdateAndShowsNewVersion(t *testing.T) {
	st := newIntegrationStore(t)
	mustServer(t, st, "linux-web", models.ShellPOSIX)
	datasetsID := seedShare(t, st, "datasets", "NAS", "NAS_MOUNT_PASSWORD", "Z", "linux-web")
	seedShare(t, st, "backup", "备份NAS", "BACKUP_PASSWORD", "Y", "linux-web")
	h := newIntegrationHandler(t, st)
	csrf := csrfToken(t, h)

	formPage := h.do(http.MethodGet, "/ui/shares/"+datasetsID+"/edit", nil)
	formPageBody := body(t, formPage)
	if formPage.StatusCode != http.StatusOK {
		t.Fatalf("编辑表单状态码 = %d，页面：\n%s", formPage.StatusCode, formPageBody)
	}
	for _, want := range []string{
		`value="datasets"`, `value="NAS"`, `value="mount-user"`, `value="NAS_MOUNT_PASSWORD"`,
		`value="Z"`, "临时数据=tmp", `value="datasets 的备注"`, "变更说明",
	} {
		if !strings.Contains(formPageBody, want) {
			t.Fatalf("编辑表单应预填当前值 %q：\n%s", want, formPageBody)
		}
	}

	unchanged := h.do(http.MethodPost, "/ui/shares/"+datasetsID+"/edit", url.Values{
		"csrf_token": {csrf}, "name": {"datasets"}, "device": {"NAS"},
		"user": {"mount-user"}, "password_secret": {"NAS_MOUNT_PASSWORD"},
		"drive": {"Z"}, "directories": {"临时数据=tmp"}, "note": {"datasets 的备注"},
		"change_note": {""},
	})
	unchangedBody := body(t, unchanged)
	if unchanged.StatusCode != http.StatusOK ||
		!strings.Contains(unchangedBody, "没有变化，未写入数据或审计") {
		t.Fatalf("未变化的提交不应写数据或审计，状态码=%d：\n%s", unchanged.StatusCode, unchangedBody)
	}
	share, err := st.GetSharedStorage("datasets")
	if err != nil || share.Version != 1 {
		t.Fatalf("未变化提交不应推进版本号：%+v %v", share, err)
	}

	edited := h.do(http.MethodPost, "/ui/shares/"+datasetsID+"/edit", url.Values{
		"csrf_token": {csrf}, "name": {"datasets"}, "device": {"NAS"},
		"user": {"new-mount-user"}, "password_secret": {"NAS_MOUNT_PASSWORD"},
		"drive": {"Z"}, "directories": {"临时数据=tmp\n长期数据=datasets"}, "note": {"datasets 的备注"},
		"change_note": {"更新挂载账号"},
	})
	if edited.StatusCode != http.StatusSeeOther ||
		edited.Header.Get("Location") != "/ui/shares/"+datasetsID+"?saved=1" {
		t.Fatalf("编辑应跳回详情页，状态码=%d 跳转=%q：\n%s",
			edited.StatusCode, edited.Header.Get("Location"), body(t, edited))
	}
	detail := h.do(http.MethodGet, edited.Header.Get("Location"), nil)
	detailBody := body(t, detail)
	if detail.StatusCode != http.StatusOK || !strings.Contains(detailBody, "已保存") ||
		!strings.Contains(detailBody, "共享存储版本号") || !strings.Contains(detailBody, "2") ||
		!strings.Contains(detailBody, "new-mount-user") || !strings.Contains(detailBody, "长期数据=datasets") ||
		!strings.Contains(detailBody, "更新挂载账号") {
		t.Fatalf("保存后详情页应显示新版本号与变更，状态码=%d：\n%s", detail.StatusCode, detailBody)
	}
	share, err = st.GetSharedStorage("datasets")
	if err != nil || share.Version != 2 || share.MountUser != "new-mount-user" ||
		share.ChangeNote != "更新挂载账号" || len(share.Directories) != 2 {
		t.Fatalf("编辑后的 vault 最终状态不正确：%+v %v", share, err)
	}
	rows, err := st.QueryAudit(store.AuditFilter{
		OwnerOnly: true, Source: store.AuditSourceWebUI, Actions: []string{"share-edit"},
	})
	if err != nil || len(rows) != 1 || rows[0].SourceIP != "192.0.2.1" {
		t.Fatalf("编辑审计不正确：%+v %v", rows, err)
	}

	conflictDrive := h.do(http.MethodPost, "/ui/shares/"+datasetsID+"/edit", url.Values{
		"csrf_token": {csrf}, "name": {"datasets"}, "device": {"NAS"},
		"user": {"new-mount-user"}, "password_secret": {"NAS_MOUNT_PASSWORD"},
		"drive": {"Y"}, "directories": {"临时数据=tmp"}, "note": {"datasets 的备注"},
	})
	conflictBody := body(t, conflictDrive)
	if conflictDrive.StatusCode != http.StatusBadRequest ||
		!strings.Contains(conflictBody, "盘符已存在") || !strings.Contains(conflictBody, "backup") {
		t.Fatalf("盘符唯一校验应原样展示存储层文案，状态码=%d：\n%s", conflictDrive.StatusCode, conflictBody)
	}
}

func TestShareDeleteShowsPreviewAndConfirms(t *testing.T) {
	st := newIntegrationStore(t)
	mustServer(t, st, "linux-web", models.ShellPOSIX)
	mustServer(t, st, "windows-web", models.ShellPowerShell)
	datasetsID := seedShare(t, st, "datasets", "NAS", "NAS_MOUNT_PASSWORD", "Z",
		"linux-web", "windows-web")
	h := newIntegrationHandler(t, st)
	csrf := csrfToken(t, h)

	preview := h.do(http.MethodGet, "/ui/shares/"+datasetsID+"/delete", nil)
	previewBody := body(t, preview)
	if preview.StatusCode != http.StatusOK {
		t.Fatalf("删除预览状态码 = %d，页面：\n%s", preview.StatusCode, previewBody)
	}
	for _, want := range []string{
		"删除 datasets", "可挂载服务器", "linux-web", "windows-web",
		"只删除登记，不会去服务器上卸载", "检查残留", "单台卸载", "确认删除",
	} {
		if !strings.Contains(previewBody, want) {
			t.Fatalf("删除预览缺少 %q：\n%s", want, previewBody)
		}
	}

	deleted := h.do(http.MethodPost, "/ui/shares/"+datasetsID+"/delete", url.Values{
		"csrf_token": {csrf},
	})
	deletedBody := body(t, deleted)
	if deleted.StatusCode != http.StatusSeeOther || deleted.Header.Get("Location") != "/ui/shares/" {
		t.Fatalf("删除应跳回列表，状态码=%d 跳转=%q：\n%s",
			deleted.StatusCode, deleted.Header.Get("Location"), deletedBody)
	}
	if _, err := st.GetSharedStorage("datasets"); err == nil {
		t.Fatal("删除后共享存储不应仍存在")
	}
	rows, err := st.QueryAudit(store.AuditFilter{
		OwnerOnly: true, Source: store.AuditSourceWebUI, Actions: []string{"share-rm"},
	})
	if err != nil || len(rows) != 1 || rows[0].SourceIP != "192.0.2.1" {
		t.Fatalf("删除审计不正确：%+v %v", rows, err)
	}
}

type authorityHandler struct {
	handler http.Handler
	cookie  *http.Cookie
}

func newShareAuthorityHandler(t *testing.T, st *store.Store, now *time.Time) authorityHandler {
	t.Helper()
	auth := vault.NewWebUIAuthStore(st)
	if _, err := auth.SetAdminPasswordWithAudit([]byte("correct-password"), webui.AuditEvent{
		Action: "admin.password_change", Status: "ok",
	}); err != nil {
		t.Fatal(err)
	}
	handler, _, err := webui.NewAuthority(webui.Config{
		Role: webui.RoleServer, Port: 18087,
		Servers:                webuiservers.FromManager(servermanage.New(st)),
		ServerWriter:           webuiservers.WriterFromManager(servermanage.New(st)),
		Access:                 webuiaccess.FromManager(access.New(st)),
		Secrets:                webuisecrets.FromManager(secretmanage.New(st)),
		SecretsWriter:          webuisecrets.WriterFromManager(secretmanage.New(st)),
		Shares:                 FromStore(openStoreForTest(st)),
		SharesWriter:           WriterFromStore(openStoreForTest(st)),
		Topology:               webuitopology.FromStore(st),
		Auth:                   auth,
		AllowedHosts:           []string{"127.0.0.1"},
		CertificateFingerprint: "sha256:0123456789abcdef",
		Now:                    func() time.Time { return *now },
	})
	if err != nil {
		t.Fatal(err)
	}
	loginPage := authorityGet(handler, "/ui/login")
	loginBody := body(t, loginPage)
	csrf := authorityHiddenValue(t, loginBody, "csrf_token")
	form := url.Values{"csrf_token": {csrf}, "password": {"correct-password"}}
	req := httptest.NewRequest(http.MethodPost, "/ui/login", strings.NewReader(form.Encode()))
	req.Host = "127.0.0.1:18087"
	req.RemoteAddr = "198.51.100.9:45678"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://127.0.0.1:18087")
	for _, cookie := range loginPage.Cookies() {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	loggedIn := rec.Result()
	_ = loggedIn.Body.Close()
	if loggedIn.StatusCode != http.StatusSeeOther {
		t.Fatalf("权威端登录失败，状态码=%d", loggedIn.StatusCode)
	}
	for _, cookie := range loggedIn.Cookies() {
		if cookie.Name == "__Host-sshmgr_ui" {
			return authorityHandler{handler: handler, cookie: cookie}
		}
	}
	t.Fatal("权威端登录响应缺少会话 cookie")
	return authorityHandler{}
}

func authorityGet(handler http.Handler, target string) *http.Response {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = "127.0.0.1:18087"
	req.RemoteAddr = "198.51.100.9:45678"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Result()
}

func (h authorityHandler) do(method, target string, form url.Values) *http.Response {
	reqBody := strings.NewReader("")
	if form != nil {
		reqBody = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, target, reqBody)
	req.Host = "127.0.0.1:18087"
	req.RemoteAddr = "198.51.100.9:45678"
	req.AddCookie(h.cookie)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "https://127.0.0.1:18087")
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec.Result()
}

func authorityHiddenValue(t *testing.T, page, name string) string {
	t.Helper()
	pattern := regexp.MustCompile(`<input type="hidden" name="` + regexp.QuoteMeta(name) + `" value="([^"]*)"`)
	match := pattern.FindStringSubmatch(page)
	if match == nil {
		t.Fatalf("页面缺少 %q 隐藏值：\n%s", name, page)
	}
	return match[1]
}

func pageHiddenValue(t *testing.T, page, name string) string {
	t.Helper()
	values := pageHiddenValues(t, page, name)
	if len(values) == 0 {
		t.Fatalf("页面缺少 %q 隐藏值：\n%s", name, page)
	}
	return values[0]
}

func pageHiddenValues(t *testing.T, page, name string) []string {
	t.Helper()
	pattern := regexp.MustCompile(`<input type="hidden" name="` + regexp.QuoteMeta(name) + `" value="([^"]*)"`)
	matches := pattern.FindAllStringSubmatch(page, -1)
	values := make([]string, 0, len(matches))
	for _, match := range matches {
		values = append(values, match[1])
	}
	return values
}

func TestAuthorityShareDeleteRequiresRecentReverify(t *testing.T) {
	st := newIntegrationStore(t)
	mustServer(t, st, "linux-web", models.ShellPOSIX)
	datasetsID := seedShare(t, st, "datasets", "NAS", "NAS_MOUNT_PASSWORD", "Z", "linux-web")
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.Local)
	h := newShareAuthorityHandler(t, st, &now)

	preview := h.do(http.MethodGet, "/ui/shares/"+datasetsID+"/delete", nil)
	previewBody := body(t, preview)
	if preview.StatusCode != http.StatusOK || !strings.Contains(previewBody, "只删除登记，不会去服务器上卸载") {
		t.Fatalf("权威端删除预览状态码=%d：\n%s", preview.StatusCode, previewBody)
	}
	detail := h.do(http.MethodGet, "/ui/shares/"+datasetsID, nil)
	detailBody := body(t, detail)
	if detail.StatusCode != http.StatusOK || !strings.Contains(detailBody, "打开桌面手动添加引导") {
		t.Fatalf("权威端共享存储详情应提供桌面手动添加入口，状态码=%d：\n%s", detail.StatusCode, detailBody)
	}
	guide := h.do(http.MethodGet, "/ui/shares/"+datasetsID+"/desktop?device=none", nil)
	guideBody := body(t, guide)
	if guide.StatusCode != http.StatusOK || !strings.Contains(guideBody, "桌面手动添加 datasets") {
		t.Fatalf("权威端应提供桌面手动添加引导，状态码=%d：\n%s", guide.StatusCode, guideBody)
	}

	now = now.Add(6 * time.Minute)
	confirm := h.do(http.MethodPost, "/ui/shares/"+datasetsID+"/delete", url.Values{
		"csrf_token": {authorityPageCSRF(t, h, "/ui/shares/"+datasetsID+"/delete")},
	})
	confirmBody := body(t, confirm)
	if confirm.StatusCode != http.StatusSeeOther ||
		confirm.Header.Get("Location") != "/ui/reverify?next="+url.QueryEscape("/ui/shares/"+datasetsID+"/delete") {
		t.Fatalf("未重新验证的删除应跳转重新验证，状态码=%d 跳转=%q：\n%s",
			confirm.StatusCode, confirm.Header.Get("Location"), confirmBody)
	}
	if _, err := st.GetSharedStorage("datasets"); err != nil {
		t.Fatalf("未重新验证前不应删除共享存储：%v", err)
	}

	reverify := h.do(http.MethodPost, "/ui/reverify", url.Values{
		"csrf_token": {authorityPageCSRF(t, h, "/ui/reverify?next="+url.QueryEscape("/ui/shares/"+datasetsID+"/delete"))},
		"password":   {"correct-password"},
		"next":       {"/ui/shares/" + datasetsID + "/delete"},
	})
	if reverify.StatusCode != http.StatusSeeOther {
		t.Fatalf("重新验证失败，状态码=%d：\n%s", reverify.StatusCode, body(t, reverify))
	}
	now = now.Add(2 * time.Minute)
	deleted := h.do(http.MethodPost, "/ui/shares/"+datasetsID+"/delete", url.Values{
		"csrf_token": {authorityPageCSRF(t, h, "/ui/shares/"+datasetsID+"/delete")},
	})
	deletedBody := body(t, deleted)
	if deleted.StatusCode != http.StatusSeeOther || deleted.Header.Get("Location") != "/ui/shares/" {
		t.Fatalf("重新验证后的删除应成功，状态码=%d：\n%s", deleted.StatusCode, deletedBody)
	}
	if _, err := st.GetSharedStorage("datasets"); err == nil {
		t.Fatal("重新验证后应删除共享存储")
	}
	rows, err := st.QueryAudit(store.AuditFilter{
		OwnerOnly: true, Source: store.AuditSourceWebUI, Actions: []string{"share-rm"},
	})
	if err != nil || len(rows) != 1 || rows[0].SourceIP != "198.51.100.9" {
		t.Fatalf("权威端删除审计不正确：%+v %v", rows, err)
	}
}

func TestAuthorityShareMountScopeConfirmRequiresReverifyAndPreservesTarget(t *testing.T) {
	st := newIntegrationStore(t)
	linuxWebID := mustServer(t, st, "linux-web", models.ShellPOSIX)
	datasetsID := seedShare(t, st, "datasets", "NAS", "NAS_MOUNT_PASSWORD", "Z", "linux-web")
	views, err := st.ListSecretsInScope(store.ScopeAllServers())
	if err != nil || len(views) != 1 {
		t.Fatalf("准备挂载密码机密项失败：%v %+v", err, views)
	}
	secretID := views[0].ID
	now := time.Date(2026, 11, 13, 11, 0, 0, 0, time.Local)
	h := newShareAuthorityHandler(t, st, &now)
	now = now.Add(6 * time.Minute)
	csrf := authorityPageCSRF(t, h, "/ui/shares/"+datasetsID)

	preview := h.do(http.MethodPost, "/ui/secrets/authorization/preview", url.Values{
		"csrf_token":                            {csrf},
		"return_to":                             {"/ui/shares/" + datasetsID},
		"secret_id":                             {secretID},
		"server_id":                             {linuxWebID},
		"cell__" + secretID + "__" + linuxWebID: {"0"},
	})
	previewBody := body(t, preview)
	if preview.StatusCode != http.StatusOK {
		t.Fatalf("权威端可挂载范围预览失败，状态码=%d：\n%s", preview.StatusCode, previewBody)
	}
	summary := authorityHiddenValue(t, previewBody, "summary")
	target := secretID + " " + linuxWebID + " 0"
	confirmRejected := h.do(http.MethodPost, "/ui/secrets/authorization/confirm", url.Values{
		"csrf_token": {csrf}, "summary": {summary},
		"return_to": {"/ui/shares/" + datasetsID}, "target": {target},
	})
	confirmBody := body(t, confirmRejected)
	resume := "/ui/secrets/authorization/preview?return_to=%2Fui%2Fshares%2F" + datasetsID +
		"&target=" + url.QueryEscape(target)
	if confirmRejected.StatusCode != http.StatusSeeOther ||
		confirmRejected.Header.Get("Location") != "/ui/reverify?next="+url.QueryEscape(resume) {
		t.Fatalf("权威端确认可挂载范围应先重新验证，状态码=%d 跳转=%q：\n%s",
			confirmRejected.StatusCode, confirmRejected.Header.Get("Location"), confirmBody)
	}

	reverify := h.do(http.MethodPost, "/ui/reverify", url.Values{
		"csrf_token": {authorityPageCSRF(t, h, "/ui/reverify?next="+url.QueryEscape(resume))},
		"password":   {"correct-password"}, "next": {resume},
	})
	if reverify.StatusCode != http.StatusSeeOther || reverify.Header.Get("Location") != resume {
		t.Fatalf("重新验证后应回到保留勾选集合的预览，状态码=%d 跳转=%q：\n%s",
			reverify.StatusCode, reverify.Header.Get("Location"), body(t, reverify))
	}
	resumedPreview := body(t, h.do(http.MethodGet, resume, nil))
	if authorityHiddenValue(t, resumedPreview, "summary") != summary ||
		len(pageHiddenValues(t, resumedPreview, "target")) != 1 ||
		pageHiddenValues(t, resumedPreview, "target")[0] != target {
		t.Fatalf("重新验证后的预览应保留目标授权集合：\n%s", resumedPreview)
	}

	confirmed := h.do(http.MethodPost, "/ui/secrets/authorization/confirm", url.Values{
		"csrf_token": {csrf}, "summary": {summary},
		"return_to": {"/ui/shares/" + datasetsID}, "target": {target},
	})
	confirmedBody := body(t, confirmed)
	if confirmed.StatusCode != http.StatusOK || !strings.Contains(confirmedBody, "已保存授权变更") {
		t.Fatalf("重新验证后应能确认可挂载范围变更，状态码=%d：\n%s", confirmed.StatusCode, confirmedBody)
	}
	after, err := st.ListSecretsInScope(store.ScopeAllServers())
	if err != nil || len(after) != 1 || len(after[0].AllowedServers) != 0 {
		t.Fatalf("权威端确认后的可挂载范围不正确：%v %+v", err, after)
	}
}

func authorityPageCSRF(t *testing.T, h authorityHandler, target string) string {
	t.Helper()
	page := body(t, h.do(http.MethodGet, target, nil))
	return authorityHiddenValue(t, page, "csrf_token")
}
