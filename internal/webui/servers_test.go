package webui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// stubServerStore 是服务器页测试用的来源桩：列表、详情与写操作都通过同一
// 个接口，页面处理器不接触数据库结构。
type stubServerStore struct {
	rows    []ServerListEntry
	details map[string]*ServerDetailView
	writer  *stubServerWriter
}

func newStubServerStore() *stubServerStore {
	return &stubServerStore{
		rows: []ServerListEntry{
			{ID: "srv-fine", Name: "fine", User: "root", Host: "192.0.2.10", Port: 22},
			{ID: "srv-bare", Name: "bare", User: "root", Host: "192.0.2.11", Port: 22,
				AttentionReasons: []string{"未挂凭据", "未设角色"}},
		},
		details: map[string]*ServerDetailView{
			"srv-fine": {
				ID: "srv-fine", Name: "fine", User: "root", Host: "192.0.2.10", Port: 22,
				Shell: "posix", CredentialType: "password", HasSudoPassword: true,
				Role: "完整角色", Tags: []string{"gpu"}, ExposeHost: true, Revision: 4,
			},
			"srv-web": {
				ID: "srv-web", Name: "web", User: "deploy", Host: "192.0.2.20", Port: 22,
				Shell: "posix", Revision: 1, AttentionReasons: []string{"未挂凭据", "未设角色"},
			},
			"srv-bare": {
				ID: "srv-bare", Name: "bare", User: "root", Host: "192.0.2.11", Port: 22,
				Shell: "posix", Revision: 1, AttentionReasons: []string{"未挂凭据", "未设角色"},
			},
		},
	}
}

func (s *stubServerStore) Servers() ([]ServerRow, error) {
	rows := make([]ServerRow, 0, len(s.rows))
	for _, row := range s.rows {
		rows = append(rows, ServerRow{ID: row.ID, Name: row.Name, User: row.User, Host: row.Host, Port: row.Port})
	}
	return rows, nil
}

func (s *stubServerStore) ServerList(attentionOnly bool) ([]ServerListEntry, error) {
	if !attentionOnly {
		return s.rows, nil
	}
	var out []ServerListEntry
	for _, row := range s.rows {
		if len(row.AttentionReasons) > 0 {
			out = append(out, row)
		}
	}
	return out, nil
}

func (s *stubServerStore) ServerDetail(identifier string) (*ServerDetailView, error) {
	return s.details[identifier], nil
}

type stubServerWriter struct {
	edits        []ServerEditForm
	deletes      []string
	importRuns   []stubImportRun
	completes    []stubComplete
	conflict     *ServerEditConflictView
	plan         *ServerImportPlanView
	run          *ServerImportRunView
	completeErr  error
	deleteDigest string
}

type stubImportRun struct {
	path     string
	selected []string
	profile  string
	source   ServerWriteSource
}

type stubComplete struct {
	id     string
	form   ServerCompleteForm
	source ServerWriteSource
}

func (w *stubServerWriter) EditServer(id string, form ServerEditForm, source ServerWriteSource) (*ServerEditResultView, *ServerEditConflictView, error) {
	w.edits = append(w.edits, form)
	if w.conflict != nil {
		return nil, w.conflict, nil
	}
	return &ServerEditResultView{Detail: &ServerDetailView{ID: id, Name: form.NameValue()}}, nil, nil
}

func (w *stubServerWriter) DeleteServerPreview(identifier string) (*ServerDeletePreviewView, error) {
	return &ServerDeletePreviewView{
		ServerID: "srv-fine", ServerName: "fine", HasDevice: true, DeviceName: "fine",
		AnchorCount: 1, ProfileNames: []string{"运维"}, SecretNames: []string{"deploy_note"},
		DeletedCredentials: []string{"登录凭据（密码）"},
		KeptCredentials:    []string{"sudo 凭据（密码）（仍被 1 台服务器引用）"},
		Digest:             "digest-1",
	}, nil
}

func (w *stubServerWriter) DeleteServer(identifier, digest string, source ServerWriteSource) error {
	w.deletes = append(w.deletes, identifier)
	w.deleteDigest = digest
	return nil
}

