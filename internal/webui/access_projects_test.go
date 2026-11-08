package webui

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// stubProjectAccessStore 在既有访问管理桩上补齐项目令牌方法；嵌入原有桩
// 是为了不改动另一张测试文件里的结构体，降低与其他访问页面的合并冲突。
type stubProjectAccessStore struct {
	*stubAccessStore
	projects           []AccessProjectSummary
	includeRevokedSeen bool
	addName            string
	addProfileID       string
	addSource          AccessWriteSource
	addResult          AccessProjectToken
	rotateID           string
	rotateSource       AccessWriteSource
	rotateResult       AccessProjectToken
	statusID           string
	statusSource       AccessWriteSource
	deleteID           string
	deleteSource       AccessWriteSource
}

func (s *stubProjectAccessStore) ListProjects(includeRevoked bool) ([]AccessProjectSummary, error) {
	s.includeRevokedSeen = includeRevoked
	if includeRevoked {
		return s.projects, nil
	}
	visible := make([]AccessProjectSummary, 0, len(s.projects))
	for _, project := range s.projects {
		if project.Status != "revoked" {
			visible = append(visible, project)
		}
	}
	return visible, nil
}

func (s *stubProjectAccessStore) AddProject(name, profileID string, source AccessWriteSource) (AccessProjectToken, error) {
	s.addName = name
	s.addProfileID = profileID
	s.addSource = source
	return s.addResult, nil
}

func (s *stubProjectAccessStore) RotateProject(id string, source AccessWriteSource) (AccessProjectToken, error) {
	s.rotateID = id
	s.rotateSource = source
	return s.rotateResult, nil
}

func (s *stubProjectAccessStore) DisableProject(id string, source AccessWriteSource) error {
	s.statusID = id
	s.statusSource = source
	return nil
}

func (s *stubProjectAccessStore) EnableProject(id string, source AccessWriteSource) error {
	s.statusID = id
	s.statusSource = source
	return nil
}

func (s *stubProjectAccessStore) RevokeProject(id string, source AccessWriteSource) error {
	s.statusID = id
	s.statusSource = source
	return nil
}

func (s *stubProjectAccessStore) DeleteProject(id string, source AccessWriteSource) error {
	s.deleteID = id
	s.deleteSource = source
	return nil
}

func newStandaloneProjectHandler(t *testing.T, store AccessStore) (http.Handler, *http.Cookie) {
	t.Helper()
	handler, startup, err := New(Config{
		Role:    RoleStandalone,
		Servers: &stubServerSource{},
		Access:  store,
		Port:    18080,
	})
	if err != nil {
		t.Fatalf("构造单机 Web UI Handler 失败：%v", err)
	}
	session := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookie := session.Cookies()[0]
	session.Body.Close()
	return handler, cookie
}

// 下面几个方法是既有访问管理桩对项目令牌接口的兜底实现：旧测试只覆盖
// profile，不需要记录项目调用；项目页测试统一使用带记录字段的包装桩。
func (s *stubAccessStore) ListProjects(bool) ([]AccessProjectSummary, error) {
	return nil, nil
}

func (s *stubAccessStore) AddProject(string, string, AccessWriteSource) (AccessProjectToken, error) {
	return AccessProjectToken{}, nil
}

func (s *stubAccessStore) RotateProject(string, AccessWriteSource) (AccessProjectToken, error) {
	return AccessProjectToken{}, nil
}

func (s *stubAccessStore) DisableProject(string, AccessWriteSource) error { return nil }

func (s *stubAccessStore) EnableProject(string, AccessWriteSource) error { return nil }

func (s *stubAccessStore) RevokeProject(string, AccessWriteSource) error { return nil }

func (s *stubAccessStore) DeleteProject(string, AccessWriteSource) error { return nil }

