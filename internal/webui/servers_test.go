package webui

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
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

func (s *stubServerStore) Servers(_ string) ([]ServerRow, error) {
	rows := make([]ServerRow, 0, len(s.rows))
	for _, row := range s.rows {
		rows = append(rows, ServerRow{ID: row.ID, Name: row.Name, User: row.User, Host: row.Host, Port: row.Port})
	}
	return rows, nil
}

func (s *stubServerStore) ServerList(_ string, attentionOnly bool) ([]ServerListEntry, error) {
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

func (s *stubServerStore) ServerDetail(_ string, identifier string) (*ServerDetailView, error) {
	return s.details[identifier], nil
}

// TestServerDetailShowsReverseReadGroups 校验详情页三组反向授权、空状态与
// 同设备共享存储提示；页面只显示名字和备注，不显示机密项值。
func TestServerDetailShowsReverseReadGroups(t *testing.T) {
	store := newStubServerStore()
	store.details["srv-fine"].Secrets = []ServerDetailSecret{{
		Name: "API_TOKEN", Note: "接口令牌", EnvironmentName: "API_TOKEN",
	}}
	store.details["srv-fine"].SharedStorages = []ServerDetailSharedStorage{{
		Name: "datasets", ProviderDeviceName: "NAS", MountUser: "mount-user",
		PasswordSecretName: "DATASET_PASSWORD", Drive: "Z", MountPoint: "/mnt/sshmgr/datasets",
	}}
	store.details["srv-fine"].Profiles = []ServerDetailProfile{{Name: "运维"}}
	store.details["srv-fine"].SiblingSharedStorages = []ServerDetailSiblingSharedStorage{{
		ServerDetailSharedStorage: ServerDetailSharedStorage{
			Name: "worker-data", ProviderDeviceName: "NAS", MountUser: "mount-user",
			PasswordSecretName: "WORKER_PASSWORD", Drive: "X", MountPoint: "X:",
		},
		SourceServerName: "worker",
	}}
	handler, cookie := newServersTestHandler(t, RoleStandalone, store, &stubServerWriter{})

	page := serversGet(t, handler, cookie, "/ui/servers/srv-fine")
	for _, want := range []string{
		"机密项", "API_TOKEN", "接口令牌", "环境变量名",
		"共享存储", "datasets", "NAS", "mount-user", "/mnt/sshmgr/datasets",
		"profile 授权", "运维",
		"同设备其他服务器条目带来的共享存储", "worker-data", "来源服务器条目", "worker",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("服务器详情页缺少 %q：\n%s", want, page)
		}
	}
	if strings.Contains(page, "ordinary-secret-value") {
		t.Fatalf("服务器详情页不得显示机密项值：\n%s", page)
	}

	empty := serversGet(t, handler, cookie, "/ui/servers/srv-bare")
	for _, want := range []string{"暂无可写入的机密项", "暂无可挂载的共享存储", "暂无授予这台服务器的 profile"} {
		if !strings.Contains(empty, want) {
			t.Fatalf("服务器详情页空状态缺少 %q：\n%s", want, empty)
		}
	}
}