func (w *stubServerWriter) ImportPlan(path, profile string) (*ServerImportPlanView, error) {
	if w.plan != nil {
		return w.plan, nil
	}
	return &ServerImportPlanView{
		Path: path, ProfileID: profile,
		Candidates: []ServerImportCandidateView{
			{Name: "web", User: "deploy", Host: "192.0.2.20", Port: 22, Note: "私钥"},
			{Name: "bare", User: "deploy", Host: "192.0.2.21", Port: 22, Note: "无凭据"},
		},
		VaultSkipped: []ServerImportSkipView{{Name: "taken", Reason: "同名冲突"}},
		Profiles:     []ServerProfileOption{{ID: "profile-1", Name: "运维"}},
	}, nil
}

func (w *stubServerWriter) ImportRun(path string, selected []string, profile string, source ServerWriteSource) (*ServerImportRunView, error) {
	w.importRuns = append(w.importRuns, stubImportRun{path: path, selected: selected, profile: profile, source: source})
	if w.run != nil {
		return w.run, nil
	}
	return &ServerImportRunView{
		Imported: 1, Skipped: 1, Failed: 0, Pending: 1,
		Entries: []ServerImportEntryView{
			{Name: "web", Status: "已导入（私钥）"},
			{Name: "taken", Status: "跳过：同名冲突"},
		},
		ImportedIDs: []string{"srv-web"},
	}, nil
}

func (w *stubServerWriter) CompleteServer(id string, form ServerCompleteForm, source ServerWriteSource) (*ServerCompleteResultView, error) {
	w.completes = append(w.completes, stubComplete{id: id, form: form, source: source})
	if w.completeErr != nil {
		return nil, w.completeErr
	}
	return &ServerCompleteResultView{Detail: &ServerDetailView{ID: id, Name: "web", Role: form.RoleValue()}}, nil
}

// newServersTestHandler 构造带可写来源的单机 Web UI，并完成启动令牌换会话。
func newServersTestHandler(t *testing.T, role Role, store ServerSource, writer ServerWriter) (http.Handler, *http.Cookie) {
	t.Helper()
	h, startup, err := New(Config{
		Role:         role,
		Servers:      store,
		ServerWriter: writer,
		Access:       &stubAccessStore{},
		VersionHint:  func() string { return "" },
		Port:         18080,
	})
	if err != nil {
		t.Fatalf("构造 Web UI Handler 失败：%v", err)
	}
	resp := exchangeStartupToken(t, h, startup.URL("127.0.0.1:18080"))
	cookies := resp.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("应签发会话 cookie，实际 %d", len(cookies))
	}
	return h, cookies[0]
}

func serversGetStatus(t *testing.T, h http.Handler, cookie *http.Cookie, target string) (string, int) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = "127.0.0.1:18080"
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := readResponseBody(t, rec.Result())
	return body, rec.Code
}

func serversGet(t *testing.T, h http.Handler, cookie *http.Cookie, target string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = "127.0.0.1:18080"
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := readResponseBody(t, rec.Result())
	return body
}

