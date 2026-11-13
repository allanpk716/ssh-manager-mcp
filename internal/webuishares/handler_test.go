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

func TestShareDetailShowsRegistrationFieldsWithoutSecretValue(t *testing.T) {
	st := newIntegrationStore(t)
	linuxID := mustServer(t, st, "linux-web", models.ShellPOSIX)
	winID := mustServer(t, st, "windows-web", models.ShellPowerShell)
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
	_ = linuxID
	_ = winID
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
