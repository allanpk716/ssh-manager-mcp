package webui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// authorizationSecretSource 是授权页测试用的机密项读取来源，额外实现
// 缺备注查询，用于验证提示与主页待办。
type authorizationSecretSource struct {
	rows            []SecretRow
	missingNoteRows []SecretRow
	deliveryRecords []SecretDeliveryRecordRow
	deliveryQueried string
}

func (s authorizationSecretSource) ListSecrets(_, _, _ string) ([]SecretRow, error) {
	return s.rows, nil
}

func (s authorizationSecretSource) ListSecretsWithoutNote() ([]SecretRow, error) {
	return s.missingNoteRows, nil
}

func (s authorizationSecretSource) SecretDeliveryRecords(secretName string) ([]SecretDeliveryRecordRow, error) {
	s.deliveryQueried = secretName
	return s.deliveryRecords, nil
}

type authorizationShareSource struct {
	shares          []ShareRow
	missingNoteRows []ShareRow
}

func (s authorizationShareSource) ListShares(_, _, _ string) ([]ShareRow, error) {
	return s.shares, nil
}

func (s authorizationShareSource) ListSharedStoragesWithoutNote() ([]ShareRow, error) {
	return s.missingNoteRows, nil
}

func (s authorizationShareSource) ShareDetail(_, identifier string) (*ShareRow, error) {
	for index := range s.shares {
		if s.shares[index].ID == identifier || s.shares[index].Name == identifier {
			row := s.shares[index]
			return &row, nil
		}
	}
	return nil, &ShareNotFoundError{Identifier: identifier}
}

type authorizationTopologySource struct{}

func (authorizationTopologySource) Topology(_, _ string) (*TopologyData, error) {
	return &TopologyData{
		Sites:    []TopologySite{{ID: "site-home", Name: "家", LocationPath: "公司 / 家"}},
		Networks: []TopologyNetwork{{ID: "network-home", Name: "家庭局域网", Type: "physical", SiteID: "site-home"}},
		Devices: []TopologyDevice{{
			ID: "device-work", Name: "工作站", SiteID: "site-home", LocationPath: "公司 / 家",
			Servers: []TopologyDeviceServer{{ID: "server-web", Name: "web-1"}},
		}},
		Memberships: []TopologyMembership{{DeviceID: "device-work", NetworkID: "network-home"}},
	}, nil
}

type authorizationShareWriter struct{}

func (authorizationShareWriter) AddShare(ShareAddForm, ShareWriteSource) (*ShareRow, error) {
	return nil, nil
}

func (authorizationShareWriter) EditShare(string, ShareEditForm, ShareWriteSource) (*ShareRow, error) {
	return nil, nil
}

func (authorizationShareWriter) DeleteShare(string, ShareWriteSource) error { return nil }

type authorizationSecretWriter struct {
	preview SecretAuthorizationPreview
	target  []SecretAuthorizationCell
}

func (w *authorizationSecretWriter) AddSecret(string, string, string, []byte, SecretWriteSource) error {
	return nil
}

func (w *authorizationSecretWriter) SetSecretValue(string, []byte, SecretWriteSource) error {
	return nil
}

func (w *authorizationSecretWriter) EditSecretMetadata(string, string, string, SecretWriteSource) error {
	return nil
}

func (w *authorizationSecretWriter) DeleteSecret(string, SecretWriteSource) error { return nil }

func (w *authorizationSecretWriter) PreviewAuthorization(target []SecretAuthorizationCell) (SecretAuthorizationPreview, error) {
	w.target = append([]SecretAuthorizationCell(nil), target...)
	return w.preview, nil
}

func (w *authorizationSecretWriter) ApplyAuthorization([]SecretAuthorizationCell, string, SecretWriteSource) (SecretApplyResult, *SecretAuthorizationConflict, error) {
	return SecretApplyResult{}, nil, nil
}