func serversPost(t *testing.T, h http.Handler, cookie *http.Cookie, target string, form url.Values) *http.Response {
	t.Helper()
	csrf := serversCSRF(t, h, cookie)
	form.Set("csrf_token", csrf)
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Host = "127.0.0.1:18080"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:18080")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

func serversCSRF(t *testing.T, h http.Handler, cookie *http.Cookie) string {
	t.Helper()
	body := serversGet(t, h, cookie, "/ui/")
	match := regexp.MustCompile(`<meta name="csrf-token" content="([^"]+)"`).FindStringSubmatch(body)
	if len(match) != 2 {
		t.Fatalf("列表页应携带防跨站请求伪造令牌：\n%s", body)
	}
	return match[1]
}

// TestServersListFilterAndDetail 覆盖过滤视图、需要注意置顶与详情页；详情
// 不显示凭据内容。
func TestServersListFilterAndDetail(t *testing.T) {
	store := newStubServerStore()
	h, cookie := newServersTestHandler(t, RoleStandalone, store, &stubServerWriter{})

	list := serversGet(t, h, cookie, "/ui/")
	for _, want := range []string{"fine", "bare", "未挂凭据", "未设角色", "只看需要注意"} {
		if !strings.Contains(list, want) {
			t.Fatalf("服务器列表缺少 %q：\n%s", want, list)
		}
	}
	if strings.Index(list, ">bare<") > strings.Index(list, ">fine<") {
		t.Fatalf("需要注意的服务器应排在最前：\n%s", list)
	}

	filtered := serversGet(t, h, cookie, "/ui/?attention=1")
	if strings.Contains(filtered, "192.0.2.10") || !strings.Contains(filtered, "bare") {
		t.Fatalf("过滤视图应只显示需要注意的服务器：\n%s", filtered)
	}
	if !strings.Contains(filtered, "bare") {
		t.Fatalf("过滤视图应显示需要注意的服务器：\n%s", filtered)
	}

	detail := serversGet(t, h, cookie, "/ui/servers/srv-fine")
	for _, want := range []string{"fine", "root@192.0.2.10:22", "密码", "已设 sudo 密码", "完整角色", "gpu", "向 agent 展示主机地址"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("详情页缺少 %q：\n%s", want, detail)
		}
	}
}

// TestServersClientRoleHasNoWriteRoutes 覆盖客户端角色：只有列表、过滤与详
// 情，写路由不存在。
func TestServersClientRoleHasNoWriteRoutes(t *testing.T) {
	store := newStubServerStore()
	h, cookie := newServersTestHandler(t, RoleClient, store, nil)

	list := serversGet(t, h, cookie, "/ui/?attention=1")
	if !strings.Contains(list, "bare") || strings.Contains(list, "编辑") || strings.Contains(list, "删除") || strings.Contains(list, "导入") {
		t.Fatalf("客户端列表不应出现写入口：\n%s", list)
	}
	detail := serversGet(t, h, cookie, "/ui/servers/srv-fine")
	if !strings.Contains(detail, "fine") {
		t.Fatalf("客户端应能查看详情：\n%s", detail)
	}
	if strings.Contains(detail, "编辑") || strings.Contains(detail, "删除") {
		t.Fatalf("客户端详情不应出现写入口：\n%s", detail)
	}
	for _, target := range []string{"/ui/servers/srv-fine/edit", "/ui/servers/srv-fine/delete", "/ui/servers/import", "/ui/servers/complete"} {
		if body, status := serversGetStatus(t, h, cookie, target); status != http.StatusNotFound {
			t.Fatalf("客户端写路由 %s 状态码 = %d，应为 404：%s", target, status, body)
		}
	}
}

// TestServersEditSubmitAndConflict 覆盖编辑表单、提交与修订号冲突后的重新
// 渲染。
func TestServersEditSubmitAndConflict(t *testing.T) {
	store := newStubServerStore()
	writer := &stubServerWriter{}
	h, cookie := newServersTestHandler(t, RoleStandalone, store, writer)

	form := url.Values{
		"server_id": {"srv-fine"}, "name": {"fine"}, "host": {"192.0.2.10"},
		"port": {"22"}, "user": {"root"}, "shell": {"posix"},
		"description": {"新说明"}, "role": {"完整角色"}, "tags": {"gpu"},
		"expose_host": {"1"}, "revision": {"4"},
		"credential_action": {"password"}, "password": {"new-login-secret"},
		"sudo_password": {"sudo-secret"},
	}
	resp := serversPost(t, h, cookie, "/ui/servers/srv-fine/edit", form)
	if resp.StatusCode != http.StatusSeeOther {
		body := readResponseBody(t, resp)
		t.Fatalf("编辑提交状态码 = %d，应为 303：\n%s", resp.StatusCode, body)
	}
	if len(writer.edits) != 1 {
		t.Fatalf("编辑应写入一次，实际 %d", len(writer.edits))
	}
	got := writer.edits[0]
	if got.DescriptionValue() != "新说明" || got.ExpectedRevision != 4 || got.PasswordValue() != "new-login-secret" {
		t.Fatalf("提交的表单不完整：%+v", got)
	}

	writer.conflict = &ServerEditConflictView{Current: store.details["srv-fine"]}
	resp = serversPost(t, h, cookie, "/ui/servers/srv-fine/edit", form)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("修订号冲突状态码 = %d，应为 409", resp.StatusCode)
	}
	body := readResponseBody(t, resp)
	for _, want := range []string{"已被其他入口修改", "完整角色"} {
		if !strings.Contains(body, want) {
			t.Fatalf("冲突页面缺少 %q：\n%s", want, body)
		}
	}
	if strings.Contains(body, "new-login-secret") || strings.Contains(body, "sudo-secret") {
		t.Fatalf("冲突页面不得回显凭据：\n%s", body)
	}
}