func TestProjectPageListsTokensAndHidesRevokedByDefault(t *testing.T) {
	created := time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC)
	store := &stubProjectAccessStore{stubAccessStore: &stubAccessStore{
		profiles: []AccessProfileSummary{{ID: "profile-1", Name: "运维"}},
	}}
	store.projects = []AccessProjectSummary{
		{ID: "project-1", Name: "agent", TokenPrefix: "tok_aaa", Status: "active", ProfileID: "profile-1", ProfileName: "运维", CreatedAt: created},
		{ID: "project-2", Name: "old", TokenPrefix: "tok_bbb", Status: "revoked", ProfileID: "profile-1", ProfileName: "运维", CreatedAt: created},
	}
	handler, cookie := newStandaloneProjectHandler(t, store)

	response := doGetWithCookie(t, handler, cookie, "/ui/access/projects")
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("项目页状态码=%d 页面：\n%s", response.StatusCode, body)
	}
	for _, want := range []string{"agent", "tok_aaa", "启用中", "运维", "2026-11-01"} {
		if !strings.Contains(body, want) {
			t.Fatalf("项目页默认列表应包含 %q：\n%s", want, body)
		}
	}
	if strings.Contains(body, "tok_bbb") || strings.Contains(body, ">已吊销<") {
		t.Fatalf("项目页默认应隐藏已吊销项目：\n%s", body)
	}
	if store.includeRevokedSeen {
		t.Fatal("项目页默认读取列表时不应要求包含已吊销项目")
	}
	if !strings.Contains(body, `<a href="/ui/access/projects?include_revoked=1">显示已吊销</a>`) {
		t.Fatalf("项目页应提供显示已吊销项目的切换：\n%s", body)
	}

	all := doGetWithCookie(t, handler, cookie, "/ui/access/projects?include_revoked=1")
	allBody := readResponseBody(t, all)
	if all.StatusCode != http.StatusOK || !strings.Contains(allBody, "tok_bbb") || !strings.Contains(allBody, "已吊销") {
		t.Fatalf("切换后应显示已吊销项目，状态码=%d 页面：\n%s", all.StatusCode, allBody)
	}
	if !store.includeRevokedSeen {
		t.Fatal("切换后读取列表时应要求包含已吊销项目")
	}
	if !strings.Contains(allBody, `<a href="/ui/access/projects">隐藏已吊销</a>`) {
		t.Fatalf("切换后应能回到默认隐藏视图：\n%s", allBody)
	}

	if !strings.Contains(allBody, `form method="post" action="/ui/access/projects/delete"`) {
		t.Fatalf("已吊销项目应有删除表单：\n%s", allBody)
	}
	defaultRowDelete := strings.Count(body, `action="/ui/access/projects/delete"`)
	if defaultRowDelete != 0 {
		t.Fatalf("未吊销项目不应有删除按钮，页面：\n%s", body)
	}
}