// TestServerDetailShowsReadOnlyDeliveryRecords 校验详情页第四组展示记录列
// 表与可展开详情，不提供任何编辑或删除入口；客户端页面整组隐藏。
func TestServerDetailShowsReadOnlyDeliveryRecords(t *testing.T) {
	store := newStubServerStore()
	store.details["srv-fine"].ShowDeliveryRecords = true
	store.details["srv-fine"].DeliveryRecords = []SecretDeliveryRecordRow{{
		ServerName: "fine", Path: "/etc/app/config.yml",
		Template: "token: {{sshmgr-secret:OLD_TOKEN}}", FileSHA256: strings.Repeat("5", 64),
		WrittenAt: time.Unix(1760000900, 0), InitiatingDeviceName: "office-laptop",
		InitiatingInstanceName: "agent-a",
		Secrets: []SecretDeliverySecretRow{{Name: "OLD_TOKEN", ValueVersion: 2, Deleted: true}},
	}}
	store.details["srv-fine"].SiblingDeliveryRecords = []ServerDetailSiblingDeliveryRecord{{
		SecretDeliveryRecordRow: SecretDeliveryRecordRow{
			ServerName: "worker", Path: "/etc/worker/config.yml",
			Template: "token: {{sshmgr-secret:OLD_TOKEN}}", FileSHA256: strings.Repeat("6", 64),
			WrittenAt: time.Unix(1760001000, 0), InitiatingDeviceName: "worker-laptop",
			InitiatingInstanceName: "agent-b",
			Secrets: []SecretDeliverySecretRow{{Name: "OLD_TOKEN", ValueVersion: 2, Deleted: true}},
		},
		SourceServerID: "srv-worker", SourceServerName: "worker",
	}}
	store.details["srv-bare"].ShowDeliveryRecords = true
	handler, cookie := newServersTestHandler(t, RoleStandalone, store, &stubServerWriter{})

	page := serversGet(t, handler, cookie, "/ui/servers/srv-fine")
	for _, want := range []string{
		"下发记录", "/etc/app/config.yml", "OLD_TOKEN", "引用的机密项已删除",
		"写入时间", "office-laptop", "agent-a", "<details", "模板原文",
		"token: {{sshmgr-secret:OLD_TOKEN}}", strings.Repeat("5", 64), "值版本号", "OLD_TOKEN：2",
		"同设备其他服务器条目带来的下发文件", "来源服务器条目", "worker",
		"/etc/worker/config.yml", "worker-laptop", "agent-b",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("服务器详情页下发记录缺少 %q：\n%s", want, page)
		}
	}
	for _, forbidden := range []string{"<form", "编辑下发记录", "删除下发记录", "清理下发记录"} {
		if strings.Contains(page, forbidden) {
			t.Fatalf("下发记录组必须只读，页面不得包含 %q：\n%s", forbidden, page)
		}
	}

	empty := serversGet(t, handler, cookie, "/ui/servers/srv-bare")
	if !strings.Contains(empty, "暂无写到这台服务器的下发记录。") {
		t.Fatalf("服务器详情页下发记录空状态不明确：\n%s", empty)
	}

	clientHandler, clientCookie := newServersTestHandler(t, RoleClient, store, &stubServerWriter{})
	clientPage := serversGet(t, clientHandler, clientCookie, "/ui/servers/srv-fine")
	for _, forbidden := range []string{"下发记录", "同设备其他服务器条目带来的下发文件"} {
		if strings.Contains(clientPage, forbidden) {
			t.Fatalf("客户端服务器详情不得显示 %q：\n%s", forbidden, clientPage)
		}
	}
}

type stubServerWriter struct {
	adds           []ServerAddForm
	otherAdds      []OtherDeviceAddForm
	edits          []ServerEditForm
	deletes        []string
	importRuns     []stubImportRun
	completes      []stubComplete
	conflict       *ServerEditConflictView
	plan           *ServerImportPlanView
	run            *ServerImportRunView
	completeErr    error
	editErr        error
	deleteDigest   string
	deleteErr      error
	anchorCopied   bool
	anchorFrom     string
	anchorConflict bool
}

func (w *stubServerWriter) AddServer(form ServerAddForm, source ServerWriteSource) (*ServerAddResultView, error) {
	w.adds = append(w.adds, form)
	return &ServerAddResultView{
		Detail:       &ServerDetailView{ID: "srv-new", Name: form.Name, User: form.User, Host: form.Host, Port: form.Port},
		DeviceName:   form.DeviceName,
		ProfileNames: []string{"运维"},
		AnchorCopied: w.anchorCopied, AnchorCopyFromServerName: w.anchorFrom,
		AnchorCopyConflict: w.anchorConflict,
	}, nil
}