func newAuthorizationTestHandler(t *testing.T) (http.Handler, *stubAccessStore, *authorizationSecretWriter, *http.Cookie) {
	t.Helper()
	access := &stubAccessStore{
		profiles: []AccessProfileSummary{{
			ID: "profile-1", Name: "运维",
			Servers: []AccessServerRef{{ID: "server-web", Name: "web-1"}},
		}},
		clients: []ClientSummary{{
			ID: "client-1", Name: "笔记本", ProfileID: "profile-1", ProfileName: "运维",
		}},
	}
	writer := &authorizationSecretWriter{preview: SecretAuthorizationPreview{
		Changes: []SecretAuthorizationChange{{
			SecretID: "secret-1", SecretName: "MATRIX_TOKEN",
			Added:   []SecretAuthorizationServer{{ID: "server-db", Name: "db-1"}},
			Removed: []SecretAuthorizationServer{{ID: "server-web", Name: "web-1"}},
		}},
		TotalAdded: 1, TotalRemoved: 1, Summary: "授权变更摘要",
	}}
	handler, startup, err := New(Config{
		Role: RoleStandalone,
		Servers: &stubServerSource{rows: []ServerRow{
			{ID: "server-web", Name: "web-1"}, {ID: "server-db", Name: "db-1"},
		}},
		Access: access,
		Secrets: authorizationSecretSource{
			rows: []SecretRow{{
				ID: "secret-1", Name: "MATRIX_TOKEN", Note: "浏览器测试机密项",
				AllowedServers: []string{"web-1"},
			}, {
				ID: "secret-2", Name: "EMPTY_NOTE", Note: " ",
			}},
			missingNoteRows: []SecretRow{{ID: "secret-2", Name: "EMPTY_NOTE"}},
		},
		SecretsWriter: writer,
		SharesWriter:  authorizationShareWriter{},
		Shares: authorizationShareSource{shares: []ShareRow{{
			ID: "share-1", Name: "datasets", Note: "临时数据", DeviceName: "工作站",
			PasswordSecret: "MATRIX_TOKEN", PasswordSecretID: "secret-1",
			MountableServers: []ShareServerView{{ID: "server-web", Name: "web-1"}},
			MountableCount:   1,
		}}},
		Topology: authorizationTopologySource{},
		Port:     18080,
	})
	if err != nil {
		t.Fatalf("构造 Web UI Handler 失败：%v", err)
	}
	resp := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookies := resp.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("应签发会话 cookie，实际 %d 个", len(cookies))
	}
	return handler, access, writer, cookies[0]
}

func authorizationForbiddenWords() []string {
	return []string{"项目令牌", "设备码", "站点", "网络成员", "登录条目", "兜底主机", "cache token"}
}

func TestAuthorizationSubpagesShowCardsAndClientCounts(t *testing.T) {
	handler, _, _, cookie := newAuthorizationTestHandler(t)

	profilePage := doGetWithCookie(t, handler, cookie, "/ui/authorization/profiles")
	profileBody := readResponseBody(t, profilePage)
	if profilePage.StatusCode != http.StatusOK {
		t.Fatalf("Profile 子页状态码=%d 页面：\n%s", profilePage.StatusCode, profileBody)
	}
	for _, want := range []string{
		`href="/ui/authorization/profiles"`, `href="/ui/authorization/secrets"`, `href="/ui/authorization/shares"`,
		"运维", "1 台服务器", "1 个客户端", "新建", `href="/ui/authorization/profiles/profile-1"`,
	} {
		if !strings.Contains(profileBody, want) {
			t.Fatalf("Profile 子页应包含 %q：\n%s", want, profileBody)
		}
	}

	secretPage := doGetWithCookie(t, handler, cookie, "/ui/authorization/secrets")
	secretBody := readResponseBody(t, secretPage)
	if secretPage.StatusCode != http.StatusOK {
		t.Fatalf("机密项子页状态码=%d 页面：\n%s", secretPage.StatusCode, secretBody)
	}
	for _, want := range []string{
		"MATRIX_TOKEN", "浏览器测试机密项", "授权 1 台", "1 个客户端能拿到",
		"1 个机密项缺备注", "去补充", `href="/ui/authorization/secrets/secret-1"`,
	} {
		if !strings.Contains(secretBody, want) {
			t.Fatalf("机密项子页应包含 %q：\n%s", want, secretBody)
		}
	}

	sharePage := doGetWithCookie(t, handler, cookie, "/ui/authorization/shares")
	shareBody := readResponseBody(t, sharePage)
	if sharePage.StatusCode != http.StatusOK {
		t.Fatalf("共享存储子页状态码=%d 页面：\n%s", sharePage.StatusCode, shareBody)
	}
	for _, want := range []string{"datasets", "临时数据", "工作站 提供", "可挂载 1 台", "1 个客户端能拿到"} {
		if !strings.Contains(shareBody, want) {
			t.Fatalf("共享存储子页应包含 %q：\n%s", want, shareBody)
		}
	}
	for _, forbidden := range authorizationForbiddenWords() {
		for name, body := range map[string]string{"Profile": profileBody, "机密项": secretBody, "共享存储": shareBody} {
			if strings.Contains(body, forbidden) {
				t.Fatalf("%s 子页不应出现 %q：\n%s", name, forbidden, body)
			}
		}
	}
}