func TestProjectPageIssuesTokenOnceWithSnippetAndNoStore(t *testing.T) {
	store := &stubProjectAccessStore{stubAccessStore: &stubAccessStore{
		profiles: []AccessProfileSummary{{ID: "profile-1", Name: "运维"}},
	}}
	store.addResult = AccessProjectToken{
		ID: "project-9", Name: "agent", Token: "one-time-token",
		Snippet: `{"mcpServers":{"ssh":{"command":"sshmgr","args":["mcp"],"env":{"SSHMGR_TOKEN":"one-time-token"}}}}`,
	}
	var logged strings.Builder
	handler, startup, err := New(Config{
		Role:    RoleStandalone,
		Servers: &stubServerSource{},
		Access:  store,
		Port:    18080,
		Log: func(format string, arguments ...any) {
			fmt.Fprintf(&logged, format, arguments...)
			logged.WriteByte('\n')
		},
	})
	if err != nil {
		t.Fatalf("构造单机 Web UI Handler 失败：%v", err)
	}
	session := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookie := session.Cookies()[0]
	session.Body.Close()

	response := postStandaloneForm(t, handler, cookie, "/ui/access/projects", url.Values{
		"name": {"agent"}, "profile_id": {"profile-1"},
	})
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("签发项目令牌应直接返回一次性页面，状态码=%d 页面：\n%s", response.StatusCode, body)
	}
	if cacheControl := response.Header.Get("Cache-Control"); cacheControl != "no-store" {
		t.Fatalf("一次性令牌页面应带 Cache-Control: no-store，实际 %q", cacheControl)
	}
	if !strings.Contains(body, "one-time-token") {
		t.Fatalf("一次性页面应显示令牌：\n%s", body)
	}
	wantSnippet := `{"mcpServers":{"ssh":{"command":"sshmgr","args":["mcp"],"env":{"SSHMGR_TOKEN":"one-time-token"}}}}`
	if !strings.Contains(body, template.HTMLEscapeString(wantSnippet)) {
		t.Fatalf("一次性页面应显示与命令行一致的 .mcp.json 片段：\n%s", body)
	}
	for _, want := range []string{
		"离开本页后不能再查看", "不要把含令牌的文件提交进 git",
		`data-copy-target="project-token-value"`, `data-copy-target="project-snippet-value"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("一次性页面应包含 %q：\n%s", want, body)
		}
	}
	if strings.Contains(logged.String(), "one-time-token") {
		t.Fatalf("日志不得包含项目令牌，实际：\n%s", logged.String())
	}

	refreshed := doGetWithCookie(t, handler, cookie, "/ui/access/projects")
	refreshedBody := readResponseBody(t, refreshed)
	if refreshed.StatusCode != http.StatusOK || strings.Contains(refreshedBody, "one-time-token") {
		t.Fatalf("刷新项目页不得再次显示令牌，状态码=%d 页面：\n%s", refreshed.StatusCode, refreshedBody)
	}
	for _, path := range []string{
		"/ui/access/projects/project-9/token", "/ui/access/projects/token", "/ui/access/projects/project-9",
	} {
		resp := doGetWithCookie(t, handler, cookie, path)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("不应存在重新查看令牌的路由 %s，状态码=%d", path, resp.StatusCode)
		}
	}
	if store.addName != "agent" || store.addProfileID != "profile-1" || store.addSource.RemoteAddress != "198.51.100.9" {
		t.Fatalf("签发请求应传给访问管理来源，实际 name=%q profile=%q source=%+v", store.addName, store.addProfileID, store.addSource)
	}
}

func TestProjectPageRotatesTokenOnce(t *testing.T) {
	store := &stubProjectAccessStore{stubAccessStore: &stubAccessStore{}}
	store.rotateResult = AccessProjectToken{
		ID: "project-1", Name: "agent", Token: "rotated-token",
		Snippet: `{"mcpServers":{"ssh":{"command":"sshmgr","args":["mcp"],"env":{"SSHMGR_TOKEN":"rotated-token"}}}}`,
	}
	handler, cookie := newStandaloneProjectHandler(t, store)

	response := postStandaloneForm(t, handler, cookie, "/ui/access/projects/rotate", url.Values{
		"project_id": {"project-1"},
	})
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("轮换项目令牌应直接返回一次性页面，状态码=%d 页面：\n%s", response.StatusCode, body)
	}
	if cacheControl := response.Header.Get("Cache-Control"); cacheControl != "no-store" {
		t.Fatalf("轮换结果页面应带 Cache-Control: no-store，实际 %q", cacheControl)
	}
	if !strings.Contains(body, "rotated-token") || !strings.Contains(body, "离开本页后不能再查看") {
		t.Fatalf("轮换结果页面应一次性显示新令牌：\n%s", body)
	}
	if store.rotateID != "project-1" || store.rotateSource.RemoteAddress != "198.51.100.9" {
		t.Fatalf("轮换请求应传给访问管理来源，实际 id=%q source=%+v", store.rotateID, store.rotateSource)
	}

	refreshed := doGetWithCookie(t, handler, cookie, "/ui/access/projects")
	refreshedBody := readResponseBody(t, refreshed)
	if strings.Contains(refreshedBody, "rotated-token") {
		t.Fatalf("刷新项目页不得再次显示轮换后的令牌：\n%s", refreshedBody)
	}
}

func TestProjectLifecycleActionsCallAccessStore(t *testing.T) {
	store := &stubProjectAccessStore{stubAccessStore: &stubAccessStore{}}
	handler, cookie := newStandaloneProjectHandler(t, store)

	disabled := postStandaloneForm(t, handler, cookie, "/ui/access/projects/disable", url.Values{
		"project_id": {"project-1"},
	})
	disabledBody := readResponseBody(t, disabled)
	if disabled.StatusCode != http.StatusSeeOther || disabled.Header.Get("Location") != "/ui/access/projects" {
		t.Fatalf("停用项目应回到列表，状态码=%d 跳转=%q 页面：\n%s", disabled.StatusCode, disabled.Header.Get("Location"), disabledBody)
	}

	revoked := postStandaloneForm(t, handler, cookie, "/ui/access/projects/revoke", url.Values{
		"project_id": {"project-1"},
	})
	revokedBody := readResponseBody(t, revoked)
	if revoked.StatusCode != http.StatusBadRequest {
		t.Fatalf("吊销缺少确认时应返回 400，状态码=%d 页面：\n%s", revoked.StatusCode, revokedBody)
	}
	if store.statusID != "project-1" {
		t.Fatalf("缺少确认时不应执行吊销，实际调用 id=%q", store.statusID)
	}

	confirmed := postStandaloneForm(t, handler, cookie, "/ui/access/projects/revoke", url.Values{
		"project_id": {"project-1"}, "confirm": {"1"},
	})
	confirmedBody := readResponseBody(t, confirmed)
	if confirmed.StatusCode != http.StatusSeeOther || confirmed.Header.Get("Location") != "/ui/access/projects" {
		t.Fatalf("确认吊销后应回到列表，状态码=%d 跳转=%q 页面：\n%s", confirmed.StatusCode, confirmed.Header.Get("Location"), confirmedBody)
	}

	enabled := postStandaloneForm(t, handler, cookie, "/ui/access/projects/enable", url.Values{
		"project_id": {"project-2"},
	})
	enabledBody := readResponseBody(t, enabled)
	if enabled.StatusCode != http.StatusSeeOther || enabled.Header.Get("Location") != "/ui/access/projects" {
		t.Fatalf("重新启用项目应回到列表，状态码=%d 跳转=%q 页面：\n%s", enabled.StatusCode, enabled.Header.Get("Location"), enabledBody)
	}

	deleted := postStandaloneForm(t, handler, cookie, "/ui/access/projects/delete", url.Values{
		"project_id": {"project-2"},
	})
	deletedBody := readResponseBody(t, deleted)
	if deleted.StatusCode != http.StatusSeeOther || deleted.Header.Get("Location") != "/ui/access/projects" {
		t.Fatalf("删除已吊销项目应回到列表，状态码=%d 跳转=%q 页面：\n%s", deleted.StatusCode, deleted.Header.Get("Location"), deletedBody)
	}
	if store.deleteID != "project-2" || store.deleteSource.RemoteAddress != "198.51.100.9" {
		t.Fatalf("删除请求应传给访问管理来源，实际 id=%q source=%+v", store.deleteID, store.deleteSource)
	}
}

func TestProjectRoutesFollowRole(t *testing.T) {
	client, clientStartup, err := New(Config{
		Role:    RoleClient,
		Servers: &stubServerSource{},
		Port:    18080,
	})
	if err != nil {
		t.Fatalf("构造客户端 Web UI Handler 失败：%v", err)
	}
	clientSession := exchangeStartupToken(t, client, clientStartup.URL("127.0.0.1:18080"))
	clientCookie := clientSession.Cookies()[0]
	clientSession.Body.Close()
	clientPage := doGetWithCookie(t, client, clientCookie, "/ui/access/")
	clientBody := readResponseBody(t, clientPage)
	clientCSRF := hiddenInputMetaOrHiddenValue(t, clientBody)
	for _, target := range []string{
		"/ui/access/projects", "/ui/access/projects/rotate", "/ui/access/projects/disable",
		"/ui/access/projects/enable", "/ui/access/projects/revoke", "/ui/access/projects/delete",
	} {
		resp := doGetWithCookie(t, client, clientCookie, target)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("客户端不应有项目数据或写路由 %s，状态码=%d", target, resp.StatusCode)
		}
		resp = postAccessWrite(t, client, clientCookie, target, url.Values{}, "http://127.0.0.1:18080", clientCSRF)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("客户端项目写路由 %s 应返回 404，状态码=%d", target, resp.StatusCode)
		}
	}

	server, _, err := NewAuthority(Config{
		Role: RoleServer, Servers: &stubServerSource{},
		Access: &stubProjectAccessStore{stubAccessStore: &stubAccessStore{
			profiles: []AccessProfileSummary{{ID: "profile-1", Name: "运维"}},
		}},
		Auth:         &fakeAuthorityAuth{passwordSet: true, password: "correct-password", now: time.Unix(1800000000, 0)},
		AllowedHosts: []string{"127.0.0.1"}, Port: 7879,
	})
	if err != nil {
		t.Fatalf("构造权威端 Web UI Handler 失败：%v", err)
	}
	cookie := authoritySuccessfulLogin(t, server, "127.0.0.1:7879", "correct-password")
	page := authorityGet(t, server, "127.0.0.1:7879", "/ui/access/projects", cookie)
	pageBody := readResponseBody(t, page)
	if page.StatusCode != http.StatusOK || !strings.Contains(pageBody, "项目令牌") {
		t.Fatalf("权威端应打开项目页，状态码=%d 页面：\n%s", page.StatusCode, pageBody)
	}
}

func TestProjectNavigationFollowsRoleCapabilities(t *testing.T) {
	standalonePage := loginAndGetList(t, RoleStandalone, []ServerRow{{Name: "web-1"}}, "")
	if !strings.Contains(standalonePage, `<a href="/ui/access/projects">项目令牌</a>`) {
		t.Fatalf("单机导航应显示项目令牌入口：\n%s", standalonePage)
	}
	clientPage := loginAndGetList(t, RoleClient, []ServerRow{{Name: "web-1"}}, "")
	if strings.Contains(clientPage, `<a href="/ui/access/projects">项目令牌</a>`) {
		t.Fatalf("客户端导航不应显示项目令牌数据入口：\n%s", clientPage)
	}
}

func TestProjectWriteRoutesRequireOriginAndCSRFToken(t *testing.T) {
	store := &stubProjectAccessStore{stubAccessStore: &stubAccessStore{}}
	handler, cookie := newStandaloneProjectHandler(t, store)
	page := doGetWithCookie(t, handler, cookie, "/ui/access/projects")
	pageBody := readResponseBody(t, page)
	csrfToken := hiddenInputValue(t, pageBody, "csrf_token")

	routes := []struct {
		target string
		form   url.Values
	}{
		{"/ui/access/projects", url.Values{"name": {"agent"}, "profile_id": {"profile-1"}}},
		{"/ui/access/projects/rotate", url.Values{"project_id": {"project-1"}}},
		{"/ui/access/projects/disable", url.Values{"project_id": {"project-1"}}},
		{"/ui/access/projects/enable", url.Values{"project_id": {"project-1"}}},
		{"/ui/access/projects/revoke", url.Values{"project_id": {"project-1"}, "confirm": {"1"}}},
		{"/ui/access/projects/delete", url.Values{"project_id": {"project-1"}}},
	}
	for _, route := range routes {
		missing := postAccessWrite(t, handler, cookie, route.target, route.form, "http://127.0.0.1:18080", "")
		missingBody := readResponseBody(t, missing)
		if missing.StatusCode != http.StatusForbidden {
			t.Fatalf("%s 缺少防跨站请求伪造令牌应返回 403，状态码=%d 页面：\n%s", route.target, missing.StatusCode, missingBody)
		}
		wrongOrigin := postAccessWrite(t, handler, cookie, route.target, route.form, "https://evil.example", csrfToken)
		wrongOriginBody := readResponseBody(t, wrongOrigin)
		if wrongOrigin.StatusCode != http.StatusForbidden {
			t.Fatalf("%s 来源不对应返回 403，状态码=%d 页面：\n%s", route.target, wrongOrigin.StatusCode, wrongOriginBody)
		}
	}
	if store.addName != "" || store.rotateID != "" || store.deleteID != "" {
		t.Fatalf("校验失败的写请求不应到达访问管理来源，实际 %+v", store)
	}
}

func TestAuthorityProjectSensitiveActionsRequireReverify(t *testing.T) {
	auth := &fakeAuthorityAuth{passwordSet: true, password: "correct-password", now: time.Unix(1800000000, 0)}
	store := &stubProjectAccessStore{stubAccessStore: &stubAccessStore{
		profiles: []AccessProfileSummary{{ID: "profile-1", Name: "运维"}},
	}}
	store.addResult = AccessProjectToken{ID: "project-9", Name: "agent", Token: "issued-after-reverify", Snippet: "{}"}
	handler, _, err := NewAuthority(Config{
		Role: RoleServer, Servers: &stubServerSource{}, Access: store, Auth: auth,
		AllowedHosts: []string{"127.0.0.1"}, Port: 7879,
		Now: func() time.Time { return auth.now },
	})
	if err != nil {
		t.Fatalf("构造权威端 Web UI Handler 失败：%v", err)
	}
	cookie := authoritySuccessfulLogin(t, handler, "127.0.0.1:7879", "correct-password")
	auth.now = auth.now.Add(6 * time.Minute)
	page := authorityGet(t, handler, "127.0.0.1:7879", "/ui/access/projects", cookie)
	pageBody := readResponseBody(t, page)
	csrfToken := hiddenInputValue(t, pageBody, "csrf_token")

	addRejected := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/access/projects", url.Values{
		"csrf_token": {csrfToken}, "name": {"agent"}, "profile_id": {"profile-1"},
	}, cookie)
	addRejectedBody := readResponseBody(t, addRejected)
	addResume := "/ui/access/projects?add_name=agent&add_profile=profile-1&reverify_add=1"
	wantAddRedirect := `/ui/reverify?next=` + url.QueryEscape(addResume)
	if addRejected.StatusCode != http.StatusSeeOther || addRejected.Header.Get("Location") != wantAddRedirect || store.addName != "" {
		t.Fatalf("签发项目令牌应先重新验证，状态码=%d 跳转=%q 调用=%q 页面：\n%s",
			addRejected.StatusCode, addRejected.Header.Get("Location"), store.addName, addRejectedBody)
	}

	rotateRejected := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/access/projects/rotate", url.Values{
		"csrf_token": {csrfToken}, "project_id": {"project-1"},
	}, cookie)
	rotateRejectedBody := readResponseBody(t, rotateRejected)
	rotateResume := "/ui/access/projects?reverify_rotate=project-1"
	wantRotateRedirect := `/ui/reverify?next=` + url.QueryEscape(rotateResume)
	if rotateRejected.StatusCode != http.StatusSeeOther || rotateRejected.Header.Get("Location") != wantRotateRedirect || store.rotateID != "" {
		t.Fatalf("轮换项目令牌应先重新验证，状态码=%d 跳转=%q 调用=%q 页面：\n%s",
			rotateRejected.StatusCode, rotateRejected.Header.Get("Location"), store.rotateID, rotateRejectedBody)
	}

	enableRejected := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/access/projects/enable", url.Values{
		"csrf_token": {csrfToken}, "project_id": {"project-1"},
	}, cookie)
	enableRejectedBody := readResponseBody(t, enableRejected)
	enableResume := "/ui/access/projects?reverify_enable=project-1"
	wantEnableRedirect := `/ui/reverify?next=` + url.QueryEscape(enableResume)
	if enableRejected.StatusCode != http.StatusSeeOther || enableRejected.Header.Get("Location") != wantEnableRedirect || store.statusID != "" {
		t.Fatalf("重新启用项目应先重新验证，状态码=%d 跳转=%q 调用=%q 页面：\n%s",
			enableRejected.StatusCode, enableRejected.Header.Get("Location"), store.statusID, enableRejectedBody)
	}

	reverifyPage := authorityGet(t, handler, "127.0.0.1:7879", wantAddRedirect, cookie)
	reverifyBody := readResponseBody(t, reverifyPage)
	reverifyCSRF := hiddenInputValue(t, reverifyBody, "csrf_token")
	reverified := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/reverify", url.Values{
		"csrf_token": {reverifyCSRF}, "password": {"correct-password"}, "next": {addResume},
	}, cookie)
	reverifiedBody := readResponseBody(t, reverified)
	if reverified.StatusCode != http.StatusSeeOther || reverified.Header.Get("Location") != addResume {
		t.Fatalf("重新验证后应回到签发表单，状态码=%d 跳转=%q 页面：\n%s",
			reverified.StatusCode, reverified.Header.Get("Location"), reverifiedBody)
	}
	resumed := authorityGet(t, handler, "127.0.0.1:7879", addResume, cookie)
	resumedBody := readResponseBody(t, resumed)
	if !strings.Contains(resumedBody, "签发项目令牌前需要重新验证管理员口令") ||
		!strings.Contains(resumedBody, `value="agent"`) ||
		!strings.Contains(resumedBody, `<option value="profile-1" selected>`) {
		t.Fatalf("重新验证返回页应提示并保留签发表单内容：\n%s", resumedBody)
	}

	added := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/access/projects", url.Values{
		"csrf_token": {csrfToken}, "name": {"agent"}, "profile_id": {"profile-1"},
	}, cookie)
	addedBody := readResponseBody(t, added)
	if added.StatusCode != http.StatusOK || !strings.Contains(addedBody, "issued-after-reverify") {
		t.Fatalf("重新验证后签发应返回一次性令牌，状态码=%d 页面：\n%s", added.StatusCode, addedBody)
	}

	disabled := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/access/projects/disable", url.Values{
		"csrf_token": {csrfToken}, "project_id": {"project-2"},
	}, cookie)
	disabledBody := readResponseBody(t, disabled)
	if disabled.StatusCode != http.StatusSeeOther || store.statusID != "project-2" {
		t.Fatalf("停用项目不应要求重新验证，状态码=%d 调用=%q 页面：\n%s",
			disabled.StatusCode, store.statusID, disabledBody)
	}
	revoked := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/access/projects/revoke", url.Values{
		"csrf_token": {csrfToken}, "project_id": {"project-3"}, "confirm": {"1"},
	}, cookie)
	revokedBody := readResponseBody(t, revoked)
	if revoked.StatusCode != http.StatusSeeOther || store.statusID != "project-3" {
		t.Fatalf("吊销项目不应要求重新验证，状态码=%d 调用=%q 页面：\n%s",
			revoked.StatusCode, store.statusID, revokedBody)
	}
	deleted := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/access/projects/delete", url.Values{
		"csrf_token": {csrfToken}, "project_id": {"project-4"},
	}, cookie)
	deletedBody := readResponseBody(t, deleted)
	if deleted.StatusCode != http.StatusSeeOther || store.deleteID != "project-4" {
		t.Fatalf("删除项目不应要求重新验证，状态码=%d 调用=%q 页面：\n%s",
			deleted.StatusCode, store.deleteID, deletedBody)
	}
}