func (w *stubServerWriter) AddOtherDevice(form OtherDeviceAddForm, source ServerWriteSource) (*OtherDeviceAddResultView, error) {
	w.otherAdds = append(w.otherAdds, form)
	return &OtherDeviceAddResultView{DeviceName: form.Name}, nil
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
	if w.editErr != nil {
		return nil, nil, w.editErr
	}
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
		DeliveryRecords: []DeliveryRecordImpactView{{
			ServerName: "fine", Path: "/etc/app/token.yml", SecretNames: []string{"API_TOKEN"},
		}},
		Digest: "digest-1",
	}, nil
}

func (w *stubServerWriter) DeleteServer(identifier, digest string, source ServerWriteSource) error {
	w.deletes = append(w.deletes, identifier)
	w.deleteDigest = digest
	return w.deleteErr
}

func (w *stubServerWriter) SharedCredentialServers(identifier string) ([]string, error) {
	return []string{"fine", "peer"}, nil
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
	for _, want := range []string{
		"fine", "设备保留", "运维", "deploy_note", "登录凭据（密码）", "仍被 1 台服务器引用",
		"会随之删除的下发记录", "fine：/etc/app/token.yml", "API_TOKEN", "确认删除",
	} {
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
	for _, want := range []string{"web", "bare", "taken", "同名冲突", "运维", "失败", "失败 0"} {
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

// TestServerPagesRequireSession 覆盖安全要求：服务器页所有需要会话的页面
// 在未携带会话时被拒绝，不能向未登录访问者泄露详情或预览。
func TestServerPagesRequireSession(t *testing.T) {
	store := newStubServerStore()
	h, _ := newServersTestHandler(t, RoleStandalone, store, &stubServerWriter{})
	targets := []string{
		"/ui/servers/srv-fine",
		"/ui/servers/srv-fine/edit",
		"/ui/servers/srv-fine/delete",
		"/ui/servers/import",
		"/ui/servers/complete?queue=srv-bare&position=0",
		"/ui/servers/complete/result?queue=srv-bare",
	}
	for _, target := range targets {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Host = "127.0.0.1:18080"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		body := readResponseBody(t, rec.Result())
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("未登录访问 %s 状态码 = %d，应为 401：\n%s", target, rec.Code, body)
		}
		if !strings.Contains(body, "需要启动会话") {
			t.Fatalf("未登录访问 %s 应显示会话错误页：\n%s", target, body)
		}
	}
}

// TestAccessPagesRequireSession 覆盖安全要求：访问区域页面未携带会话时被拒
// 绝，单机回环返回会话错误页。
func TestAccessPagesRequireSession(t *testing.T) {
	store := newStubServerStore()
	h, _ := newServersTestHandler(t, RoleStandalone, store, &stubServerWriter{})
	for _, target := range []string{"/ui/access/", "/ui/access/profiles", "/ui/access/profiles/profile-1/grants", "/ui/access/projects"} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Host = "127.0.0.1:18080"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		body := readResponseBody(t, rec.Result())
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("未登录访问 %s 状态码 = %d，应为 401：\n%s", target, rec.Code, body)
		}
		if !strings.Contains(body, "需要启动会话") {
			t.Fatalf("未登录访问 %s 应显示会话错误页：\n%s", target, body)
		}
	}
}