// TestServersDeletePreviewConfirmAndCSRF 覆盖删除预览、确认、防跨站请求伪
// 造校验。
func TestServersDeletePreviewConfirmAndCSRF(t *testing.T) {
	store := newStubServerStore()
	writer := &stubServerWriter{}
	h, cookie := newServersTestHandler(t, RoleStandalone, store, writer)

	preview := serversGet(t, h, cookie, "/ui/servers/srv-fine/delete")
	for _, want := range []string{"fine", "设备保留", "运维", "deploy_note", "登录凭据（密码）", "仍被 1 台服务器引用", "确认删除"} {
		if !strings.Contains(preview, want) {
			t.Fatalf("删除预览缺少 %q：\n%s", want, preview)
		}
	}

	resp := serversPost(t, h, cookie, "/ui/servers/srv-fine/delete", url.Values{
		"server_id": {"srv-fine"}, "digest": {"digest-1"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		body := readResponseBody(t, resp)
		t.Fatalf("确认删除状态码 = %d，应为 303：\n%s", resp.StatusCode, body)
	}
	if len(writer.deletes) != 1 || writer.deleteDigest != "digest-1" {
		t.Fatalf("删除应带预览摘要执行：%+v", writer)
	}

	// 缺少防跨站请求伪造令牌的写请求被拒绝。
	req := httptest.NewRequest(http.MethodPost, "/ui/servers/srv-fine/delete", strings.NewReader("server_id=srv-fine&digest=digest-1"))
	req.Host = "127.0.0.1:18080"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:18080")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("缺少令牌的删除状态码 = %d，应为 403", rec.Code)
	}
}

// TestServersImportThreeStepsAndComplete 覆盖导入三步与补全的跳过、继续、
// 结束。
func TestServersImportThreeStepsAndComplete(t *testing.T) {
	store := newStubServerStore()
	writer := &stubServerWriter{}
	h, cookie := newServersTestHandler(t, RoleStandalone, store, writer)

	pathForm := serversGet(t, h, cookie, "/ui/servers/import")
	if !strings.Contains(pathForm, "SSH 配置路径") {
		t.Fatalf("导入第一步应是路径表单：\n%s", pathForm)
	}

	planResp := serversPost(t, h, cookie, "/ui/servers/import/plan", url.Values{
		"path": {"/tmp/config"}, "profile": {"profile-1"},
	})
	planBody := readResponseBody(t, planResp)
	for _, want := range []string{"web", "bare", "taken", "同名冲突", "运维"} {
		if !strings.Contains(planBody, want) {
			t.Fatalf("候选清单缺少 %q：\n%s", want, planBody)
		}
	}

	runResp := serversPost(t, h, cookie, "/ui/servers/import/run", url.Values{
		"path": {"/tmp/config"}, "profile": {"profile-1"}, "candidate": {"web"},
	})
	if runResp.StatusCode != http.StatusOK {
		t.Fatalf("导入执行状态码 = %d", runResp.StatusCode)
	}
	runBody := readResponseBody(t, runResp)
	for _, want := range []string{"导入 1", "跳过 1", "失败 0", "待补 1", "开始逐台补全"} {
		if !strings.Contains(runBody, want) {
			t.Fatalf("导入结果缺少 %q：\n%s", want, runBody)
		}
	}

	complete := serversGet(t, h, cookie, "/ui/servers/complete?queue=srv-web&position=0")
	for _, want := range []string{"web", "说明", "sudo 密码", "密码（现在设置）", "跳过这一台", "结束补全"} {
		if !strings.Contains(complete, want) {
			t.Fatalf("补全表单缺少 %q：\n%s", want, complete)
		}
	}

	skip := serversPost(t, h, cookie, "/ui/servers/complete", url.Values{
		"action": {"skip"}, "queue": {"srv-web,srv-bare"}, "position": {"0"},
	})
	if loc := skip.Header.Get("Location"); loc != "/ui/servers/complete?failed=0&imported=0&position=1&queue=srv-web%2Csrv-bare&skipped=0" {
		t.Fatalf("跳过应推进到下一台，实际跳转 %q", loc)
	}

	saved := serversPost(t, h, cookie, "/ui/servers/complete", url.Values{
		"action": {"save"}, "queue": {"srv-web"}, "position": {"0"},
		"role": {"网站"}, "password": {"login-pass"},
	})
	if saved.StatusCode != http.StatusSeeOther {
		body := readResponseBody(t, saved)
		t.Fatalf("保存补全状态码 = %d：\n%s", saved.StatusCode, body)
	}
	if len(writer.completes) != 1 || writer.completes[0].form.RoleValue() != "网站" || writer.completes[0].form.Password != "login-pass" {
		t.Fatalf("补全提交不完整：%+v", writer.completes)
	}

	ended := serversPost(t, h, cookie, "/ui/servers/complete", url.Values{
		"action": {"end"}, "queue": {"srv-web"}, "position": {"0"}, "imported": {"1"}, "skipped": {"1"}, "failed": {"0"},
	})
	if ended.StatusCode != http.StatusSeeOther {
		t.Fatalf("结束补全状态码 = %d", ended.StatusCode)
	}
	result := serversGet(t, h, cookie, ended.Header.Get("Location"))
	for _, want := range []string{"导入 1", "跳过 1", "失败 0", "待补"} {
		if !strings.Contains(result, want) {
			t.Fatalf("补全结果页缺少 %q：\n%s", want, result)
		}
	}
}

// TestAuthorityServerCredentialWritesNeedRecentVerify 覆盖权威端守卫：未验
// 证时凭据写入被拒且不回显；验证后通过；单机不要求重新验证；删除执行同
// 样要求验证。
func TestAuthorityServerCredentialWritesNeedRecentVerify(t *testing.T) {
	store := newStubServerStore()
	writer := &stubServerWriter{}
	auth := &fakeAuthorityAuth{passwordSet: true, password: "correct-password"}
	now := time.Unix(1800000000, 0)
	handler, _, err := NewAuthority(Config{
		Role: RoleServer, Servers: store, ServerWriter: writer, Access: &stubAccessStore{}, Auth: auth,
		CertificateFingerprint: "sha256:0123456789abcdef",
		AllowedHosts:           []string{"127.0.0.1"}, Port: 7879,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie := authoritySuccessfulLogin(t, handler, "127.0.0.1:7879", "correct-password")
	now = now.Add(6 * time.Minute)

	// 超过 5 分钟未重新验证：编辑页凭据区域提示重新验证；提交被拒且不回显。
	page := authorityGet(t, handler, "127.0.0.1:7879", "/ui/servers/srv-fine/edit", cookie)
	body := readResponseBody(t, page)
	if !strings.Contains(body, "重新验证后才能填写") {
		t.Fatalf("未验证的编辑页应提示重新验证：\n%s", body)
	}

	resp := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/servers/srv-fine/edit", url.Values{
		"csrf_token": {hiddenInputValue(t, body, "csrf_token")},
		"server_id":  {"srv-fine"}, "name": {"fine"}, "host": {"192.0.2.10"}, "port": {"22"},
		"user": {"root"}, "shell": {"posix"}, "revision": {"4"},
		"credential_action": {"password"}, "password": {"authority-secret"},
	}, cookie)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("未验证的凭据提交状态码 = %d，应为 303 跳转重新验证", resp.StatusCode)
	}
	next := resp.Header.Get("Location")
	if !strings.Contains(next, "/ui/reverify") || strings.Contains(next, "authority-secret") {
		t.Fatalf("未验证提交应跳转重新验证且不得带凭据：%s", next)
	}

	// 重新验证后回到表单，再次提交通过。
	reverify := authorityGet(t, handler, "127.0.0.1:7879", next, cookie)
	reverifyBody := readResponseBody(t, reverify)
	verified := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/reverify", url.Values{
		"csrf_token": {hiddenInputValue(t, reverifyBody, "csrf_token")},
		"password":   {"correct-password"}, "next": {"/ui/servers/srv-fine/edit"},
	}, cookie)
	if verified.StatusCode != http.StatusSeeOther {
		t.Fatalf("重新验证状态码 = %d", verified.StatusCode)
	}
	after := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/servers/srv-fine/edit", url.Values{
		"csrf_token": {hiddenInputValue(t, reverifyBody, "csrf_token")},
		"server_id":  {"srv-fine"}, "name": {"fine"}, "host": {"192.0.2.10"}, "port": {"22"},
		"user": {"root"}, "shell": {"posix"}, "revision": {"4"},
		"credential_action": {"password"}, "password": {"authority-secret"},
	}, cookie)
	if after.StatusCode != http.StatusSeeOther || after.Header.Get("Location") != "/ui/servers/srv-fine" {
		t.Fatalf("验证后的凭据提交应通过，状态码=%d 跳转=%s", after.StatusCode, after.Header.Get("Location"))
	}
	if len(writer.edits) != 1 || writer.edits[0].PasswordValue() != "authority-secret" {
		t.Fatalf("验证后的提交应写入：%+v", writer.edits)
	}

	// 超过 5 分钟后删除执行被拒；重新验证后通过。
	now = now.Add(6 * time.Minute)
	deleteResp := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/servers/srv-fine/delete", url.Values{
		"csrf_token": {hiddenInputValue(t, reverifyBody, "csrf_token")},
		"server_id":  {"srv-fine"}, "digest": {"digest-1"},
	}, cookie)
	if deleteResp.StatusCode != http.StatusSeeOther || !strings.Contains(deleteResp.Header.Get("Location"), "/ui/reverify") {
		t.Fatalf("验证过期的删除应跳转重新验证，状态码=%d 跳转=%s", deleteResp.StatusCode, deleteResp.Header.Get("Location"))
	}
	if len(writer.deletes) != 0 {
		t.Fatal("验证过期时不得执行删除")
	}
}