func TestAuthorizationDetailPagesShowClientAccess(t *testing.T) {
	handler, _, _, cookie := newAuthorizationTestHandler(t)

	secretPage := doGetWithCookie(t, handler, cookie, "/ui/authorization/secrets/secret-1")
	secretBody := readResponseBody(t, secretPage)
	if secretPage.StatusCode != http.StatusOK {
		t.Fatalf("机密项详情状态码=%d 页面：\n%s", secretPage.StatusCode, secretBody)
	}
	for _, want := range []string{
		"浏览器测试机密项", "实际能拿到它的客户端", `href="/ui/clients/client-1"`, "笔记本",
		"经 web-1", `href="/ui/authorization/secrets/secret-1/servers"`, "调整服务器",
	} {
		if !strings.Contains(secretBody, want) {
			t.Fatalf("机密项详情应包含 %q：\n%s", want, secretBody)
		}
	}

	sharePage := doGetWithCookie(t, handler, cookie, "/ui/authorization/shares/share-1")
	shareBody := readResponseBody(t, sharePage)
	if sharePage.StatusCode != http.StatusOK {
		t.Fatalf("共享存储详情状态码=%d 页面：\n%s", sharePage.StatusCode, shareBody)
	}
	for _, want := range []string{
		"临时数据", "实际能拿到它的客户端", `href="/ui/clients/client-1"`, "笔记本", "经 web-1",
		`href="/ui/authorization/shares/share-1/servers"`, "调整服务器",
	} {
		if !strings.Contains(shareBody, want) {
			t.Fatalf("共享存储详情应包含 %q：\n%s", want, shareBody)
		}
	}
	for _, forbidden := range authorizationForbiddenWords() {
		if strings.Contains(shareBody, forbidden) {
			t.Fatalf("共享存储详情不应出现 %q：\n%s", forbidden, shareBody)
		}
	}
}

func TestServerSelectionPageRendersReusableComponent(t *testing.T) {
	handler, _, _, cookie := newAuthorizationTestHandler(t)

	response := doGetWithCookie(t, handler, cookie, "/ui/authorization/secrets/secret-1/servers")
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("服务器勾选页状态码=%d 页面：\n%s", response.StatusCode, body)
	}
	for _, want := range []string{
		"搜索服务器", "按地点筛选", "按网络筛选", "按 profile 筛选",
		"全选当前筛选结果", "新增 0 台、移除 0 台", "下一步",
		`data-server-selection`, `data-server-site="site-home"`, `data-server-network="network-home"`,
		`data-server-profile="profile-1"`, `name="server" value="server-web" checked`,
		`name="expected" value="server-web"`, `name="resource_kind" value="secret"`,
		`name="resource_id" value="secret-1"`, `data-selection-search`, `data-selection-toggle`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("服务器勾选页应包含 %q：\n%s", want, body)
		}
	}
}