// TestAuthorityPagesRequireSession 覆盖安全要求：权威端所有需要会话的页面
// 未登录时跳转登录页，而不是显示页面内容。
func TestAuthorityPagesRequireSession(t *testing.T) {
	store := newStubServerStore()
	handler, _, err := NewAuthority(Config{
		Role: RoleServer, Servers: store, ServerWriter: &stubServerWriter{},
		Access: &stubAccessStore{}, Auth: &fakeAuthorityAuth{passwordSet: true, password: "correct-password"},
		CertificateFingerprint: "sha256:0123456789abcdef",
		AllowedHosts:           []string{"127.0.0.1"},
		Port:                   7879,
		Now:                    func() time.Time { return time.Unix(1800000000, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	targets := []string{
		"/ui/servers/srv-fine",
		"/ui/servers/srv-fine/edit",
		"/ui/servers/srv-fine/delete",
		"/ui/servers/import",
		"/ui/servers/complete?queue=srv-bare&position=0",
		"/ui/servers/complete/result?queue=srv-bare",
		"/ui/access/profiles",
		"/ui/access/profiles/profile-1/grants",
		"/ui/access/projects",
		"/ui/access/device-codes",
		"/ui/access/pairing/list",
	}
	for _, target := range targets {
		resp := authorityGetNoSession(t, handler, target)
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != loginPath {
			t.Fatalf("未登录访问 %s 应跳转登录页，状态码=%d 跳转=%s", target, resp.StatusCode, resp.Header.Get("Location"))
		}
		resp.Body.Close()
	}
}

func authorityGetNoSession(t *testing.T, handler http.Handler, target string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = "127.0.0.1:7879"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Result()
}

// TestAuthorityImportWithProfileNeedsRecentVerify 授权 profile 属于扩大权
// 限：权威端导入执行选了 profile 时，要求 5 分钟内重新验证过管理员口令。
func TestAuthorityImportWithProfileNeedsRecentVerify(t *testing.T) {
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

	planPage := authorityGet(t, handler, "127.0.0.1:7879", "/ui/servers/import", cookie)
	planCSRF := hiddenInputValue(t, readResponseBody(t, planPage), "csrf_token")
	resp := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/servers/import/run", url.Values{
		"csrf_token": {planCSRF},
		"path":       {"/home/owner/.ssh/config"}, "candidate": {"web"}, "profile": {"profile-1"},
	}, cookie)
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "/ui/reverify") {
		t.Fatalf("未验证的带 profile 导入应跳转重新验证，状态码=%d 跳转=%s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if len(writer.importRuns) != 0 {
		t.Fatal("未验证时不得执行导入")
	}

	// 重新验证后回到候选页；再次提交通过。
	next := resp.Header.Get("Location")
	reverify := authorityGet(t, handler, "127.0.0.1:7879", next, cookie)
	reverifyBody := readResponseBody(t, reverify)
	verified := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/reverify", url.Values{
		"csrf_token": {hiddenInputValue(t, reverifyBody, "csrf_token")},
		"password":   {"correct-password"}, "next": {hiddenInputValue(t, reverifyBody, "next")},
	}, cookie)
	if verified.StatusCode != http.StatusSeeOther {
		t.Fatalf("重新验证状态码 = %d", verified.StatusCode)
	}
	after := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/servers/import/run", url.Values{
		"csrf_token": {planCSRF},
		"path":       {"/home/owner/.ssh/config"}, "candidate": {"web"}, "profile": {"profile-1"},
	}, cookie)
	if after.StatusCode != http.StatusOK || len(writer.importRuns) != 1 || writer.importRuns[0].profile != "profile-1" {
		t.Fatalf("验证后的导入应执行，状态码=%d 提交记录=%+v", after.StatusCode, writer.importRuns)
	}
}

// TestAuthorityClearCredentialNeedsRecentVerify 清除凭据同样是凭据写入：
// 权威端未重新验证时被拒，验证后通过。
func TestAuthorityClearCredentialNeedsRecentVerify(t *testing.T) {
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

	editPage := authorityGet(t, handler, "127.0.0.1:7879", "/ui/servers/srv-fine/edit", cookie)
	editCSRF := hiddenInputValue(t, readResponseBody(t, editPage), "csrf_token")
	form := url.Values{
		"csrf_token": {editCSRF},
		"server_id":  {"srv-fine"}, "name": {"fine"}, "host": {"192.0.2.10"}, "port": {"22"},
		"user": {"root"}, "shell": {"posix"}, "revision": {"4"},
		"credential_action": {"clear"},
	}
	resp := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/servers/srv-fine/edit", form, cookie)
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "/ui/reverify") {
		t.Fatalf("未验证的清除凭据应跳转重新验证，状态码=%d 跳转=%s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if len(writer.edits) != 0 {
		t.Fatal("未验证时不得清除凭据")
	}
}