// TestStandaloneServerWritesNeedNoReverify 覆盖单机回环：凭据写入与删除不
// 要求重新验证。
func TestStandaloneServerWritesNeedNoReverify(t *testing.T) {
	store := newStubServerStore()
	writer := &stubServerWriter{}
	h, cookie := newServersTestHandler(t, RoleStandalone, store, writer)

	page := serversGet(t, h, cookie, "/ui/servers/srv-fine/edit")
	if strings.Contains(page, "重新验证后才能填写") {
		t.Fatalf("单机不应要求重新验证：\n%s", page)
	}
	resp := serversPost(t, h, cookie, "/ui/servers/srv-fine/edit", url.Values{
		"server_id": {"srv-fine"}, "name": {"fine"}, "host": {"192.0.2.10"}, "port": {"22"},
		"user": {"root"}, "shell": {"posix"}, "revision": {"4"},
		"credential_action": {"password"}, "password": {"loopback-secret"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("单机凭据写入状态码 = %d，应为 303", resp.StatusCode)
	}
	del := serversPost(t, h, cookie, "/ui/servers/srv-fine/delete", url.Values{
		"server_id": {"srv-fine"}, "digest": {"digest-1"},
	})
	if del.StatusCode != http.StatusSeeOther {
		t.Fatalf("单机删除状态码 = %d，应为 303", del.StatusCode)
	}
}