func TestServerSelectionConfirmReusesSecretAuthorizationPreview(t *testing.T) {
	handler, _, writer, cookie := newAuthorizationTestHandler(t)
	selection := doGetWithCookie(t, handler, cookie, "/ui/authorization/secrets/secret-1/servers")
	csrf := hiddenInputValue(t, readResponseBody(t, selection), "csrf_token")

	req := httptest.NewRequest(http.MethodPost, "/ui/authorization/selection/preview", strings.NewReader(url.Values{
		"csrf_token":    {csrf},
		"resource_kind": {"secret"},
		"resource_id":   {"secret-1"},
		"return_to":     {"/ui/authorization/secrets/secret-1"},
		"server":        {"server-db"},
	}.Encode()))
	req.Host = "127.0.0.1:18080"
	req.AddCookie(cookie)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:18080")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	response := rec.Result()
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("确认页状态码=%d 页面：\n%s", response.StatusCode, body)
	}
	for _, want := range []string{
		"机密项授权预览", "新增 1 个授权，移除 1 个授权。", "db-1", "web-1",
		"实际能拿到它的客户端如何变化", "笔记本", "经 web-1", "提交后无法拿到",
		`action="/ui/secrets/authorization/confirm"`, `value="secret-1 server-db 1"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("确认页应包含 %q：\n%s", want, body)
		}
	}
	if len(writer.target) == 0 {
		t.Fatalf("确认页应经机密项授权预览来源计算差异")
	}
}

func TestProfileServerSelectionConfirmAndApply(t *testing.T) {
	handler, access, _, cookie := newAuthorizationTestHandler(t)
	selection := doGetWithCookie(t, handler, cookie, "/ui/authorization/profiles/profile-1/servers")
	selectionBody := readResponseBody(t, selection)
	if selection.StatusCode != http.StatusOK {
		t.Fatalf("Profile 勾选页状态码=%d 页面：\n%s", selection.StatusCode, selectionBody)
	}
	csrf := hiddenInputValue(t, selectionBody, "csrf_token")

	req := httptest.NewRequest(http.MethodPost, "/ui/authorization/selection/preview", strings.NewReader(url.Values{
		"csrf_token":    {csrf},
		"resource_kind": {"profile"},
		"resource_id":   {"profile-1"},
		"return_to":     {"/ui/authorization/profiles/profile-1"},
		"server":        {"server-db"},
	}.Encode()))
	req.Host = "127.0.0.1:18080"
	req.AddCookie(cookie)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:18080")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	response := rec.Result()
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("Profile 确认页状态码=%d 页面：\n%s", response.StatusCode, body)
	}
	for _, want := range []string{
		"确认授权", "新增 1 台", "移除 1 台", "db-1", "web-1",
		"实际能拿到它的客户端如何变化", "笔记本", "现在经 web-1", "提交后无法拿到",
		`action="/ui/access/profiles/profile-1/grants"`,
		`name="expected" value="server-web"`, `name="server" value="server-db"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("Profile 确认页应包含 %q：\n%s", want, body)
		}
	}

	confirm := postStandaloneForm(t, handler, cookie, "/ui/access/profiles/profile-1/grants", url.Values{
		"csrf_token": {csrf},
		"expected":   {"server-web"},
		"server":     {"server-db"},
	})
	confirmBody := readResponseBody(t, confirm)
	if confirm.StatusCode != http.StatusSeeOther || confirm.Header.Get("Location") != "/ui/authorization/profiles" {
		t.Fatalf("确认保存应跳回 Profile 子页，状态码=%d 跳转=%q 页面：\n%s",
			confirm.StatusCode, confirm.Header.Get("Location"), confirmBody)
	}
	if access.grantProfileID != "profile-1" {
		t.Fatalf("保存请求应指定 profile，实际 %q", access.grantProfileID)
	}
	if strings.Join(access.grantExpected, ",") != "server-web" || strings.Join(access.grantDesired, ",") != "server-db" {
		t.Fatalf("保存请求应携带期望与目标集合，实际 expected=%v desired=%v",
			access.grantExpected, access.grantDesired)
	}
}

func TestNewSecretFormRequiresNote(t *testing.T) {
	handler, _, _, cookie := newAuthorizationTestHandler(t)

	response := doGetWithCookie(t, handler, cookie, "/ui/authorization/secrets/new")
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("新建机密项状态码=%d 页面：\n%s", response.StatusCode, body)
	}
	for _, want := range []string{
		`<textarea id="secret-note" name="note" required`, `data-note-submit disabled`,
		"agent 只能靠备注判断这个机密项的用途。",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("新建机密项表单应包含 %q：\n%s", want, body)
		}
	}
}

func TestOldAccessRoutesRedirectToAuthorizationPages(t *testing.T) {
	handler, _, _, cookie := newAuthorizationTestHandler(t)

	list := doGetWithCookie(t, handler, cookie, "/ui/access/profiles")
	if list.StatusCode != http.StatusSeeOther || list.Header.Get("Location") != "/ui/authorization/profiles" {
		t.Fatalf("旧 Profile 地址应重定向，状态码=%d 跳转=%q", list.StatusCode, list.Header.Get("Location"))
	}
	grants := doGetWithCookie(t, handler, cookie, "/ui/access/profiles/profile-1/grants?server=server-db")
	if grants.StatusCode != http.StatusSeeOther ||
		grants.Header.Get("Location") != "/ui/authorization/profiles/profile-1/servers?server=server-db" {
		t.Fatalf("旧授权表单地址应重定向并保留勾选，状态码=%d 跳转=%q",
			grants.StatusCode, grants.Header.Get("Location"))
	}
}

func TestHomeShowsMissingNoteTask(t *testing.T) {
	handler, _, _, cookie := newAuthorizationTestHandler(t)

	response := doGetWithCookie(t, handler, cookie, "/ui/")
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("主页状态码=%d 页面：\n%s", response.StatusCode, body)
	}
	for _, want := range []string{"1 个资源缺备注", "1 个机密项缺备注", `href="/ui/authorization/"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("主页应显示缺备注待办 %q：\n%s", want, body)
		}
	}
}