// TestAuthorityCompleteSudoPasswordNeedsRecentVerify 补全页的 sudo 密码输入
// 同样受重新验证门控：未验证时不显示输入框。
func TestAuthorityCompleteSudoPasswordNeedsRecentVerify(t *testing.T) {
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

	page := authorityGet(t, handler, "127.0.0.1:7879", "/ui/servers/complete?queue=srv-fine&position=0", cookie)
	body := readResponseBody(t, page)
	if !strings.Contains(body, "重新验证后才能填写密码") {
		t.Fatalf("未验证的补全页应提示重新验证：\n%s", body)
	}
	if strings.Contains(body, `name="sudo_password"`) {
		t.Fatalf("未验证的补全页不应显示 sudo 密码输入框：\n%s", body)
	}
}

// TestServersEditUsesSubmittedRevision 编辑提交必须使用表单隐藏字段携带的
// 修订号做乐观锁，而不是提交时重新读取的当前修订号。
func TestServersEditUsesSubmittedRevision(t *testing.T) {
	store := newStubServerStore()
	writer := &stubServerWriter{}
	h, cookie := newServersTestHandler(t, RoleStandalone, store, writer)

	form := url.Values{
		"server_id": {"srv-fine"}, "name": {"fine"}, "host": {"192.0.2.10"},
		"port": {"22"}, "user": {"root"}, "shell": {"posix"}, "revision": {"3"},
		"description": {"新说明"},
	}
	resp := serversPost(t, h, cookie, "/ui/servers/srv-fine/edit", form)
	body := readResponseBody(t, resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("编辑提交状态码 = %d，应为 303：\n%s", resp.StatusCode, body)
	}
	if len(writer.edits) != 1 || writer.edits[0].ExpectedRevision != 3 {
		t.Fatalf("编辑应使用表单提交的修订号 3，实际：%+v", writer.edits)
	}
}

