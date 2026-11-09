package webuisecrets

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
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
	return sessionHandler{handler: handler, cookie: *cookies[0], host: "127.0.0.1:18082"}
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

func postMultipart(h sessionHandler, target string, fields map[string]string, fileField, fileName string, fileSize int) *http.Response {
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	for name, value := range fields {
		_ = writer.WriteField(name, value)
	}
	if fileField != "" {
		part, err := writer.CreateFormFile(fileField, fileName)
		if err != nil {
			panic(err)
		}
		_, _ = part.Write(bytes.Repeat([]byte("x"), fileSize))
	}
	_ = writer.Close()
	req := httptest.NewRequest(http.MethodPost, target, &buffer)
	req.Host = h.host
	req.RemoteAddr = "192.0.2.1:45678"
	req.AddCookie(&h.cookie)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Origin", "http://"+h.host)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec.Result()
}

type authorityHandler struct {
	handler http.Handler
	cookie  *http.Cookie
}

func newAuthorityHandler(t *testing.T, st *store.Store, now *time.Time) authorityHandler {
	t.Helper()
	auth := vault.NewWebUIAuthStore(st)
	if _, err := auth.SetAdminPasswordWithAudit([]byte("correct-password"), webui.AuditEvent{
		Action: "admin.password_change", Status: "ok",
	}); err != nil {
		t.Fatal(err)
	}
	handler, _, err := webui.NewAuthority(webui.Config{
		Role: webui.RoleServer, Port: 18083,
		Servers:                webuiservers.FromManager(servermanage.New(st)),
		ServerWriter:           webuiservers.WriterFromManager(servermanage.New(st)),
		Access:                 webuiaccess.FromManager(access.New(st)),
		Secrets:                FromManager(secretmanage.New(st)),
		SecretsWriter:          WriterFromManager(secretmanage.New(st)),
		Auth:                   auth,
		AllowedHosts:           []string{"127.0.0.1"},
		CertificateFingerprint: "sha256:0123456789abcdef",
		Now:                    func() time.Time { return *now },
	})
	if err != nil {
		t.Fatal(err)
	}
	loginPage := authorityRawGet(handler, "/ui/login")
	loginBody := body(t, loginPage)
	csrf := hiddenValue(t, loginBody, "csrf_token")
	var cookies []*http.Cookie
	for _, cookie := range loginPage.Cookies() {
		cookies = append(cookies, cookie)
	}
	form := url.Values{"csrf_token": {csrf}, "password": {"correct-password"}}
	req := httptest.NewRequest(http.MethodPost, "/ui/login", strings.NewReader(form.Encode()))
	req.Host = "127.0.0.1:18083"
	req.RemoteAddr = "198.51.100.9:45678"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://127.0.0.1:18083")
	for _, cookie := range cookies {
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

func authorityRawGet(handler http.Handler, target string) *http.Response {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = "127.0.0.1:18083"
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
	req.Host = "127.0.0.1:18083"
	req.RemoteAddr = "198.51.100.9:45678"
	req.AddCookie(h.cookie)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "https://127.0.0.1:18083")
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec.Result()
}

func hiddenValue(t *testing.T, page, name string) string {
	t.Helper()
	values := hiddenValues(t, page, name)
	if len(values) == 0 {
		t.Fatalf("页面缺少 %q 隐藏值：\n%s", name, page)
	}
	return values[0]
}

func hiddenValues(t *testing.T, page, name string) []string {
	t.Helper()
	pattern := regexp.MustCompile(`<input type="hidden" name="` + regexp.QuoteMeta(name) + `" value="([^"]*)"`)
	matches := pattern.FindAllStringSubmatch(page, -1)
	values := make([]string, 0, len(matches))
	for _, match := range matches {
		values = append(values, match[1])
	}
	return values
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

func TestSecretAddWritesValueWithoutEchoAndAuditsWebUISource(t *testing.T) {
	st := newIntegrationStore(t)
	mustServer(t, st, "web-1")
	h := newIntegrationHandler(t, st)
	csrf := csrfToken(t, h)

	form := h.do(http.MethodPost, "/ui/secrets/new", url.Values{
		"csrf_token": {csrf}, "name": {"API_TOKEN"},
		"note": {"网站部署"}, "environment_name": {"API_TOKEN_FILE"},
		"value": {"web-secret-value"},
	})
	formBody := body(t, form)
	if form.StatusCode != http.StatusSeeOther || form.Header.Get("Location") != "/ui/secrets/" {
		t.Fatalf("新增应跳回列表，状态码=%d 跳转=%q 页面：\n%s", form.StatusCode, form.Header.Get("Location"), formBody)
	}
	list := h.do(http.MethodGet, "/ui/secrets/", nil)
	listBody := body(t, list)
	if strings.Contains(listBody, "web-secret-value") {
		t.Fatalf("新增后页面不得包含值：\n%s", listBody)
	}

	snapshot, err := st.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Secrets) != 1 || snapshot.Secrets[0].Value == nil || string(snapshot.Secrets[0].Value) != "web-secret-value" {
		t.Fatalf("vault 最终状态不正确：%+v", snapshot.Secrets)
	}
	rows, err := st.QueryAudit(store.AuditFilter{OwnerOnly: true, Actions: []string{"secret-add"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Source != store.AuditSourceWebUI || rows[0].SourceIP != "192.0.2.1" {
		t.Fatalf("新增审计来源不正确：%+v", rows)
	}
	if strings.Contains(rows[0].Command, "web-secret-value") {
		t.Fatalf("审计摘要包含值：%s", rows[0].Command)
	}

	invalid := h.do(http.MethodPost, "/ui/secrets/new", url.Values{
		"csrf_token": {csrf}, "name": {"SHORT_TOKEN"},
		"note": {"保留备注"}, "environment_name": {"SHORT_TOKEN_FILE"},
		"value": {"short"},
	})
	invalidBody := body(t, invalid)
	if invalid.StatusCode != http.StatusBadRequest {
		t.Fatalf("校验失败状态码 = %d，页面：\n%s", invalid.StatusCode, invalidBody)
	}
	for _, want := range []string{"至少", `value="SHORT_TOKEN"`, `value="保留备注"`, `value="SHORT_TOKEN_FILE"`} {
		if !strings.Contains(invalidBody, want) {
			t.Fatalf("校验失败页面缺少 %q：\n%s", want, invalidBody)
		}
	}
	if strings.Contains(invalidBody, "short") {
		t.Fatalf("校验失败页面不得回显值：\n%s", invalidBody)
	}
	if !strings.Contains(invalidBody, `<input type="password"`) ||
		!strings.Contains(invalidBody, `autocomplete="off"`) {
		t.Fatalf("值输入应是关闭自动填充的隐藏输入框：\n%s", invalidBody)
	}

	denied := h.do(http.MethodPost, "/ui/secrets/new", url.Values{
		"name": {"NO_TOKEN"}, "value": {"another-secret-value"},
	})
	if denied.StatusCode != http.StatusForbidden {
		t.Fatalf("缺少防跨站请求伪造令牌应返回 403，实际 %d", denied.StatusCode)
	}
}

func TestSecretDetailSupportsEditValueAndDelete(t *testing.T) {
	st := newIntegrationStore(t)
	mustServer(t, st, "web-1")
	manager := secretmanage.New(st)
	if err := manager.AddSecret("OLD_TOKEN", "旧备注", "OLD_TOKEN_FILE", []byte("old-secret-value"), secretmanage.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := manager.AllowSecrets([]string{"OLD_TOKEN"}, []string{"web-1"}, secretmanage.CLISource()); err != nil {
		t.Fatal(err)
	}
	views, err := manager.List(secretmanage.ListFilter{})
	if err != nil || len(views) != 1 {
		t.Fatalf("准备机密项失败：%v %+v", err, views)
	}
	secretID := views[0].ID
	h := newIntegrationHandler(t, st)
	csrf := csrfToken(t, h)
	detailTarget := "/ui/secrets/" + secretID

	detail := h.do(http.MethodGet, detailTarget, nil)
	detailBody := body(t, detail)
	if detail.StatusCode != http.StatusOK {
		t.Fatalf("详情页状态码 = %d：\n%s", detail.StatusCode, detailBody)
	}
	for _, want := range []string{"OLD_TOKEN", "旧备注", "OLD_TOKEN_FILE", "web-1", "修改元数据", "换值", "删除"} {
		if !strings.Contains(detailBody, want) {
			t.Fatalf("详情页缺少 %q：\n%s", want, detailBody)
		}
	}
	if strings.Contains(detailBody, "old-secret-value") {
		t.Fatalf("详情页不得显示值：\n%s", detailBody)
	}

	edited := h.do(http.MethodPost, detailTarget+"/edit", url.Values{
		"csrf_token": {csrf}, "note": {"新备注"}, "environment_name": {"NEW_TOKEN_FILE"},
	})
	editedBody := body(t, edited)
	if edited.StatusCode != http.StatusSeeOther || edited.Header.Get("Location") != detailTarget {
		t.Fatalf("修改元数据应跳回详情，状态码=%d 跳转=%q 页面：\n%s", edited.StatusCode, edited.Header.Get("Location"), editedBody)
	}

	valuePage := h.do(http.MethodGet, detailTarget+"/value", nil)
	valuePageBody := body(t, valuePage)
	if valuePage.StatusCode != http.StatusOK || strings.Contains(valuePageBody, "old-secret-value") {
		t.Fatalf("换值页不得显示旧值，状态码=%d：\n%s", valuePage.StatusCode, valuePageBody)
	}
	if !strings.Contains(valuePageBody, "上一个值仍会参与输出遮蔽") ||
		!strings.Contains(valuePageBody, `autocomplete="off"`) {
		t.Fatalf("换值页应说明遮蔽并关闭自动填充：\n%s", valuePageBody)
	}
	invalidValue := h.do(http.MethodPost, detailTarget+"/value", url.Values{
		"csrf_token": {csrf}, "value": {"short"},
	})
	invalidValueBody := body(t, invalidValue)
	if invalidValue.StatusCode != http.StatusBadRequest || strings.Contains(invalidValueBody, "short") ||
		!strings.Contains(invalidValueBody, `autocomplete="off"`) {
		t.Fatalf("换值校验失败应重新渲染空值框，状态码=%d：\n%s", invalidValue.StatusCode, invalidValueBody)
	}
	changed := h.do(http.MethodPost, detailTarget+"/value", url.Values{
		"csrf_token": {csrf}, "value": {"new-secret-value"},
	})
	changedBody := body(t, changed)
	if changed.StatusCode != http.StatusSeeOther || changed.Header.Get("Location") != detailTarget {
		t.Fatalf("换值应跳回详情，状态码=%d 跳转=%q 页面：\n%s", changed.StatusCode, changed.Header.Get("Location"), changedBody)
	}
	snapshot, err := st.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if string(snapshot.Secrets[0].Value) != "new-secret-value" || string(snapshot.Secrets[0].PreviousValue) != "old-secret-value" {
		t.Fatalf("换值后的 vault 状态不正确：%+v", snapshot.Secrets[0])
	}

	deletePage := h.do(http.MethodGet, detailTarget+"/delete", nil)
	deletePageBody := body(t, deletePage)
	if deletePage.StatusCode != http.StatusOK {
		t.Fatalf("删除确认页状态码 = %d：\n%s", deletePage.StatusCode, deletePageBody)
	}
	for _, want := range []string{"OLD_TOKEN", "NEW_TOKEN_FILE", "web-1", "确认删除"} {
		if !strings.Contains(deletePageBody, want) {
			t.Fatalf("删除确认页缺少 %q：\n%s", want, deletePageBody)
		}
	}
	deleted := h.do(http.MethodPost, detailTarget+"/delete", url.Values{
		"csrf_token": {csrf},
	})
	deletedBody := body(t, deleted)
	if deleted.StatusCode != http.StatusSeeOther || deleted.Header.Get("Location") != "/ui/secrets/" {
		t.Fatalf("删除应跳回列表，状态码=%d 跳转=%q 页面：\n%s", deleted.StatusCode, deleted.Header.Get("Location"), deletedBody)
	}
	views, err = manager.List(secretmanage.ListFilter{})
	if err != nil || len(views) != 0 {
		t.Fatalf("删除后的 vault 状态不正确：%v %+v", err, views)
	}
}

func TestSecretUploadOverLimitIsRejectedBeforeReadingValue(t *testing.T) {
	st := newIntegrationStore(t)
	mustServer(t, st, "web-1")
	h := newIntegrationHandler(t, st)

	resp := postMultipart(h, "/ui/secrets/new", map[string]string{
		"csrf_token": csrfToken(t, h), "name": "BIG_TOKEN", "note": " oversized",
	}, "value_file", "value.bin", 64*1024+1)
	respBody := body(t, resp)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("上传超过 64 KiB 应提前返回 413，实际 %d：\n%s", resp.StatusCode, respBody)
	}
	if strings.Contains(respBody, "xxxx") {
		t.Fatalf("超限错误页不得包含上传值：\n%s", respBody)
	}
	rows, err := st.QueryAudit(store.AuditFilter{OwnerOnly: true, Actions: []string{"secret-add"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("超限上传不应写审计：%+v", rows)
	}
}

func TestAuthoritySecretWritesRequireReverifyAndPreserveSubmittedState(t *testing.T) {
	st := newIntegrationStore(t)
	serverID := mustServer(t, st, "web-1")
	manager := secretmanage.New(st)
	if err := manager.AddSecret("AUTH_TOKEN", "权威端备注", "AUTH_TOKEN_FILE", []byte("authority-secret-value"), secretmanage.CLISource()); err != nil {
		t.Fatal(err)
	}
	views, err := manager.List(secretmanage.ListFilter{})
	if err != nil || len(views) != 1 {
		t.Fatalf("准备机密项失败：%v %+v", err, views)
	}
	secretID := views[0].ID
	now := time.Now()
	h := newAuthorityHandler(t, st, &now)
	now = now.Add(6 * time.Minute)
	csrf := hiddenValue(t, body(t, h.do(http.MethodGet, "/ui/secrets/", nil)), "csrf_token")

	addResume := "/ui/secrets/new?environment_name=AUTH_TOKEN_FILE&name=NEW_TOKEN&note=%E6%9D%83%E5%A8%81%E7%AB%AF&resume=1"
	addRejected := h.do(http.MethodPost, "/ui/secrets/new", url.Values{
		"csrf_token": {csrf}, "name": {"NEW_TOKEN"}, "note": {"权威端"},
		"environment_name": {"AUTH_TOKEN_FILE"}, "value": {"authority-new-secret"},
	})
	if addRejected.StatusCode != http.StatusSeeOther || addRejected.Header.Get("Location") != "/ui/reverify?next="+url.QueryEscape(addResume) {
		t.Fatalf("新增应先重新验证，状态码=%d 跳转=%q", addRejected.StatusCode, addRejected.Header.Get("Location"))
	}
	valueRejected := h.do(http.MethodPost, "/ui/secrets/"+secretID+"/value", url.Values{
		"csrf_token": {csrf}, "value": {"authority-replaced-secret"},
	})
	if valueRejected.StatusCode != http.StatusSeeOther || valueRejected.Header.Get("Location") != "/ui/reverify?next="+url.QueryEscape("/ui/secrets/"+secretID+"/value?resume=1") {
		t.Fatalf("换值应先重新验证，状态码=%d 跳转=%q", valueRejected.StatusCode, valueRejected.Header.Get("Location"))
	}
	deleteRejected := h.do(http.MethodPost, "/ui/secrets/"+secretID+"/delete", url.Values{
		"csrf_token": {csrf},
	})
	if deleteRejected.StatusCode != http.StatusSeeOther || deleteRejected.Header.Get("Location") != "/ui/reverify?next="+url.QueryEscape("/ui/secrets/"+secretID+"/delete?resume=1") {
		t.Fatalf("删除应先重新验证，状态码=%d 跳转=%q", deleteRejected.StatusCode, deleteRejected.Header.Get("Location"))
	}

	preview := h.do(http.MethodPost, "/ui/secrets/authorization/preview", url.Values{
		"csrf_token": {csrf}, "return_to": {"/ui/secrets/matrix"},
		"secret_id": {secretID}, "server_id": {serverID},
		"cell__" + secretID + "__" + serverID: {"1"},
	})
	previewBody := body(t, preview)
	summary := hiddenValue(t, previewBody, "summary")
	confirmRejected := h.do(http.MethodPost, "/ui/secrets/authorization/confirm", url.Values{
		"csrf_token": {csrf}, "summary": {summary}, "return_to": {"/ui/secrets/matrix"},
		"target": {secretID + " " + serverID + " 1"},
	})
	confirmNext := "/ui/secrets/authorization/preview?return_to=%2Fui%2Fsecrets%2Fmatrix&target=" + url.QueryEscape(secretID+" "+serverID+" 1")
	if confirmRejected.StatusCode != http.StatusSeeOther || confirmRejected.Header.Get("Location") != "/ui/reverify?next="+url.QueryEscape(confirmNext) {
		t.Fatalf("确认授权应先重新验证，状态码=%d 跳转=%q", confirmRejected.StatusCode, confirmRejected.Header.Get("Location"))
	}
	rows, err := st.QueryAudit(store.AuditFilter{OwnerOnly: true, Source: store.AuditSourceWebUI, Actions: []string{"secret-add", "secret-set", "secret-rm", "secret-allow-change"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("未重新验证前不应写机密项审计：%+v", rows)
	}

	reverifyPage := h.do(http.MethodGet, "/ui/reverify?next="+url.QueryEscape(confirmNext), nil)
	reverifyCSRF := hiddenValue(t, body(t, reverifyPage), "csrf_token")
	reverified := h.do(http.MethodPost, "/ui/reverify", url.Values{
		"csrf_token": {reverifyCSRF}, "password": {"correct-password"}, "next": {confirmNext},
	})
	if reverified.StatusCode != http.StatusSeeOther || reverified.Header.Get("Location") != confirmNext {
		t.Fatalf("重新验证后应回到授权预览，状态码=%d 跳转=%q", reverified.StatusCode, reverified.Header.Get("Location"))
	}
	resumedPreview := body(t, h.do(http.MethodGet, confirmNext, nil))
	if hiddenValue(t, resumedPreview, "summary") != summary ||
		len(hiddenValues(t, resumedPreview, "target")) != 1 ||
		hiddenValues(t, resumedPreview, "target")[0] != secretID+" "+serverID+" 1" {
		t.Fatalf("重新验证后的预览应保留目标授权集合：\n%s", resumedPreview)
	}
	confirmed := h.do(http.MethodPost, "/ui/secrets/authorization/confirm", url.Values{
		"csrf_token": {csrf}, "summary": {summary}, "return_to": {"/ui/secrets/matrix"},
		"target": {secretID + " " + serverID + " 1"},
	})
	confirmedBody := body(t, confirmed)
	if confirmed.StatusCode != http.StatusOK || !strings.Contains(confirmedBody, "已保存授权变更") {
		t.Fatalf("重新验证后应能确认授权，状态码=%d：\n%s", confirmed.StatusCode, confirmedBody)
	}

	addPage := body(t, h.do(http.MethodGet, addResume, nil))
	for _, want := range []string{`value="NEW_TOKEN"`, `value="权威端"`, `value="AUTH_TOKEN_FILE"`} {
		if !strings.Contains(addPage, want) {
			t.Fatalf("新增回跳页应保留元数据 %q：\n%s", want, addPage)
		}
	}
	added := h.do(http.MethodPost, "/ui/secrets/new", url.Values{
		"csrf_token": {csrf}, "name": {"NEW_TOKEN"}, "note": {"权威端"},
		"environment_name": {"AUTH_TOKEN_FILE2"}, "value": {"authority-new-secret"},
	})
	addedBody := body(t, added)
	if added.StatusCode != http.StatusSeeOther {
		t.Fatalf("重新验证后应能新增机密项，状态码=%d：\n%s", added.StatusCode, addedBody)
	}
	rows, err = st.QueryAudit(store.AuditFilter{OwnerOnly: true, Source: store.AuditSourceWebUI, Actions: []string{"secret-add", "secret-allow-change"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("权威端写入审计数量不正确：%+v", rows)
	}
	for _, row := range rows {
		if row.Source != store.AuditSourceWebUI || row.SourceIP != "198.51.100.9" || strings.Contains(row.Command, "authority-new-secret") {
			t.Fatalf("权威端审计来源不正确：%+v", row)
		}
	}
}

func TestClientSecretsPageIsReadOnlyFromSnapshotAndWriteRoutesAreAbsent(t *testing.T) {
	st := newIntegrationStore(t)
	serverID := mustServer(t, st, "web-1")
	manager := secretmanage.New(st)
	if err := manager.AddSecret("CLIENT_TOKEN", "客户端备注", "CLIENT_TOKEN_FILE", []byte("client-secret-value"), secretmanage.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := manager.AllowSecrets([]string{"CLIENT_TOKEN"}, []string{"web-1"}, secretmanage.CLISource()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := st.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	handler, startup, err := webui.New(webui.Config{
		Role: webui.RoleClient,
		Servers: clientServerSource{
			rows: []webui.ServerRow{{ID: serverID, Name: "web-1"}},
		},
		Secrets: SnapshotSource(func(string) (*store.Snapshot, error) {
			return snapshot, nil
		}),
		Port: 18084,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, startup.URL("127.0.0.1:18084"), nil)
	req.Host = "127.0.0.1:18084"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("应签发客户端会话 cookie，实际 %d", len(cookies))
	}
	h := sessionHandler{handler: handler, cookie: *cookies[0], host: "127.0.0.1:18084"}
	page := body(t, h.do(http.MethodGet, "/ui/secrets/", nil))
	for _, want := range []string{"CLIENT_TOKEN", "客户端备注", "CLIENT_TOKEN_FILE", "web-1", "只显示本客户端可用的部分，修改请到权威端。"} {
		if !strings.Contains(page, want) {
			t.Fatalf("客户端机密项页缺少 %q：\n%s", want, page)
		}
	}
	if strings.Contains(page, "client-secret-value") || strings.Contains(page, "创建时间") || strings.Contains(page, "值最后更改") ||
		strings.Contains(page, "新增机密项") || strings.Contains(page, "授权矩阵") {
		t.Fatalf("客户端页不得显示值、时间或写入口：\n%s", page)
	}
	csrf := csrfToken(t, h)
	writeTargets := []string{
		"/ui/secrets/new", "/ui/secrets/matrix",
		"/ui/secrets/authorization/preview", "/ui/secrets/authorization/confirm",
		"/ui/secrets/" + snapshot.Secrets[0].ID,
	}
	for _, target := range writeTargets {
		resp := h.do(http.MethodPost, target, url.Values{"csrf_token": {csrf}})
		respBody := body(t, resp)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("客户端写路由 %s 不应存在，状态码=%d：\n%s", target, resp.StatusCode, respBody)
		}
	}
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

func TestSecretAuthorizationMatrixPreviewsConfirmsAndDetectsConflict(t *testing.T) {
	st := newIntegrationStore(t)
	webID := mustServer(t, st, "web-1")
	dbID := mustServer(t, st, "db-1")
	manager := secretmanage.New(st)
	if err := manager.AddSecret("WEB_TOKEN", "网站", "", []byte("web-secret-value"), secretmanage.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := manager.AddSecret("DB_TOKEN", "数据库", "", []byte("db-secret-value"), secretmanage.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := manager.AllowSecrets([]string{"WEB_TOKEN"}, []string{"web-1"}, secretmanage.CLISource()); err != nil {
		t.Fatal(err)
	}
	views, err := manager.List(secretmanage.ListFilter{})
	if err != nil || len(views) != 2 {
		t.Fatalf("准备机密项失败：%v %+v", err, views)
	}
	secretIDs := map[string]string{}
	for _, view := range views {
		secretIDs[view.Name] = view.ID
	}
	h := newIntegrationHandler(t, st)

	matrix := h.do(http.MethodGet, "/ui/secrets/matrix", nil)
	matrixBody := body(t, matrix)
	if matrix.StatusCode != http.StatusOK {
		t.Fatalf("授权矩阵状态码 = %d：\n%s", matrix.StatusCode, matrixBody)
	}
	for _, want := range []string{"WEB_TOKEN", "DB_TOKEN", "web-1", "db-1", "预览授权变更", "secrets-matrix."} {
		if !strings.Contains(matrixBody, want) {
			t.Fatalf("授权矩阵缺少 %q：\n%s", want, matrixBody)
		}
	}
	if !regexp.MustCompile(`name="cell__` + regexp.QuoteMeta(secretIDs["WEB_TOKEN"]) + `__` + regexp.QuoteMeta(webID) + `" value="1" checked`).MatchString(matrixBody) {
		t.Fatalf("授权矩阵应显示当前授权：\n%s", matrixBody)
	}

	previewForm := url.Values{
		"csrf_token": {csrfToken(t, h)},
		"secret_id":  {secretIDs["WEB_TOKEN"], secretIDs["DB_TOKEN"]},
		"server_id":  {webID, dbID},
	}
	previewForm.Set("cell__"+secretIDs["WEB_TOKEN"]+"__"+webID, "1")
	previewForm.Set("cell__"+secretIDs["WEB_TOKEN"]+"__"+dbID, "1")
	previewForm.Set("cell__"+secretIDs["DB_TOKEN"]+"__"+webID, "1")
	preview := h.do(http.MethodPost, "/ui/secrets/authorization/preview", previewForm)
	previewBody := body(t, preview)
	if preview.StatusCode != http.StatusOK {
		t.Fatalf("授权预览状态码 = %d：\n%s", preview.StatusCode, previewBody)
	}
	for _, want := range []string{"新增 2", "移除 0", "WEB_TOKEN", "db-1", "DB_TOKEN", "web-1", `name="summary"`} {
		if !strings.Contains(previewBody, want) {
			t.Fatalf("授权预览缺少 %q：\n%s", want, previewBody)
		}
	}
	summary := hiddenValue(t, previewBody, "summary")
	targets := hiddenValues(t, previewBody, "target")
	if len(targets) != 4 || strings.Contains(previewBody, "web-secret-value") || strings.Contains(previewBody, "db-secret-value") {
		t.Fatalf("授权预览目标或值泄露处理不正确：%v\n%s", targets, previewBody)
	}

	if err := manager.AllowSecrets([]string{"WEB_TOKEN"}, []string{"db-1"}, secretmanage.CLISource()); err != nil {
		t.Fatal(err)
	}
	confirmForm := url.Values{"csrf_token": {csrfToken(t, h)}, "summary": {summary}, "return_to": {"/ui/secrets/matrix"}}
	for _, target := range targets {
		confirmForm.Add("target", target)
	}
	conflict := h.do(http.MethodPost, "/ui/secrets/authorization/confirm", confirmForm)
	conflictBody := body(t, conflict)
	if conflict.StatusCode != http.StatusConflict || !strings.Contains(conflictBody, "授权状态已被其他入口修改") ||
		!strings.Contains(conflictBody, "DB_TOKEN") || !strings.Contains(conflictBody, "web-1") {
		t.Fatalf("并发修改应返回 409 并显示最新预览，状态码=%d：\n%s", conflict.StatusCode, conflictBody)
	}
	latestSummary := hiddenValue(t, conflictBody, "summary")
	confirmForm.Set("summary", latestSummary)
	confirmed := h.do(http.MethodPost, "/ui/secrets/authorization/confirm", confirmForm)
	confirmedBody := body(t, confirmed)
	if confirmed.StatusCode != http.StatusOK || !strings.Contains(confirmedBody, "已保存授权变更") {
		t.Fatalf("最新摘要确认应显示结果，状态码=%d：\n%s", confirmed.StatusCode, confirmedBody)
	}

	views, err = manager.List(secretmanage.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range views {
		if view.Name == "WEB_TOKEN" && strings.Join(view.AllowedServers, ",") != "db-1,web-1" {
			t.Fatalf("WEB_TOKEN 最终授权不正确：%+v", view)
		}
		if view.Name == "DB_TOKEN" && strings.Join(view.AllowedServers, ",") != "web-1" {
			t.Fatalf("DB_TOKEN 最终授权不正确：%+v", view)
		}
	}
}

func TestSecretAuthorizationScopeCoversOnlyVisibleMatrixAndDetailCells(t *testing.T) {
	st := newIntegrationStore(t)
	webID := mustServer(t, st, "web-1")
	dbID := mustServer(t, st, "db-1")
	manager := secretmanage.New(st)
	if err := manager.AddSecret("API_TOKEN", "", "", []byte("api-secret-value"), secretmanage.CLISource()); err != nil {
		t.Fatal(err)
	}
	views, err := manager.List(secretmanage.ListFilter{})
	if err != nil || len(views) != 1 {
		t.Fatalf("准备机密项失败：%v %+v", err, views)
	}
	secretID := views[0].ID
	h := newIntegrationHandler(t, st)

	filtered := h.do(http.MethodGet, "/ui/secrets/matrix?server=db-1", nil)
	filteredBody := body(t, filtered)
	if filtered.StatusCode != http.StatusOK {
		t.Fatalf("筛选后的矩阵状态码 = %d：\n%s", filtered.StatusCode, filteredBody)
	}
	if strings.Contains(filteredBody, `name="server_id" value="`+webID+`"`) ||
		!strings.Contains(filteredBody, `name="server_id" value="`+dbID+`"`) {
		t.Fatalf("矩阵筛选后只应提交可见服务器列：\n%s", filteredBody)
	}

	detail := h.do(http.MethodGet, "/ui/secrets/"+secretID, nil)
	detailBody := body(t, detail)
	if !strings.Contains(detailBody, "可写入服务器勾选清单") ||
		!strings.Contains(detailBody, `name="cell__`+secretID+`__`+dbID+`"`) {
		t.Fatalf("详情页应有单个机密项的勾选清单：\n%s", detailBody)
	}
	preview := h.do(http.MethodPost, "/ui/secrets/authorization/preview", url.Values{
		"csrf_token": {csrfToken(t, h)}, "return_to": {"/ui/secrets/" + secretID},
		"secret_id": {secretID}, "server_id": {webID, dbID},
		"cell__" + secretID + "__" + webID: {"1"}, "cell__" + secretID + "__" + dbID: {"1"},
	})
	previewBody := body(t, preview)
	if preview.StatusCode != http.StatusOK || !strings.Contains(previewBody, "新增 2") {
		t.Fatalf("单个机密项勾选清单应能预览授权，状态码=%d：\n%s", preview.StatusCode, previewBody)
	}
}

// TestSecretUploadExactlyAtLimitSucceedsOnNewAndValuePages 覆盖审查第 1 项：
// 恰好 64 KiB 的合法文件应能通过新增页与换值页保存，多部分表单本身的字段
// 与文件名开销不能挤占值的上限；超过 1 个字节仍在读取值之前拒绝。
func TestSecretUploadExactlyAtLimitSucceedsOnNewAndValuePages(t *testing.T) {
	st := newIntegrationStore(t)
	h := newIntegrationHandler(t, st)
	tempWatch := t.TempDir()
	t.Setenv("TMPDIR", tempWatch)
	longFileName := "very-long-machine-generated-value-file-name.bin"

	added := postMultipart(h, "/ui/secrets/new", map[string]string{
		"csrf_token": csrfToken(t, h), "name": "LIMIT_TOKEN", "note": "恰好上限",
		"environment_name": "LIMIT_TOKEN_FILE", "value": "",
	}, "value_file", longFileName, 64*1024)
	addedBody := body(t, added)
	if added.StatusCode != http.StatusSeeOther || added.Header.Get("Location") != "/ui/secrets/" {
		t.Fatalf("新增页上传恰好 64 KiB 应成功，状态码=%d 跳转=%q 页面：\n%s", added.StatusCode, added.Header.Get("Location"), addedBody)
	}
	manager := secretmanage.New(st)
	views, err := manager.List(secretmanage.ListFilter{})
	if err != nil || len(views) != 1 {
		t.Fatalf("读取新增后的机密项失败：%v %+v", err, views)
	}
	secretID := views[0].ID
	snapshot, err := st.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Secrets) != 1 || len(snapshot.Secrets[0].Value) != 64*1024 {
		t.Fatalf("新增后应保存完整 64 KiB 值，实际长度=%d", len(snapshot.Secrets[0].Value))
	}

	replaced := postMultipart(h, "/ui/secrets/"+secretID+"/value", map[string]string{
		"csrf_token": csrfToken(t, h), "value": "",
	}, "value_file", longFileName, 64*1024)
	replacedBody := body(t, replaced)
	if replaced.StatusCode != http.StatusSeeOther || replaced.Header.Get("Location") != "/ui/secrets/"+secretID {
		t.Fatalf("换值页上传恰好 64 KiB 应成功，状态码=%d 跳转=%q 页面：\n%s", replaced.StatusCode, replaced.Header.Get("Location"), replacedBody)
	}
	snapshot, err = st.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Secrets[0].Value) != 64*1024 || len(snapshot.Secrets[0].PreviousValue) != 64*1024 {
		t.Fatalf("换值后应保存完整 64 KiB 值，实际长度=%d/%d",
			len(snapshot.Secrets[0].Value), len(snapshot.Secrets[0].PreviousValue))
	}

	overLimit := postMultipart(h, "/ui/secrets/"+secretID+"/value", map[string]string{
		"csrf_token": csrfToken(t, h), "value": "",
	}, "value_file", longFileName, 64*1024+1)
	overLimitBody := body(t, overLimit)
	if overLimit.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("换值页上传超过 64 KiB 应返回 413，实际 %d：\n%s", overLimit.StatusCode, overLimitBody)
	}
	leftovers, err := os.ReadDir(tempWatch)
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("上传解析不得把机密项值写进磁盘临时文件：%v", leftovers)
	}
}

// TestSecretAuthorizationConfirmDeletedSecretReturnsConflictWithLatestPreview
// 覆盖审查第 2 项：预览之后相关机密项被删除，确认页面应返回 409 并展示按
// 剩余格子重新计算的最新预览，而不是 500。
func TestSecretAuthorizationConfirmDeletedSecretReturnsConflictWithLatestPreview(t *testing.T) {
	st := newIntegrationStore(t)
	webID := mustServer(t, st, "web-1")
	dbID := mustServer(t, st, "db-1")
	manager := secretmanage.New(st)
	for _, name := range []string{"WEB_TOKEN", "DB_TOKEN"} {
		if err := manager.AddSecret(name, "", "", []byte(name+"-secret-value"), secretmanage.CLISource()); err != nil {
			t.Fatal(err)
		}
	}
	views, err := manager.List(secretmanage.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	secretIDs := make(map[string]string, len(views))
	for _, view := range views {
		secretIDs[view.Name] = view.ID
	}
	h := newIntegrationHandler(t, st)
	previewForm := url.Values{
		"csrf_token": {csrfToken(t, h)},
		"secret_id":  {secretIDs["WEB_TOKEN"], secretIDs["DB_TOKEN"]},
		"server_id":  {webID, dbID},
	}
	previewForm.Set("cell__"+secretIDs["WEB_TOKEN"]+"__"+webID, "1")
	previewForm.Set("cell__"+secretIDs["DB_TOKEN"]+"__"+dbID, "1")
	preview := h.do(http.MethodPost, "/ui/secrets/authorization/preview", previewForm)
	previewBody := body(t, preview)
	if preview.StatusCode != http.StatusOK {
		t.Fatalf("授权预览状态码 = %d：\n%s", preview.StatusCode, previewBody)
	}
	summary := hiddenValue(t, previewBody, "summary")
	targets := hiddenValues(t, previewBody, "target")

	if err := manager.DeleteSecret("WEB_TOKEN", secretmanage.CLISource()); err != nil {
		t.Fatal(err)
	}
	confirmForm := url.Values{"csrf_token": {csrfToken(t, h)}, "summary": {summary}, "return_to": {"/ui/secrets/matrix"}}
	for _, target := range targets {
		confirmForm.Add("target", target)
	}
	conflict := h.do(http.MethodPost, "/ui/secrets/authorization/confirm", confirmForm)
	conflictBody := body(t, conflict)
	if conflict.StatusCode != http.StatusConflict {
		t.Fatalf("机密项被删除后确认应返回 409，实际 %d：\n%s", conflict.StatusCode, conflictBody)
	}
	for _, want := range []string{"授权状态已被其他入口修改", "DB_TOKEN", "db-1"} {
		if !strings.Contains(conflictBody, want) {
			t.Fatalf("冲突页面缺少 %q：\n%s", want, conflictBody)
		}
	}
	if strings.Contains(conflictBody, "WEB_TOKEN") {
		t.Fatalf("最新预览不应再包含已删除的机密项：\n%s", conflictBody)
	}
	latestSummary := hiddenValue(t, conflictBody, "summary")
	confirmForm.Set("summary", latestSummary)
	confirmForm = pruneDeletedTargets(confirmForm, secretIDs["WEB_TOKEN"])
	confirmed := h.do(http.MethodPost, "/ui/secrets/authorization/confirm", confirmForm)
	confirmedBody := body(t, confirmed)
	if confirmed.StatusCode != http.StatusOK || !strings.Contains(confirmedBody, "已保存授权变更") {
		t.Fatalf("按剩余格子的最新摘要确认应成功，状态码=%d：\n%s", confirmed.StatusCode, confirmedBody)
	}
}

// pruneDeletedTargets 从确认表单里去掉属于已删除机密项的目标，模拟操作者
// 使用最新预览页面重新确认。
func pruneDeletedTargets(form url.Values, deletedSecretID string) url.Values {
	out := url.Values{}
	for key, values := range form {
		if key != "target" {
			out[key] = values
			continue
		}
		for _, value := range values {
			if !strings.HasPrefix(value, deletedSecretID+" ") {
				out.Add("target", value)
			}
		}
	}
	return out
}

// TestSecretMetadataEditWithoutChangeWritesNothing 覆盖审查第 3 项：Web 表
// 单提交与当前值完全相同的备注与环境变量时，不写数据也不写审计，页面提示
// 没有变化；只有确实变化的字段才进入修改。
func TestSecretMetadataEditWithoutChangeWritesNothing(t *testing.T) {
	st := newIntegrationStore(t)
	manager := secretmanage.New(st)
	if err := manager.AddSecret("API_TOKEN", "原备注", "API_TOKEN_FILE", []byte("api-secret-value"), secretmanage.CLISource()); err != nil {
		t.Fatal(err)
	}
	views, err := manager.List(secretmanage.ListFilter{})
	if err != nil || len(views) != 1 {
		t.Fatalf("准备机密项失败：%v %+v", err, views)
	}
	detailTarget := "/ui/secrets/" + views[0].ID
	h := newIntegrationHandler(t, st)
	csrf := csrfToken(t, h)

	unchanged := h.do(http.MethodPost, detailTarget+"/edit", url.Values{
		"csrf_token": {csrf}, "note": {"原备注"}, "environment_name": {"API_TOKEN_FILE"},
	})
	unchangedBody := body(t, unchanged)
	if unchanged.StatusCode != http.StatusOK || !strings.Contains(unchangedBody, "没有变化，未写入数据或审计。") {
		t.Fatalf("未变化的提交应停留在详情页并提示没有变化，状态码=%d：\n%s", unchanged.StatusCode, unchangedBody)
	}
	rows, err := webUISecretAuditRows(st, "secret-edit", "secret-env")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("未变化的提交不应写审计：%+v", rows)
	}

	noteOnly := h.do(http.MethodPost, detailTarget+"/edit", url.Values{
		"csrf_token": {csrf}, "note": {"新备注"}, "environment_name": {"API_TOKEN_FILE"},
	})
	if noteOnly.StatusCode != http.StatusSeeOther {
		t.Fatalf("只改备注应正常保存，状态码=%d：%s", noteOnly.StatusCode, body(t, noteOnly))
	}
	envOnly := h.do(http.MethodPost, detailTarget+"/edit", url.Values{
		"csrf_token": {csrf}, "note": {"新备注"}, "environment_name": {"API_TOKEN_FILE2"},
	})
	if envOnly.StatusCode != http.StatusSeeOther {
		t.Fatalf("只改环境变量名应正常保存，状态码=%d：%s", envOnly.StatusCode, body(t, envOnly))
	}
	rows, err = webUISecretAuditRows(st, "secret-edit", "secret-env")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("只有实际变化的字段才写审计，实际 %+v", rows)
	}
	after, err := manager.List(secretmanage.ListFilter{})
	if err != nil || len(after) != 1 || after[0].Note != "新备注" || after[0].EnvironmentName != "API_TOKEN_FILE2" {
		t.Fatalf("修改后的元数据不正确：%v %+v", err, after)
	}
}

// webUISecretAuditRows 只取 Web UI 来源的机密项审计行，排除测试准备阶段
// 命令行写入的审计。
func webUISecretAuditRows(st *store.Store, actions ...string) ([]store.AuditRow, error) {
	return st.QueryAudit(store.AuditFilter{OwnerOnly: true, Source: store.AuditSourceWebUI, Actions: actions})
}
