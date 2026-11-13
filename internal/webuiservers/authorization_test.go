package webuiservers

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
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
)

func newAuthorizationTestHandler(t *testing.T, st *store.Store) (http.Handler, *http.Cookie) {
	t.Helper()
	handler, startup, err := webui.New(webui.Config{
		Role:          webui.RoleStandalone,
		Servers:       FromManager(servermanage.New(st)),
		ServerWriter:  WriterFromManager(servermanage.New(st)),
		Access:        webuiaccess.FromManager(access.New(st)),
		Secrets:       webuisecrets.FromManager(secretmanage.New(st)),
		SecretsWriter: webuisecrets.WriterFromManager(secretmanage.New(st)),
		Port:          18084,
	})
	if err != nil {
		t.Fatalf("构造 Web UI Handler 失败：%v", err)
	}
	request := httptest.NewRequest(http.MethodGet, startup.URL("127.0.0.1:18084"), nil)
	request.Host = "127.0.0.1:18084"
	request.RemoteAddr = "127.0.0.1:12345"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("应签发会话 cookie，实际 %d", len(cookies))
	}
	return handler, cookies[0]
}

func authorizationRequest(
	handler http.Handler, cookie *http.Cookie, method, target string, form url.Values,
) *http.Response {
	body := strings.NewReader("")
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	request := httptest.NewRequest(method, target, body)
	request.Host = "127.0.0.1:18084"
	request.AddCookie(cookie)
	if form != nil {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Origin", "http://127.0.0.1:18084")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder.Result()
}

func authorizationBody(t *testing.T, response *http.Response) string {
	t.Helper()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	return string(data)
}

func authorizationCSRF(t *testing.T, handler http.Handler, cookie *http.Cookie) string {
	t.Helper()
	page := authorizationBody(t, authorizationRequest(handler, cookie, http.MethodGet, "/ui/secrets/", nil))
	return authorizationCSRFValue(page)
}

func authorizationCSRFValue(page string) string {
	start := strings.Index(page, `name="csrf-token" content="`)
	if start < 0 {
		panic("页面缺少防跨站请求伪造令牌")
	}
	start += len(`name="csrf-token" content="`)
	end := strings.Index(page[start:], `"`)
	if end < 0 {
		panic("页面令牌格式不正确")
	}
	return page[start : start+end]
}

func authorizationHiddenValues(page, field string) []string {
	prefix := `name="` + field + `" value="`
	values := make([]string, 0, 2)
	for {
		start := strings.Index(page, prefix)
		if start < 0 {
			return values
		}
		start += len(prefix)
		end := strings.Index(page[start:], `"`)
		if end < 0 {
			return values
		}
		values = append(values, page[start:start+end])
		page = page[start+end:]
	}
}

func authorizationHiddenOne(page, field string) string {
	values := authorizationHiddenValues(page, field)
	if len(values) == 0 {
		panic("页面缺少隐藏字段 " + field + "：\n" + page)
	}
	return values[0]
}

// TestServerDetailEditsOrdinarySecretAuthorization 用真实 vault 走公开 HTTP
// 入口：详情页提交固定服务器条目的目标集合，预览确认后复用机密项授权事务。
func TestServerDetailEditsOrdinarySecretAuthorization(t *testing.T) {
	key, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	server, err := st.AddServer(&models.Server{
		Name: "web", Host: "192.0.2.10", Port: 22, User: "deploy",
	})
	if err != nil {
		t.Fatal(err)
	}
	secrets := secretmanage.New(st)
	if err := secrets.AddSecret("API_TOKEN", "接口", "API_TOKEN", []byte("api-secret-value"), secretmanage.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := secrets.AddSecret("BACKUP_TOKEN", "备份", "BACKUP_TOKEN", []byte("backup-secret-value"), secretmanage.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := secrets.AllowSecrets([]string{"API_TOKEN"}, []string{"web"}, secretmanage.CLISource()); err != nil {
		t.Fatal(err)
	}
	views, err := secrets.List(secretmanage.ListFilter{})
	if err != nil || len(views) != 2 {
		t.Fatalf("准备机密项失败：%v，条目=%d", err, len(views))
	}
	secretIDs := map[string]string{}
	for _, view := range views {
		secretIDs[view.Name] = view.ID
	}
	handler, cookie := newAuthorizationTestHandler(t, st)
	csrf := authorizationCSRF(t, handler, cookie)

	detail := authorizationRequest(handler, cookie, http.MethodGet, "/ui/servers/"+server, nil)
	detailBody := authorizationBody(t, detail)
	if detail.StatusCode != http.StatusOK {
		t.Fatalf("服务器详情状态码=%d：\n%s", detail.StatusCode, detailBody)
	}
	if !strings.Contains(detailBody, `name="secret_id" value="`+secretIDs["API_TOKEN"]+`"`) ||
		!strings.Contains(detailBody, `name="secret_id" value="`+secretIDs["BACKUP_TOKEN"]+`"`) {
		t.Fatalf("机密项编辑组应列出这台服务器可调整的全部普通机密项：\n%s", detailBody)
	}
	apiCell := `name="cell__` + secretIDs["API_TOKEN"] + `__` + server + `" value="1" checked`
	backupCell := `name="cell__` + secretIDs["BACKUP_TOKEN"] + `__` + server + `" value="1"`
	if !strings.Contains(detailBody, apiCell) || strings.Contains(detailBody, backupCell+` checked`) {
		t.Fatalf("机密项编辑组应显示当前授权状态：\n%s", detailBody)
	}

	previewForm := url.Values{
		"csrf_token": {csrf}, "return_to": {"/ui/servers/" + server},
		"secret_id": {secretIDs["API_TOKEN"], secretIDs["BACKUP_TOKEN"]},
		"server_id": {server},
	}
	previewForm.Set("cell__"+secretIDs["API_TOKEN"]+"__"+server, "1")
	previewForm.Set("cell__"+secretIDs["BACKUP_TOKEN"]+"__"+server, "1")
	preview := authorizationRequest(handler, cookie, http.MethodPost, "/ui/secrets/authorization/preview", previewForm)
	previewBody := authorizationBody(t, preview)
	if preview.StatusCode != http.StatusOK || !strings.Contains(previewBody, "新增 1") ||
		!strings.Contains(previewBody, "BACKUP_TOKEN") || strings.Contains(previewBody, "api-secret-value") {
		t.Fatalf("普通机密项预览不正确，状态码=%d：\n%s", preview.StatusCode, previewBody)
	}
	summary := authorizationHiddenValues(previewBody, "summary")
	targets := authorizationHiddenValues(previewBody, "target")
	if len(summary) != 1 || len(targets) != 2 {
		t.Fatalf("预览应保留两个目标授权格：%v %v\n%s", summary, targets, previewBody)
	}

	confirmForm := url.Values{
		"csrf_token": {csrf}, "summary": summary, "return_to": {"/ui/servers/" + server},
	}
	for _, target := range targets {
		confirmForm.Add("target", target)
	}
	confirmed := authorizationRequest(handler, cookie, http.MethodPost, "/ui/secrets/authorization/confirm", confirmForm)
	confirmedBody := authorizationBody(t, confirmed)
	if confirmed.StatusCode != http.StatusOK || !strings.Contains(confirmedBody, "已保存授权变更") {
		t.Fatalf("确认普通机密项授权失败，状态码=%d：\n%s", confirmed.StatusCode, confirmedBody)
	}
	views, err = secrets.List(secretmanage.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range views {
		want := ""
		if view.Name == "API_TOKEN" || view.Name == "BACKUP_TOKEN" {
			want = "web"
		}
		if strings.Join(view.AllowedServers, ",") != want {
			t.Fatalf("%s 最终可写入服务器不正确：%v", view.Name, view.AllowedServers)
		}
	}
	rows, err := st.QueryAudit(store.AuditFilter{
		Source: store.AuditSourceWebUI, Actions: []string{"secret-allow-change"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || strings.Contains(rows[0].Command, "api-secret-value") ||
		strings.Contains(rows[0].Command, "backup-secret-value") {
		t.Fatalf("应只写一条不含值的 Web UI 审计：%+v", rows)
	}
}

// TestServerDetailEditsSharedStorageAuthorization 验证共享存储组仍写入
// 挂载密码机密项的可写入服务器列表，但预览与结果都按共享名呈现影响。
func TestServerDetailEditsSharedStorageAuthorization(t *testing.T) {
	key, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.AddDevice("storage", "共享存储提供方", ""); err != nil {
		t.Fatal(err)
	}
	server, err := st.AddServer(&models.Server{
		Name: "web", Host: "192.0.2.10", Port: 22, User: "deploy",
	})
	if err != nil {
		t.Fatal(err)
	}
	secrets := secretmanage.New(st)
	if err := secrets.AddSecret("DATASET_PASSWORD", "数据集挂载密码", "", []byte("dataset-secret-value"), secretmanage.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := secrets.AddSecret("MEDIA_PASSWORD", "媒体挂载密码", "", []byte("media-secret-value"), secretmanage.CLISource()); err != nil {
		t.Fatal(err)
	}
	for _, input := range []store.SharedStorageInput{
		{Name: "datasets", Device: "storage", User: "mount-user", PasswordSecret: "DATASET_PASSWORD", Drive: "Z"},
		{Name: "scratch", Device: "storage", User: "mount-user", PasswordSecret: "DATASET_PASSWORD", Drive: "Y"},
		{Name: "media", Device: "storage", User: "media-user", PasswordSecret: "MEDIA_PASSWORD", Drive: "X"},
	} {
		if _, err := st.AddSharedStorage(input, store.CLISource()); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.AllowSecrets([]string{"DATASET_PASSWORD"}, []string{"web"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	views, err := secrets.List(secretmanage.ListFilter{})
	if err != nil || len(views) != 2 {
		t.Fatalf("准备机密项失败：%v，条目=%d", err, len(views))
	}
	secretIDs := map[string]string{}
	for _, view := range views {
		secretIDs[view.Name] = view.ID
	}
	handler, cookie := newAuthorizationTestHandler(t, st)
	csrf := authorizationCSRF(t, handler, cookie)

	detail := authorizationRequest(handler, cookie, http.MethodGet, "/ui/servers/"+server, nil)
	detailBody := authorizationBody(t, detail)
	if detail.StatusCode != http.StatusOK {
		t.Fatalf("服务器详情状态码=%d：\n%s", detail.StatusCode, detailBody)
	}
	for _, want := range []string{"datasets", "scratch", "media", "预览共享存储授权变更"} {
		if !strings.Contains(detailBody, want) {
			t.Fatalf("共享存储编辑组缺少 %q：\n%s", want, detailBody)
		}
	}
	datasetCell := `name="cell__` + secretIDs["DATASET_PASSWORD"] + `__` + server + `" value="1"`
	mediaCell := `name="cell__` + secretIDs["MEDIA_PASSWORD"] + `__` + server + `" value="1"`
	if !strings.Contains(detailBody, datasetCell+` checked`) || strings.Contains(detailBody, mediaCell+` checked`) {
		t.Fatalf("共享存储编辑组应显示当前挂载能力：\n%s", detailBody)
	}

	previewForm := url.Values{
		"csrf_token": {csrf}, "return_to": {"/ui/servers/" + server},
		"authorization_view": {"server-shares"}, "server_id": {server},
		"secret_id": {secretIDs["DATASET_PASSWORD"], secretIDs["MEDIA_PASSWORD"]},
	}
	previewForm.Set("cell__"+secretIDs["MEDIA_PASSWORD"]+"__"+server, "1")
	preview := authorizationRequest(handler, cookie, http.MethodPost, "/ui/secrets/authorization/preview", previewForm)
	previewBody := authorizationBody(t, preview)
	if preview.StatusCode != http.StatusOK ||
		!strings.Contains(previewBody, "datasets") || !strings.Contains(previewBody, "scratch") ||
		!strings.Contains(previewBody, "media") ||
		strings.Contains(previewBody, "DATASET_PASSWORD") || strings.Contains(previewBody, "MEDIA_PASSWORD") ||
		strings.Contains(previewBody, "dataset-secret-value") || strings.Contains(previewBody, "media-secret-value") {
		t.Fatalf("共享存储预览应按共享名呈现且不显示密码机密项名字或值，状态码=%d：\n%s", preview.StatusCode, previewBody)
	}
	summary := authorizationHiddenValues(previewBody, "summary")
	targets := authorizationHiddenValues(previewBody, "target")
	if len(summary) != 1 || len(targets) != 2 {
		t.Fatalf("共享存储预览应保留两个目标授权格：%v %v\n%s", summary, targets, previewBody)
	}

	confirmForm := url.Values{
		"csrf_token": {csrf}, "summary": summary,
		"return_to": {"/ui/servers/" + server}, "authorization_view": {"server-shares"},
	}
	for _, target := range targets {
		confirmForm.Add("target", target)
	}
	confirmed := authorizationRequest(handler, cookie, http.MethodPost, "/ui/secrets/authorization/confirm", confirmForm)
	confirmedBody := authorizationBody(t, confirmed)
	if confirmed.StatusCode != http.StatusOK || !strings.Contains(confirmedBody, "已保存授权变更") {
		t.Fatalf("确认共享存储授权失败，状态码=%d：\n%s", confirmed.StatusCode, confirmedBody)
	}
	wantServers := map[string]bool{"datasets": false, "scratch": false, "media": true}
	wantVersions := map[string]int64{"datasets": 3, "scratch": 3, "media": 2}
	for name := range wantServers {
		share, err := st.GetSharedStorage(name)
		if err != nil {
			t.Fatal(err)
		}
		hasServer := false
		for _, mountable := range share.MountableServers {
			if mountable.ID == server {
				hasServer = true
				break
			}
		}
		if hasServer != wantServers[name] || share.Version != wantVersions[name] {
			t.Fatalf("共享 %s 的可挂载范围或版本号不正确：可挂载=%v 版本=%d", name, hasServer, share.Version)
		}
	}
	rows, err := st.QueryAudit(store.AuditFilter{
		Source: store.AuditSourceWebUI, Actions: []string{"secret-allow-change"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 ||
		strings.Contains(rows[0].Command, "dataset-secret-value") ||
		strings.Contains(rows[0].Command, "media-secret-value") ||
		strings.Contains(rows[1].Command, "dataset-secret-value") ||
		strings.Contains(rows[1].Command, "media-secret-value") {
		t.Fatalf("应逐个机密项写不含值的 Web UI 审计：%+v", rows)
	}
}

// TestServerDetailAuthorizationNoChangeAndConflict 验证固定服务器表单在没
// 有变化时不写入；其他入口改过同一授权后，过期预览整体拒绝并显示最新预览。
func TestServerDetailAuthorizationNoChangeAndConflict(t *testing.T) {
	key, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	server, err := st.AddServer(&models.Server{
		Name: "web", Host: "192.0.2.10", Port: 22, User: "deploy",
	})
	if err != nil {
		t.Fatal(err)
	}
	secrets := secretmanage.New(st)
	if err := secrets.AddSecret("API_TOKEN", "接口", "API_TOKEN", []byte("api-secret-value"), secretmanage.CLISource()); err != nil {
		t.Fatal(err)
	}
	views, err := secrets.List(secretmanage.ListFilter{})
	if err != nil || len(views) != 1 {
		t.Fatalf("准备机密项失败：%v，条目=%d", err, len(views))
	}
	secretID := views[0].ID
	handler, cookie := newAuthorizationTestHandler(t, st)
	csrf := authorizationCSRF(t, handler, cookie)

	noChange := authorizationRequest(handler, cookie, http.MethodPost, "/ui/secrets/authorization/preview", url.Values{
		"csrf_token": {csrf}, "return_to": {"/ui/servers/" + server},
		"secret_id": {secretID}, "server_id": {server},
	})
	noChangeBody := authorizationBody(t, noChange)
	if noChange.StatusCode != http.StatusOK || !strings.Contains(noChangeBody, "没有变化") ||
		strings.Contains(noChangeBody, "确认保存授权") {
		t.Fatalf("无变化预览不应提供确认按钮，状态码=%d：\n%s", noChange.StatusCode, noChangeBody)
	}

	preview := authorizationRequest(handler, cookie, http.MethodPost, "/ui/secrets/authorization/preview", url.Values{
		"csrf_token": {csrf}, "return_to": {"/ui/servers/" + server},
		"secret_id": {secretID}, "server_id": {server},
		"cell__" + secretID + "__" + server: {"1"},
	})
	previewBody := authorizationBody(t, preview)
	summary := authorizationHiddenValues(previewBody, "summary")
	targets := authorizationHiddenValues(previewBody, "target")
	if preview.StatusCode != http.StatusOK || len(summary) != 1 || len(targets) != 1 {
		t.Fatalf("准备冲突预览失败，状态码=%d：%v %v\n%s", preview.StatusCode, summary, targets, previewBody)
	}
	if err := secrets.AllowSecrets([]string{"API_TOKEN"}, []string{"web"}, secretmanage.CLISource()); err != nil {
		t.Fatal(err)
	}
	confirmForm := url.Values{
		"csrf_token": {csrf}, "summary": summary, "return_to": {"/ui/servers/" + server},
	}
	for _, target := range targets {
		confirmForm.Add("target", target)
	}
	conflict := authorizationRequest(handler, cookie, http.MethodPost, "/ui/secrets/authorization/confirm", confirmForm)
	conflictBody := authorizationBody(t, conflict)
	if conflict.StatusCode != http.StatusConflict ||
		!strings.Contains(conflictBody, "授权状态已被其他入口修改") ||
		!strings.Contains(conflictBody, "最新预览") {
		t.Fatalf("过期摘要应整体拒绝并显示最新预览，状态码=%d：\n%s", conflict.StatusCode, conflictBody)
	}
	rows, err := st.QueryAudit(store.AuditFilter{Source: store.AuditSourceWebUI, Actions: []string{"secret-allow-change"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("无变化与冲突提交都不应写 Web UI 审计：%+v", rows)
	}
}

// TestServerDetailEditsProfileAuthorization 用真实 vault 走公开 HTTP 入口：
// 详情页一次提交多个访问配置组的差异，预览确认后沿用 profile 授权审计。
func TestServerDetailEditsProfileAuthorization(t *testing.T) {
	key, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	server, err := st.AddServer(&models.Server{
		Name: "web", Host: "192.0.2.20", Port: 22, User: "deploy",
	})
	if err != nil {
		t.Fatal(err)
	}
	profileIDs := make(map[string]string, 3)
	for _, name := range []string{"team-a", "team-b", "team-c"} {
		id, err := st.AddProfile(name)
		if err != nil {
			t.Fatal(err)
		}
		profileIDs[name] = id
	}
	if err := st.GrantServers(profileIDs["team-a"], []string{server}); err != nil {
		t.Fatal(err)
	}
	handler, cookie := newAuthorizationTestHandler(t, st)
	csrf := authorizationCSRF(t, handler, cookie)

	detail := authorizationRequest(handler, cookie, http.MethodGet, "/ui/servers/"+server, nil)
	detailBody := authorizationBody(t, detail)
	if detail.StatusCode != http.StatusOK ||
		!strings.Contains(detailBody, "/ui/servers/profile-authorization/preview") ||
		!strings.Contains(detailBody, "team-a") ||
		!strings.Contains(detailBody, "team-b") ||
		!strings.Contains(detailBody, "team-c") ||
		!strings.Contains(detailBody, `value="`+profileIDs["team-a"]+`" checked`) ||
		strings.Contains(detailBody, `value="`+profileIDs["team-b"]+`" checked`) {
		t.Fatalf("详情页应列出全部 profile 并预选当前授权，状态码=%d：\n%s", detail.StatusCode, detailBody)
	}

	previewForm := url.Values{
		"csrf_token": {csrf}, "server_id": {server},
		"profile": {profileIDs["team-b"], profileIDs["team-c"]},
	}
	preview := authorizationRequest(handler, cookie, http.MethodPost, "/ui/servers/profile-authorization/preview", previewForm)
	previewBody := authorizationBody(t, preview)
	if preview.StatusCode != http.StatusOK ||
		!strings.Contains(previewBody, "新增") || !strings.Contains(previewBody, "team-b") ||
		!strings.Contains(previewBody, "team-c") || !strings.Contains(previewBody, "移除") ||
		!strings.Contains(previewBody, "team-a") ||
		!strings.Contains(previewBody, "确认保存 profile 授权") {
		t.Fatalf("profile 授权预览应显示新增与移除清单，状态码=%d：\n%s", preview.StatusCode, previewBody)
	}
	summary := authorizationHiddenValues(previewBody, "summary")
	if len(summary) != 1 {
		t.Fatalf("预览应携带一个确认摘要：%v\n%s", summary, previewBody)
	}

	confirmForm := url.Values{
		"csrf_token": {csrf}, "server_id": {server}, "summary": summary,
		"profile": {profileIDs["team-b"], profileIDs["team-c"]},
	}
	confirmed := authorizationRequest(handler, cookie, http.MethodPost, "/ui/servers/profile-authorization/confirm", confirmForm)
	confirmedBody := authorizationBody(t, confirmed)
	if confirmed.StatusCode != http.StatusOK || !strings.Contains(confirmedBody, "已保存 profile 授权") {
		t.Fatalf("确认 profile 授权失败，状态码=%d：\n%s", confirmed.StatusCode, confirmedBody)
	}
	for name, wantGranted := range map[string]bool{
		"team-a": false, "team-b": true, "team-c": true,
	} {
		ids, err := st.ServersForProfile(profileIDs[name])
		if err != nil {
			t.Fatal(err)
		}
		granted := len(ids) == 1 && ids[0] == server
		if granted != wantGranted {
			t.Fatalf("%s 授权结果不正确：%v", name, ids)
		}
	}

	rows, err := st.QueryAudit(store.AuditFilter{
		Source:  store.AuditSourceWebUI,
		Actions: []string{"profile.grant", "profile.revoke"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("应逐个 profile 写授权与撤销审计，实际 %d 行：%+v", len(rows), rows)
	}
	commands := make([]string, 0, len(rows))
	for _, row := range rows {
		commands = append(commands, row.Command)
		if row.SourceIP != "192.0.2.1" {
			t.Fatalf("审计来源地址不正确：%+v", row)
		}
	}
	joined := strings.Join(commands, "\n")
	for _, want := range []string{
		`"profile":"team-b"`, `"profile":"team-c"`, `"profile":"team-a"`,
		`"servers":"1台:web"`,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("审计摘要缺少 %s：%s", want, joined)
		}
	}
}

// TestServerDetailProfileAuthorizationNoChangeAndConflict 验证无变化不写
// 数据或审计；其他入口改过同一台服务器的 profile 集合时，过期确认整体
// 拒绝并按最新集合重新确认。
func TestServerDetailProfileAuthorizationNoChangeAndConflict(t *testing.T) {
	key, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	server, err := st.AddServer(&models.Server{
		Name: "web", Host: "192.0.2.21", Port: 22, User: "deploy",
	})
	if err != nil {
		t.Fatal(err)
	}
	profileA, err := st.AddProfile("team-a")
	if err != nil {
		t.Fatal(err)
	}
	profileB, err := st.AddProfile("team-b")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileA, []string{server}); err != nil {
		t.Fatal(err)
	}
	handler, cookie := newAuthorizationTestHandler(t, st)
	csrf := authorizationCSRF(t, handler, cookie)

	noChange := authorizationRequest(handler, cookie, http.MethodPost, "/ui/servers/profile-authorization/preview", url.Values{
		"csrf_token": {csrf}, "server_id": {server}, "profile": {profileA},
	})
	noChangeBody := authorizationBody(t, noChange)
	if noChange.StatusCode != http.StatusOK || !strings.Contains(noChangeBody, "没有变化") ||
		strings.Contains(noChangeBody, "确认保存 profile 授权") {
		t.Fatalf("无变化预览不应提供确认按钮，状态码=%d：\n%s", noChange.StatusCode, noChangeBody)
	}

	preview := authorizationRequest(handler, cookie, http.MethodPost, "/ui/servers/profile-authorization/preview", url.Values{
		"csrf_token": {csrf}, "server_id": {server}, "profile": {profileB},
	})
	previewBody := authorizationBody(t, preview)
	summary := authorizationHiddenValues(previewBody, "summary")
	if preview.StatusCode != http.StatusOK || len(summary) != 1 ||
		!strings.Contains(previewBody, "team-a") || !strings.Contains(previewBody, "team-b") {
		t.Fatalf("准备冲突预览失败，状态码=%d：%v\n%s", preview.StatusCode, summary, previewBody)
	}
	if _, err := access.New(st).GrantServers(profileB, []string{"web"}, access.CLISource()); err != nil {
		t.Fatal(err)
	}

	confirmForm := url.Values{
		"csrf_token": {csrf}, "server_id": {server}, "summary": summary,
		"profile": {profileB},
	}
	conflict := authorizationRequest(handler, cookie, http.MethodPost, "/ui/servers/profile-authorization/confirm", confirmForm)
	conflictBody := authorizationBody(t, conflict)
	if conflict.StatusCode != http.StatusConflict ||
		!strings.Contains(conflictBody, "profile 授权已被其他入口修改") ||
		!strings.Contains(conflictBody, "当前授予的 profile") ||
		!strings.Contains(conflictBody, "team-a") || !strings.Contains(conflictBody, "team-b") {
		t.Fatalf("过期摘要应整体拒绝并显示最新集合，状态码=%d：\n%s", conflict.StatusCode, conflictBody)
	}
	rows, err := st.QueryAudit(store.AuditFilter{
		Source: store.AuditSourceWebUI, Actions: []string{"profile.grant", "profile.revoke"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("无变化与冲突提交都不应写 Web UI 审计：%+v", rows)
	}

	latestSummary := authorizationHiddenValues(conflictBody, "summary")
	if len(latestSummary) != 1 {
		t.Fatalf("冲突页应携带最新确认摘要：%v\n%s", latestSummary, conflictBody)
	}
	confirmForm.Set("summary", latestSummary[0])
	confirmed := authorizationRequest(handler, cookie, http.MethodPost, "/ui/servers/profile-authorization/confirm", confirmForm)
	confirmedBody := authorizationBody(t, confirmed)
	if confirmed.StatusCode != http.StatusOK || !strings.Contains(confirmedBody, "已保存 profile 授权") {
		t.Fatalf("按最新摘要重新确认失败，状态码=%d：\n%s", confirmed.StatusCode, confirmedBody)
	}
	for profileID, wantGranted := range map[string]bool{profileA: false, profileB: true} {
		ids, err := st.ServersForProfile(profileID)
		if err != nil {
			t.Fatal(err)
		}
		granted := len(ids) == 1 && ids[0] == server
		if granted != wantGranted {
			t.Fatalf("重新确认后授权不正确：%v", ids)
		}
	}
}

// TestAuthorityServerProfileEditDoesNotRequireRecentPasswordVerification 验证
// 权威端调整 profile 授权不需要重新输入管理员口令。
func TestAuthorityServerProfileEditDoesNotRequireRecentPasswordVerification(t *testing.T) {
	key, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	server, err := st.AddServer(&models.Server{
		Name: "web", Host: "192.0.2.22", Port: 22, User: "deploy",
	})
	if err != nil {
		t.Fatal(err)
	}
	profileID, err := st.AddProfile("team")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	authStore := vault.NewWebUIAuthStore(st)
	if _, err := authStore.SetAdminPasswordWithAudit([]byte("correct-password"), webui.AuditEvent{
		Action: "admin.password_change", Status: "ok",
	}); err != nil {
		t.Fatal(err)
	}
	handler, _, err := webui.NewAuthority(webui.Config{
		Role: webui.RoleServer, Port: 18086,
		Servers:                FromManager(servermanage.New(st)),
		Access:                 webuiaccess.FromManager(access.New(st)),
		Auth:                   authStore,
		AllowedHosts:           []string{"127.0.0.1"},
		CertificateFingerprint: "sha256:0123456789abcdef",
		Now:                    func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, target string, form url.Values, cookie *http.Cookie) *http.Response {
		requestBody := strings.NewReader("")
		if form != nil {
			requestBody = strings.NewReader(form.Encode())
		}
		request := httptest.NewRequest(method, target, requestBody)
		request.Host = "127.0.0.1:18086"
		request.RemoteAddr = "198.51.100.10:45678"
		if cookie != nil {
			request.AddCookie(cookie)
		}
		if form != nil {
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request.Header.Set("Origin", "https://127.0.0.1:18086")
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder.Result()
	}

	loginPage := do(http.MethodGet, "/ui/login", nil, nil)
	loginBody := authorizationBody(t, loginPage)
	loginCSRF := authorizationHiddenOne(loginBody, "csrf_token")
	var preloginCookie *http.Cookie
	for _, cookie := range loginPage.Cookies() {
		preloginCookie = cookie
	}
	loggedIn := do(http.MethodPost, "/ui/login", url.Values{
		"csrf_token": {loginCSRF}, "password": {"correct-password"},
	}, preloginCookie)
	_ = loggedIn.Body.Close()
	var session *http.Cookie
	for _, cookie := range loggedIn.Cookies() {
		if cookie.Name == "__Host-sshmgr_ui" {
			session = cookie
		}
	}
	if loggedIn.StatusCode != http.StatusSeeOther || session == nil {
		t.Fatalf("权威端登录失败，状态码=%d cookie=%v", loggedIn.StatusCode, loggedIn.Cookies())
	}

	detail := do(http.MethodGet, "/ui/servers/"+server, nil, session)
	detailBody := authorizationBody(t, detail)
	csrf := authorizationCSRFValue(detailBody)
	now = now.Add(6 * time.Minute)
	preview := do(http.MethodPost, "/ui/servers/profile-authorization/preview", url.Values{
		"csrf_token": {csrf}, "server_id": {server}, "profile": {profileID},
	}, session)
	previewBody := authorizationBody(t, preview)
	summary := authorizationHiddenValues(previewBody, "summary")
	if preview.StatusCode != http.StatusOK || len(summary) != 1 {
		t.Fatalf("权威端生成 profile 授权预览失败，状态码=%d：%v\n%s", preview.StatusCode, summary, previewBody)
	}
	confirmed := do(http.MethodPost, "/ui/servers/profile-authorization/confirm", url.Values{
		"csrf_token": {csrf}, "server_id": {server}, "summary": summary,
		"profile": {profileID},
	}, session)
	confirmedBody := authorizationBody(t, confirmed)
	if confirmed.StatusCode != http.StatusOK || !strings.Contains(confirmedBody, "已保存 profile 授权") {
		t.Fatalf("超过重新验证窗口后仍应直接确认 profile 授权，状态码=%d：\n%s", confirmed.StatusCode, confirmedBody)
	}
	ids, err := st.ServersForProfile(profileID)
	if err != nil || len(ids) != 1 || ids[0] != server {
		t.Fatalf("权威端确认后授权不正确：%v（错误 %v）", ids, err)
	}
}

// TestAuthorityServerShareEditRequiresRecentPasswordVerification 验证权威端
// 确认共享存储授权前必须重新输入管理员口令，回跳保留待确认集合且不包含值。
func TestAuthorityServerShareEditRequiresRecentPasswordVerification(t *testing.T) {
	key, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.AddDevice("storage", "共享存储提供方", ""); err != nil {
		t.Fatal(err)
	}
	server, err := st.AddServer(&models.Server{
		Name: "web", Host: "192.0.2.10", Port: 22, User: "deploy",
	})
	if err != nil {
		t.Fatal(err)
	}
	secrets := secretmanage.New(st)
	if err := secrets.AddSecret("DATASET_PASSWORD", "数据集挂载密码", "", []byte("dataset-secret-value"), secretmanage.CLISource()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSharedStorage(store.SharedStorageInput{
		Name: "datasets", Device: "storage", User: "mount-user",
		PasswordSecret: "DATASET_PASSWORD", Drive: "Z",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	views, err := secrets.List(secretmanage.ListFilter{})
	if err != nil || len(views) != 1 {
		t.Fatalf("准备机密项失败：%v，条目=%d", err, len(views))
	}
	secretID := views[0].ID
	now := time.Now()
	authStore := vault.NewWebUIAuthStore(st)
	if _, err := authStore.SetAdminPasswordWithAudit([]byte("correct-password"), webui.AuditEvent{
		Action: "admin.password_change", Status: "ok",
	}); err != nil {
		t.Fatal(err)
	}
	handler, _, err := webui.NewAuthority(webui.Config{
		Role: webui.RoleServer, Port: 18085,
		Servers:                FromManager(servermanage.New(st)),
		ServerWriter:           WriterFromManager(servermanage.New(st)),
		Access:                 webuiaccess.FromManager(access.New(st)),
		Secrets:                webuisecrets.FromManager(secretmanage.New(st)),
		SecretsWriter:          webuisecrets.WriterFromManager(secretmanage.New(st)),
		Auth:                   authStore,
		AllowedHosts:           []string{"127.0.0.1"},
		CertificateFingerprint: "sha256:0123456789abcdef",
		Now:                    func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, target string, form url.Values, cookie *http.Cookie) *http.Response {
		requestBody := strings.NewReader("")
		if form != nil {
			requestBody = strings.NewReader(form.Encode())
		}
		request := httptest.NewRequest(method, target, requestBody)
		request.Host = "127.0.0.1:18085"
		request.RemoteAddr = "198.51.100.9:45678"
		if cookie != nil {
			request.AddCookie(cookie)
		}
		if form != nil {
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request.Header.Set("Origin", "https://127.0.0.1:18085")
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder.Result()
	}
	readBody := func(response *http.Response) string {
		data, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		return string(data)
	}

	loginPage := do(http.MethodGet, "/ui/login", nil, nil)
	loginBody := readBody(loginPage)
	loginCSRF := authorizationHiddenOne(loginBody, "csrf_token")
	var preloginCookie *http.Cookie
	for _, cookie := range loginPage.Cookies() {
		preloginCookie = cookie
	}
	loginForm := url.Values{"csrf_token": {loginCSRF}, "password": {"correct-password"}}
	loggedIn := do(http.MethodPost, "/ui/login", loginForm, preloginCookie)
	_ = loggedIn.Body.Close()
	var session *http.Cookie
	for _, cookie := range loggedIn.Cookies() {
		if cookie.Name == "__Host-sshmgr_ui" {
			session = cookie
		}
	}
	if loggedIn.StatusCode != http.StatusSeeOther || session == nil {
		t.Fatalf("权威端登录失败，状态码=%d cookie=%v", loggedIn.StatusCode, loggedIn.Cookies())
	}

	secretsPage := do(http.MethodGet, "/ui/secrets/", nil, session)
	csrf := authorizationCSRFValue(readBody(secretsPage))
	now = now.Add(6 * time.Minute)
	preview := do(http.MethodPost, "/ui/secrets/authorization/preview", url.Values{
		"csrf_token": {csrf}, "return_to": {"/ui/servers/" + server},
		"authorization_view": {"server-shares"}, "server_id": {server}, "secret_id": {secretID},
		"cell__" + secretID + "__" + server: {"1"},
	}, session)
	previewBody := readBody(preview)
	summary := authorizationHiddenValues(previewBody, "summary")
	targets := authorizationHiddenValues(previewBody, "target")
	if preview.StatusCode != http.StatusOK || len(summary) != 1 || len(targets) != 1 ||
		!strings.Contains(previewBody, "datasets") {
		t.Fatalf("准备共享存储授权预览失败，状态码=%d：\n%s", preview.StatusCode, previewBody)
	}
	confirmForm := url.Values{
		"csrf_token": {csrf}, "summary": summary, "return_to": {"/ui/servers/" + server},
		"authorization_view": {"server-shares"}, "server_id": {server},
	}
	for _, target := range targets {
		confirmForm.Add("target", target)
	}
	rejected := do(http.MethodPost, "/ui/secrets/authorization/confirm", confirmForm, session)
	rejectedBody := readBody(rejected)
	if rejected.StatusCode != http.StatusSeeOther || !strings.HasPrefix(rejected.Header.Get("Location"), "/ui/reverify?next=") {
		t.Fatalf("过期验证应跳转重新验证，状态码=%d 跳转=%q 页面：\n%s",
			rejected.StatusCode, rejected.Header.Get("Location"), rejectedBody)
	}
	redirect, err := url.Parse(rejected.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	next := redirect.Query().Get("next")
	resume, err := url.Parse(next)
	if err != nil || resume.Path != "/ui/secrets/authorization/preview" ||
		resume.Query().Get("authorization_view") != "server-shares" ||
		resume.Query().Get("server_id") != server ||
		len(resume.Query()["target"]) != 1 ||
		strings.Contains(next, "dataset-secret-value") {
		t.Fatalf("重新验证回跳应保留目标集合且不含值：%v %v", next, err)
	}
	rows, err := st.QueryAudit(store.AuditFilter{Source: store.AuditSourceWebUI, Actions: []string{"secret-allow-change"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("重新验证前不应写授权审计：%+v", rows)
	}

	reverifyPage := do(http.MethodGet, redirect.String(), nil, session)
	reverifyCSRF := authorizationHiddenOne(readBody(reverifyPage), "csrf_token")
	verified := do(http.MethodPost, "/ui/reverify", url.Values{
		"csrf_token": {reverifyCSRF}, "password": {"correct-password"}, "next": {next},
	}, session)
	_ = verified.Body.Close()
	if verified.StatusCode != http.StatusSeeOther || verified.Header.Get("Location") != next {
		t.Fatalf("重新验证后应回到预览，状态码=%d 跳转=%q", verified.StatusCode, verified.Header.Get("Location"))
	}
	resumed := do(http.MethodGet, next, nil, session)
	resumedBody := readBody(resumed)
	if resumed.StatusCode != http.StatusOK || authorizationHiddenValues(resumedBody, "summary")[0] != summary[0] ||
		!strings.Contains(resumedBody, "datasets") {
		t.Fatalf("重新验证后的预览不正确，状态码=%d：\n%s", resumed.StatusCode, resumedBody)
	}
	confirmed := do(http.MethodPost, "/ui/secrets/authorization/confirm", confirmForm, session)
	confirmedBody := readBody(confirmed)
	if confirmed.StatusCode != http.StatusOK || !strings.Contains(confirmedBody, "已保存授权变更") {
		t.Fatalf("重新验证后确认失败，状态码=%d：\n%s", confirmed.StatusCode, confirmedBody)
	}
}

type authorizationClientServerSource struct {
	detail *webui.ServerDetailView
}

func (s authorizationClientServerSource) Servers(string) ([]webui.ServerRow, error) {
	return []webui.ServerRow{{ID: s.detail.ID, Name: s.detail.Name}}, nil
}

func (s authorizationClientServerSource) ServerList(string, bool) ([]webui.ServerListEntry, error) {
	return []webui.ServerListEntry{{ID: s.detail.ID, Name: s.detail.Name}}, nil
}

func (s authorizationClientServerSource) ServerDetail(string, string) (*webui.ServerDetailView, error) {
	return s.detail, nil
}

// TestClientServerDetailHasNoReverseAuthorizationWriteEntrance 验证客户端
// 详情页保持只读，反向授权预览与确认路由也不存在。
func TestClientServerDetailHasNoReverseAuthorizationWriteEntrance(t *testing.T) {
	detail := &webui.ServerDetailView{
		ID: "srv-web", Name: "web", User: "deploy", Host: "192.0.2.10", Port: 22,
		Secrets: []webui.ServerDetailSecret{{
			ID: "secret-api", Name: "API_TOKEN", Note: "接口令牌", EnvironmentName: "API_TOKEN_FILE",
		}},
		SharedStorages: []webui.ServerDetailSharedStorage{{
			ID: "share-datasets", Name: "datasets", ProviderDeviceName: "NAS", MountUser: "mount-user",
			PasswordSecretID: "secret-share", PasswordSecretName: "NAS_MOUNT_PASSWORD", MountPoint: "/mnt/sshmgr/datasets",
		}},
		SiblingSharedStorages: []webui.ServerDetailSiblingSharedStorage{{
			ServerDetailSharedStorage: webui.ServerDetailSharedStorage{
				ID: "share-worker", Name: "worker-data", MountPoint: "/mnt/sshmgr/worker-data",
			},
			SourceServerID:   "srv-worker",
			SourceServerName: "worker",
		}},
	}
	handler, startup, err := webui.New(webui.Config{
		Role: webui.RoleClient, Port: 18084,
		Servers: authorizationClientServerSource{detail: detail},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, startup.URL("127.0.0.1:18084"), nil)
	request.Host = "127.0.0.1:18084"
	request.RemoteAddr = "127.0.0.1:12345"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 {
		response := recorder.Result()
		data, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		t.Fatalf("应签发客户端会话 cookie，实际 %d，状态码=%d 页面：\n%s",
			len(cookies), response.StatusCode, data)
	}
	client := authorizationRequest(handler, cookies[0], http.MethodGet, "/ui/servers/srv-web", nil)
	page := authorizationBody(t, client)
	if client.StatusCode != http.StatusOK ||
		!strings.Contains(page, "仅显示本客户端可用的部分，修改请到权威端。") ||
		!strings.Contains(page, "接口令牌") ||
		strings.Contains(page, "authorization/preview") ||
		strings.Contains(page, "预览机密项授权变更") ||
		strings.Contains(page, "预览共享存储授权变更") ||
		strings.Contains(page, "预览 profile 授权变更") ||
		strings.Contains(page, "profile-authorization") ||
		strings.Contains(page, "环境变量名") ||
		strings.Contains(page, "profile 授权") ||
		strings.Contains(page, "下发记录") ||
		strings.Contains(page, "同设备其他服务器条目带来的下发文件") ||
		strings.Contains(page, "/ui/servers/srv-web/edit") ||
		strings.Contains(page, "/ui/servers/srv-web/delete") ||
		!strings.Contains(page, "API_TOKEN") ||
		!strings.Contains(page, "datasets") ||
		!strings.Contains(page, "NAS") ||
		!strings.Contains(page, "mount-user") ||
		!strings.Contains(page, "/mnt/sshmgr/datasets") ||
		!strings.Contains(page, "同设备其他服务器条目带来的共享存储") ||
		!strings.Contains(page, "worker-data") ||
		!strings.Contains(page, "worker") {
		t.Fatalf("客户端详情应只读展示两组内容且没有写入口，状态码=%d：\n%s", client.StatusCode, page)
	}
	for _, target := range []string{
		"/ui/secrets/authorization/preview", "/ui/secrets/authorization/confirm",
		"/ui/servers/profile-authorization/preview", "/ui/servers/profile-authorization/confirm",
		"/ui/servers/srv-web/edit", "/ui/servers/srv-web/delete",
	} {
		response := authorizationRequest(handler, cookies[0], http.MethodPost, target, url.Values{
			"csrf_token": {authorizationCSRFValue(page)},
		})
		responseBody := authorizationBody(t, response)
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("客户端写路由 %s 不应存在，状态码=%d：\n%s", target, response.StatusCode, responseBody)
		}
	}
}