// TestServersDeletePreviewChangedShowsFreshPreview 删除预览变化时不能给固定
// 409 文案，而要渲染执行时重新组装的最新预览，让 owner 可以直接再次确认。
func TestServersDeletePreviewChangedShowsFreshPreview(t *testing.T) {
	store := newStubServerStore()
	writer := &stubServerWriter{}
	writer.deleteErr = &ServerDeletePreviewChanged{Preview: &ServerDeletePreviewView{
		ServerID: "srv-fine", ServerName: "fine", AnchorCount: 3, Digest: "digest-2",
		DeletedCredentials: []string{"登录凭据（密码）"},
	}}
	h, cookie := newServersTestHandler(t, RoleStandalone, store, writer)

	body := serversPost(t, h, cookie, "/ui/servers/srv-fine/delete", url.Values{
		"server_id": {"srv-fine"}, "digest": {"digest-1"},
	})
	page := readResponseBody(t, body)
	if body.StatusCode != http.StatusConflict {
		t.Fatalf("预览变化状态码 = %d，应为 409", body.StatusCode)
	}
	for _, want := range []string{"删除预览已变化", "要删除 3 个", `name="digest" value="digest-2"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("新预览页缺少 %q：\n%s", want, page)
		}
	}
}

// TestServersDeleteErrorsAreTyped 删除的其他错误按类型给出对应状态码：服务
// 器不存在是 404，其余失败是 500。
func TestServersDeleteErrorsAreTyped(t *testing.T) {
	store := newStubServerStore()
	writer := &stubServerWriter{deleteErr: ErrServerNotFound}
	h, cookie := newServersTestHandler(t, RoleStandalone, store, writer)
	resp := serversPost(t, h, cookie, "/ui/servers/srv-fine/delete", url.Values{
		"server_id": {"srv-fine"}, "digest": {"digest-1"},
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("服务器不存在状态码 = %d，应为 404", resp.StatusCode)
	}

	writer.deleteErr = errServerWriterFailure
	resp = serversPost(t, h, cookie, "/ui/servers/srv-fine/delete", url.Values{
		"server_id": {"srv-fine"}, "digest": {"digest-1"},
	})
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("存储失败状态码 = %d，应为 500", resp.StatusCode)
	}
	if strings.Contains(readResponseBody(t, resp), "预览已变化") {
		t.Fatal("其他失败不得误报为预览变化")
	}
}

var errServerWriterFailure = errors.New("存储暂时不可用")

// TestServersPagesListSharedCredentialServers 补私钥口令影响共用凭据的全部
// 服务器：补全页与编辑页在提交前列出受影响的服务器名。
func TestServersPagesListSharedCredentialServers(t *testing.T) {
	store := newStubServerStore()
	store.details["srv-key"] = &ServerDetailView{
		ID: "srv-key", Name: "key", User: "root", Host: "192.0.2.30", Port: 22,
		Shell: "posix", CredentialType: "private_key", Role: "网站",
		AttentionReasons: []string{models.ReasonNeedsPassphrase}, Revision: 2,
	}
	h, cookie := newServersTestHandler(t, RoleStandalone, store, &stubServerWriter{})

	complete := serversGet(t, h, cookie, "/ui/servers/complete?queue=srv-key&position=0")
	if !strings.Contains(complete, "共用这把私钥的服务器会一起去掉待补标记：fine,peer") {
		t.Fatalf("补全页应列出共用凭据的服务器：\n%s", complete)
	}
	edit := serversGet(t, h, cookie, "/ui/servers/srv-key/edit")
	if !strings.Contains(edit, "「只补私钥口令」会影响共用这把私钥的全部服务器：fine,peer") {
		t.Fatalf("编辑页应列出共用凭据的服务器：\n%s", edit)
	}
}

// TestServersEditValidationErrorRendersForm 编辑校验失败要重新渲染表单并给出
// 指向字段的中文提示：非凭据字段保留提交值，凭据不回显。
func TestServersEditValidationErrorRendersForm(t *testing.T) {
	store := newStubServerStore()
	writer := &stubServerWriter{editErr: &ServerFormError{Message: "密码不能为空（更换凭据请给新值）"}}
	h, cookie := newServersTestHandler(t, RoleStandalone, store, writer)

	resp := serversPost(t, h, cookie, "/ui/servers/srv-fine/edit", url.Values{
		"server_id": {"srv-fine"}, "name": {"fine"}, "host": {"192.0.2.10"},
		"port": {"22"}, "user": {"root"}, "shell": {"posix"}, "revision": {"4"},
		"description": {"页面说明"}, "role": {"新角色"},
		"credential_action": {"password"}, "password": {"secret-input"},
	})
	body := readResponseBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("校验失败状态码 = %d，应为 400：\n%s", resp.StatusCode, body)
	}
	for _, want := range []string{"密码不能为空", `value="页面说明"`, `value="新角色"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("校验页面缺少 %q：\n%s", want, body)
		}
	}
	if strings.Contains(body, "secret-input") || strings.Contains(body, "--password") {
		t.Fatalf("校验页面不得回显凭据或命令行参数名：\n%s", body)
	}
}

// TestServersCompleteValidationErrorRendersForm 补全校验失败同样重新渲染表单
// 并显示面向页面的中文提示。
func TestServersCompleteValidationErrorRendersForm(t *testing.T) {
	store := newStubServerStore()
	writer := &stubServerWriter{completeErr: &ServerFormError{Message: "这台服务器已有登录凭据，补全表单不能另设密码"}}
	h, cookie := newServersTestHandler(t, RoleStandalone, store, writer)

	resp := serversPost(t, h, cookie, "/ui/servers/complete", url.Values{
		"csrf_token": {serversCSRF(t, h, cookie)},
		"queue":      {"srv-fine"}, "position": {"0"}, "action": {"save"},
		"description": {"页面说明"}, "password": {"secret-input"},
	})
	body := readResponseBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("补全校验失败状态码 = %d，应为 400：\n%s", resp.StatusCode, body)
	}
	for _, want := range []string{"已有登录凭据", `value="页面说明"`, "srv-fine"} {
		if !strings.Contains(body, want) {
			t.Fatalf("补全校验页面缺少 %q：\n%s", want, body)
		}
	}
	if strings.Contains(body, "secret-input") {
		t.Fatalf("补全校验页面不得回显凭据：\n%s", body)
	}
}
