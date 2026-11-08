package webui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

type stubAccessStore struct {
	profiles       []AccessProfileSummary
	addName        string
	addSrc         AccessWriteSource
	deleteID       string
	deleteSrc      AccessWriteSource
	deleteInUse    *AccessProfileInUse
	deleteErr      error
	grantProfileID string
	grantExpected  []string
	grantDesired   []string
	grantSrc       AccessWriteSource
	grantResult    AccessGrantChange
	grantConflict  *AccessGrantConflict
	grantErr       error

	deviceCodes      []AccessDeviceCodeSummary
	addDeviceName    string
	addDeviceProfile string
	addDeviceURL     string
	addDeviceSrc     AccessWriteSource
	addDeviceResult  AccessDeviceCodeResult
	addDeviceErr     error
	revokeDeviceID   string
	revokeDeviceSrc  AccessWriteSource
	revokeDeviceErr  error
}

func (s *stubAccessStore) ListProfiles() ([]AccessProfileSummary, error) {
	if s.profiles == nil {
		return nil, nil
	}
	return s.profiles, nil
}

func (s *stubAccessStore) AddProfile(name string, source AccessWriteSource) error {
	s.addName = name
	s.addSrc = source
	return nil
}

func (s *stubAccessStore) DeleteProfile(id string, source AccessWriteSource) (*AccessProfileInUse, error) {
	s.deleteID = id
	s.deleteSrc = source
	return s.deleteInUse, s.deleteErr
}

func (s *stubAccessStore) ListDeviceCodes() ([]AccessDeviceCodeSummary, error) {
	if s.deviceCodes == nil {
		return nil, nil
	}
	return s.deviceCodes, nil
}

func (s *stubAccessStore) AddDeviceCode(name, profile, serveURL string, source AccessWriteSource) (AccessDeviceCodeResult, error) {
	s.addDeviceName = name
	s.addDeviceProfile = profile
	s.addDeviceURL = serveURL
	s.addDeviceSrc = source
	return s.addDeviceResult, s.addDeviceErr
}

func (s *stubAccessStore) RevokeDeviceCode(id string, source AccessWriteSource) error {
	s.revokeDeviceID = id
	s.revokeDeviceSrc = source
	return s.revokeDeviceErr
}

func (s *stubAccessStore) SetServerGrants(profileID string, expected, desired []string, source AccessWriteSource) (AccessGrantChange, *AccessGrantConflict, error) {
	s.grantProfileID = profileID
	s.grantExpected = append([]string(nil), expected...)
	s.grantDesired = append([]string(nil), desired...)
	s.grantSrc = source
	return s.grantResult, s.grantConflict, s.grantErr
}

func TestAccessRoutesAndNoticeFollowRole(t *testing.T) {
	standalone, standaloneStartup, err := New(Config{
		Role:    RoleStandalone,
		Servers: &stubServerSource{},
		Access:  &stubAccessStore{},
		Port:    18080,
	})
	if err != nil {
		t.Fatalf("构造单机 Web UI Handler 失败：%v", err)
	}
	standaloneSession := exchangeStartupToken(t, standalone, standaloneStartup.URL("127.0.0.1:18080"))
	standaloneCookie := standaloneSession.Cookies()[0]
	standaloneSession.Body.Close()

	standalonePage := doGetWithCookie(t, standalone, standaloneCookie, "/ui/access/profiles")
	standaloneBody := readResponseBody(t, standalonePage)
	if standalonePage.StatusCode != http.StatusOK || !strings.Contains(standaloneBody, "Profile") {
		t.Fatalf("单机应打开 Profile 页，状态码=%d 页面：\n%s", standalonePage.StatusCode, standaloneBody)
	}
	for _, path := range []string{"/ui/access/pairing", "/ui/access/device-codes"} {
		resp := doGetWithCookie(t, standalone, standaloneCookie, path)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("单机不应有配对或设备码页面，%s 状态码=%d", path, resp.StatusCode)
		}
	}

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
	if clientPage.StatusCode != http.StatusOK || !strings.Contains(clientBody, "profile、配对、设备码和项目令牌在权威端管理") {
		t.Fatalf("客户端访问区域应只有权威端提示，状态码=%d 页面：\n%s", clientPage.StatusCode, clientBody)
	}
	for _, target := range []string{
		"/ui/access/profiles", "/ui/access/profiles/p-1/grants", "/ui/access/pairing",
	} {
		resp := doGetWithCookie(t, client, clientCookie, target)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("客户端不应有访问数据页面，%s 状态码=%d", target, resp.StatusCode)
		}
	}
	clientNotice := doGetWithCookie(t, client, clientCookie, "/ui/access/")
	clientNoticeBody := readResponseBody(t, clientNotice)
	clientCSRF := hiddenInputMetaOrHiddenValue(t, clientNoticeBody)
	for _, target := range []string{
		"/ui/access/profiles", "/ui/access/profiles/delete", "/ui/access/profiles/p-1/grants",
	} {
		resp := postAccessWrite(t, client, clientCookie, target, url.Values{}, "http://127.0.0.1:18080", clientCSRF)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("客户端访问写路由 %s 应返回 404，状态码=%d", target, resp.StatusCode)
		}
	}

	auth := &fakeAuthorityAuth{passwordSet: true, password: "correct-password", now: time.Unix(1800000000, 0)}
	server, _, err := NewAuthority(Config{
		Role:         RoleServer,
		Servers:      &stubServerSource{},
		Access:       &stubAccessStore{},
		Auth:         auth,
		AllowedHosts: []string{"127.0.0.1"},
		Port:         7879,
		Now:          func() time.Time { return auth.now },
	})
	if err != nil {
		t.Fatalf("构造权威端 Web UI Handler 失败：%v", err)
	}
	serverCookie := authoritySuccessfulLogin(t, server, "127.0.0.1:7879", "correct-password")
	serverPage := authorityGet(t, server, "127.0.0.1:7879", "/ui/access/profiles", serverCookie)
	serverBody := readResponseBody(t, serverPage)
	if serverPage.StatusCode != http.StatusOK || !strings.Contains(serverBody, "Profile") {
		t.Fatalf("权威端应打开 Profile 页，状态码=%d 页面：\n%s", serverPage.StatusCode, serverBody)
	}
}

func TestAccessNavigationFollowsRoleCapabilities(t *testing.T) {
	standalonePage := loginAndGetList(t, RoleStandalone, []ServerRow{{Name: "web-1"}}, "")
	for _, want := range []string{
		`<a href="/ui/access/">访问</a>`,
		`<a href="/ui/access/profiles">Profile</a>`,
	} {
		if !strings.Contains(standalonePage, want) {
			t.Fatalf("单机导航应包含 %q：\n%s", want, standalonePage)
		}
	}
	if strings.Contains(standalonePage, "/ui/access/pairing") || strings.Contains(standalonePage, "/ui/access/device-codes") {
		t.Fatalf("单机导航不应显示配对或设备码入口：\n%s", standalonePage)
	}

	clientPage := loginAndGetList(t, RoleClient, []ServerRow{{Name: "web-1"}}, "")
	if !strings.Contains(clientPage, `<a href="/ui/access/">访问</a>`) {
		t.Fatalf("客户端导航应显示访问区域入口：\n%s", clientPage)
	}
	if strings.Contains(clientPage, `<a href="/ui/access/profiles">Profile</a>`) {
		t.Fatalf("客户端导航不应显示 Profile 数据入口：\n%s", clientPage)
	}

	auth := &fakeAuthorityAuth{passwordSet: true, password: "correct-password", now: time.Unix(1800000000, 0)}
	server, _, err := NewAuthority(Config{
		Role: RoleServer, Servers: &stubServerSource{}, Access: &stubAccessStore{}, Auth: auth,
		AllowedHosts: []string{"127.0.0.1"}, Port: 7879,
		Now: func() time.Time { return auth.now },
	})
	if err != nil {
		t.Fatalf("构造权威端 Web UI Handler 失败：%v", err)
	}
	cookie := authoritySuccessfulLogin(t, server, "127.0.0.1:7879", "correct-password")
	serverPageResponse := authorityGet(t, server, "127.0.0.1:7879", "/ui/access/profiles", cookie)
	serverPage := readResponseBody(t, serverPageResponse)
	if !strings.Contains(serverPage, `<a href="/ui/access/profiles">Profile</a>`) {
		t.Fatalf("权威端导航应显示 Profile 入口：\n%s", serverPage)
	}
}

func TestProfilePageListsGrantsAndReferences(t *testing.T) {
	handler, startup, err := New(Config{
		Role: RoleStandalone,
		Servers: &stubServerSource{rows: []ServerRow{
			{Name: "web-1"}, {Name: "db-1"}, {Name: "bastion"},
		}},
		Access: &stubAccessStore{profiles: []AccessProfileSummary{{
			ID:          "profile-1",
			Name:        "运维",
			Servers:     []AccessServerRef{{ID: "server-web", Name: "web-1"}},
			Projects:    []AccessReference{{Name: "项目甲", Status: "active"}},
			DeviceCodes: []AccessReference{{Name: "笔记本", Status: "active"}},
		}}},
		Port: 18080,
	})
	if err != nil {
		t.Fatalf("构造单机 Web UI Handler 失败：%v", err)
	}
	session := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookie := session.Cookies()[0]
	session.Body.Close()

	response := doGetWithCookie(t, handler, cookie, "/ui/access/profiles")
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("Profile 页状态码=%d 页面：\n%s", response.StatusCode, body)
	}
	for _, want := range []string{
		"运维", "web-1", "项目令牌", "1", "设备码", "笔记本",
		`<a href="/ui/access/profiles/profile-1/grants">调整授权</a>`,
		`<button type="submit">删除</button>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("Profile 页应包含 %q：\n%s", want, body)
		}
	}
}

func TestProfilePageAddsProfileThroughAccessStore(t *testing.T) {
	store := &stubAccessStore{}
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

	response := postStandaloneForm(t, handler, cookie, "/ui/access/profiles", url.Values{
		"name": {"team-a"},
	})
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/ui/access/profiles" {
		t.Fatalf("新增 profile 应回到列表，状态码=%d 跳转=%q 页面：\n%s", response.StatusCode, response.Header.Get("Location"), body)
	}
	if store.addName != "team-a" {
		t.Fatalf("新增请求应传给访问管理来源，实际 name=%q", store.addName)
	}
	if store.addSrc.RemoteAddress != "198.51.100.9" {
		t.Fatalf("新增请求应带来源地址，实际 %+v", store.addSrc)
	}
}

func TestProfilePageDeletesProfileThroughAccessStore(t *testing.T) {
	store := &stubAccessStore{}
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

	response := postStandaloneForm(t, handler, cookie, "/ui/access/profiles/delete", url.Values{
		"profile_id": {"profile-1"},
	})
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/ui/access/profiles" {
		t.Fatalf("删除 profile 应回到列表，状态码=%d 跳转=%q 页面：\n%s", response.StatusCode, response.Header.Get("Location"), body)
	}
	if store.deleteID != "profile-1" || store.deleteSrc.RemoteAddress != "198.51.100.9" {
		t.Fatalf("删除请求应传给访问管理来源，实际 id=%q source=%+v", store.deleteID, store.deleteSrc)
	}
}

func TestProfilePageShowsReferencedProfileDeleteRefusal(t *testing.T) {
	store := &stubAccessStore{
		profiles: []AccessProfileSummary{{ID: "profile-1", Name: "运维"}},
		deleteInUse: &AccessProfileInUse{
			ProfileName: "运维",
			Projects:    []string{"项目甲"},
			DeviceCodes: []string{"笔记本"},
		},
	}
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

	response := postStandaloneForm(t, handler, cookie, "/ui/access/profiles/delete", url.Values{
		"profile_id": {"profile-1"},
	})
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("被引用的 profile 删除应返回 409，状态码=%d 页面：\n%s", response.StatusCode, body)
	}
	for _, want := range []string{"仍被引用", "项目甲", "笔记本"} {
		if !strings.Contains(body, want) {
			t.Fatalf("删除被拒页面应包含 %q：\n%s", want, body)
		}
	}
}

func TestGrantFormPrechecksCurrentServers(t *testing.T) {
	handler, startup, err := New(Config{
		Role: RoleStandalone,
		Servers: &stubServerSource{rows: []ServerRow{
			{ID: "server-web", Name: "web-1"}, {ID: "server-db", Name: "db-1"},
		}},
		Access: &stubAccessStore{profiles: []AccessProfileSummary{{
			ID:      "profile-1",
			Name:    "运维",
			Servers: []AccessServerRef{{ID: "server-web", Name: "web-1"}},
		}}},
		Port: 18080,
	})
	if err != nil {
		t.Fatalf("构造单机 Web UI Handler 失败：%v", err)
	}
	session := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookie := session.Cookies()[0]
	session.Body.Close()

	response := doGetWithCookie(t, handler, cookie, "/ui/access/profiles/profile-1/grants")
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("授权表单状态码=%d 页面：\n%s", response.StatusCode, body)
	}
	if !strings.Contains(body, `<input type="checkbox" name="server" value="server-web" checked>`) {
		t.Fatalf("当前已授权服务器应预先勾选：\n%s", body)
	}
	if !strings.Contains(body, `<input type="checkbox" name="server" value="server-db">`) {
		t.Fatalf("未授权服务器不应勾选：\n%s", body)
	}
	if !strings.Contains(body, `<input type="hidden" name="expected" value="server-web">`) {
		t.Fatalf("表单应保存打开时看到的授权集合：\n%s", body)
	}
}

func TestGrantFormSubmitsFullSetAndShowsSummary(t *testing.T) {
	store := &stubAccessStore{profiles: []AccessProfileSummary{{
		ID:      "profile-1",
		Name:    "运维",
		Servers: []AccessServerRef{{ID: "server-web", Name: "web-1"}},
	}}}
	store.grantResult = AccessGrantChange{
		Added:   []AccessServerRef{{ID: "server-db", Name: "db-1"}},
		Removed: []AccessServerRef{{ID: "server-web", Name: "web-1"}},
	}
	handler, startup, err := New(Config{
		Role: RoleStandalone,
		Servers: &stubServerSource{rows: []ServerRow{
			{ID: "server-web", Name: "web-1"}, {ID: "server-db", Name: "db-1"},
		}},
		Access: store,
		Port:   18080,
	})
	if err != nil {
		t.Fatalf("构造单机 Web UI Handler 失败：%v", err)
	}
	session := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookie := session.Cookies()[0]
	session.Body.Close()

	response := postStandaloneForm(t, handler, cookie, "/ui/access/profiles/profile-1/grants", url.Values{
		"expected": {"server-web"}, "server": {"server-db"},
	})
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/ui/access/profiles" {
		t.Fatalf("提交授权应回到列表，状态码=%d 跳转=%q 页面：\n%s", response.StatusCode, response.Header.Get("Location"), body)
	}
	if store.grantProfileID != "profile-1" ||
		strings.Join(store.grantExpected, ",") != "server-web" ||
		strings.Join(store.grantDesired, ",") != "server-db" ||
		store.grantSrc.RemoteAddress != "198.51.100.9" {
		t.Fatalf("授权提交参数不正确：%+v", store)
	}

	list := doGetWithCookie(t, handler, cookie, "/ui/access/profiles")
	listBody := readResponseBody(t, list)
	if !strings.Contains(listBody, "新增授权：db-1；撤销授权：web-1") {
		t.Fatalf("授权提交后应显示结果摘要：\n%s", listBody)
	}
}

func TestGrantConflictRendersLatestServerSet(t *testing.T) {
	store := &stubAccessStore{profiles: []AccessProfileSummary{{
		ID:      "profile-1",
		Name:    "运维",
		Servers: []AccessServerRef{{ID: "server-web", Name: "web-1"}},
	}}}
	store.grantConflict = &AccessGrantConflict{
		ProfileName: "运维",
		Current:     []AccessServerRef{{ID: "server-db", Name: "db-1"}},
	}
	handler, startup, err := New(Config{
		Role: RoleStandalone,
		Servers: &stubServerSource{rows: []ServerRow{
			{ID: "server-web", Name: "web-1"}, {ID: "server-db", Name: "db-1"},
		}},
		Access: store,
		Port:   18080,
	})
	if err != nil {
		t.Fatalf("构造单机 Web UI Handler 失败：%v", err)
	}
	session := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookie := session.Cookies()[0]
	session.Body.Close()

	response := postStandaloneForm(t, handler, cookie, "/ui/access/profiles/profile-1/grants", url.Values{
		"expected": {"server-web"}, "server": {"server-db"},
	})
	body := readResponseBody(t, response)
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("授权冲突应返回 409，状态码=%d 页面：\n%s", response.StatusCode, body)
	}
	if !strings.Contains(body, "授权范围已被其他入口修改") {
		t.Fatalf("授权冲突应说明原因：\n%s", body)
	}
	if !strings.Contains(body, `<input type="checkbox" name="server" value="server-db" checked>`) ||
		!strings.Contains(body, `<input type="checkbox" name="server" value="server-web">`) ||
		!strings.Contains(body, `<input type="hidden" name="expected" value="server-db">`) {
		t.Fatalf("授权冲突应用最新授权范围重新渲染表单：\n%s", body)
	}
}

func TestAuthorityGrantAdditionRequiresReverifyAndPreservesSelection(t *testing.T) {
	auth := &fakeAuthorityAuth{passwordSet: true, password: "correct-password", now: time.Unix(1800000000, 0)}
	store := &stubAccessStore{profiles: []AccessProfileSummary{{
		ID:      "profile-1",
		Name:    "运维",
		Servers: []AccessServerRef{{ID: "server-web", Name: "web-1"}},
	}}}
	handler, _, err := NewAuthority(Config{
		Role: RoleServer,
		Servers: &stubServerSource{rows: []ServerRow{
			{ID: "server-web", Name: "web-1"}, {ID: "server-db", Name: "db-1"},
		}},
		Access:                 store,
		Auth:                   auth,
		AllowedHosts:           []string{"127.0.0.1"},
		Port:                   7879,
		CertificateFingerprint: "sha256:0123456789abcdef",
		Now:                    func() time.Time { return auth.now },
	})
	if err != nil {
		t.Fatalf("构造权威端 Web UI Handler 失败：%v", err)
	}
	cookie := authoritySuccessfulLogin(t, handler, "127.0.0.1:7879", "correct-password")
	auth.now = auth.now.Add(6 * time.Minute)

	grantsPage := authorityGet(t, handler, "127.0.0.1:7879", "/ui/access/profiles/profile-1/grants", cookie)
	grantsBody := readResponseBody(t, grantsPage)
	resumePath := "/ui/access/profiles/profile-1/grants?server=server-db"
	wantReverifyLink := `/ui/reverify?next=` + url.QueryEscape("/ui/access/profiles/profile-1/grants?server=server-web")
	if !strings.Contains(grantsBody, wantReverifyLink) {
		t.Fatalf("未重新验证的授权表单应链接重新验证页 %q：\n%s", wantReverifyLink, grantsBody)
	}
	csrfToken := hiddenInputValue(t, grantsBody, "csrf_token")

	rejected := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/access/profiles/profile-1/grants", url.Values{
		"csrf_token": {csrfToken}, "expected": {"server-web"}, "server": {"server-db"},
	}, cookie)
	rejectedBody := readResponseBody(t, rejected)
	rejectedReverifyLink := `/ui/reverify?next=` + url.QueryEscape(resumePath)
	if rejected.StatusCode != http.StatusSeeOther || rejected.Header.Get("Location") != rejectedReverifyLink {
		t.Fatalf("含新增授权的提交应先重新验证，状态码=%d 跳转=%q 页面：\n%s", rejected.StatusCode, rejected.Header.Get("Location"), rejectedBody)
	}
	if store.grantProfileID != "" {
		t.Fatalf("未重新验证时不应调用访问管理模块，实际调用 profile=%q", store.grantProfileID)
	}

	reverifyPage := authorityGet(t, handler, "127.0.0.1:7879", wantReverifyLink, cookie)
	reverifyBody := readResponseBody(t, reverifyPage)
	reverifyCSRF := hiddenInputValue(t, reverifyBody, "csrf_token")
	reverified := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/reverify", url.Values{
		"csrf_token": {reverifyCSRF}, "password": {"correct-password"}, "next": {resumePath},
	}, cookie)
	reverifiedBody := readResponseBody(t, reverified)
	if reverified.StatusCode != http.StatusSeeOther || reverified.Header.Get("Location") != resumePath {
		t.Fatalf("重新验证后应回到保留选择的授权表单，状态码=%d 跳转=%q 页面：\n%s", reverified.StatusCode, reverified.Header.Get("Location"), reverifiedBody)
	}

	resumed := authorityGet(t, handler, "127.0.0.1:7879", resumePath, cookie)
	resumedBody := readResponseBody(t, resumed)
	if !strings.Contains(resumedBody, `<input type="checkbox" name="server" value="server-db" checked>`) ||
		!strings.Contains(resumedBody, `<input type="checkbox" name="server" value="server-web">`) {
		t.Fatalf("重新验证后应保留提交时选择的服务器：\n%s", resumedBody)
	}
	resumedCSRF := hiddenInputValue(t, resumedBody, "csrf_token")
	store.grantResult = AccessGrantChange{Added: []AccessServerRef{{ID: "server-db", Name: "db-1"}}}
	accepted := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/access/profiles/profile-1/grants", url.Values{
		"csrf_token": {resumedCSRF}, "expected": {"server-web"}, "server": {"server-db"},
	}, cookie)
	acceptedBody := readResponseBody(t, accepted)
	if accepted.StatusCode != http.StatusSeeOther || store.grantProfileID != "profile-1" {
		t.Fatalf("重新验证后应提交授权，状态码=%d 调用=%q 页面：\n%s", accepted.StatusCode, store.grantProfileID, acceptedBody)
	}
}

func TestAuthorityProfileReverificationRules(t *testing.T) {
	auth := &fakeAuthorityAuth{passwordSet: true, password: "correct-password", now: time.Unix(1800000000, 0)}
	store := &stubAccessStore{profiles: []AccessProfileSummary{{
		ID:      "profile-1",
		Name:    "运维",
		Servers: []AccessServerRef{{ID: "server-web", Name: "web-1"}, {ID: "server-db", Name: "db-1"}},
	}}}
	handler, _, err := NewAuthority(Config{
		Role: RoleServer,
		Servers: &stubServerSource{rows: []ServerRow{
			{ID: "server-web", Name: "web-1"}, {ID: "server-db", Name: "db-1"},
		}},
		Access:                 store,
		Auth:                   auth,
		AllowedHosts:           []string{"127.0.0.1"},
		Port:                   7879,
		CertificateFingerprint: "sha256:0123456789abcdef",
		Now:                    func() time.Time { return auth.now },
	})
	if err != nil {
		t.Fatalf("构造权威端 Web UI Handler 失败：%v", err)
	}
	cookie := authoritySuccessfulLogin(t, handler, "127.0.0.1:7879", "correct-password")
	auth.now = auth.now.Add(6 * time.Minute)
	profilesPage := authorityGet(t, handler, "127.0.0.1:7879", "/ui/access/profiles", cookie)
	profilesBody := readResponseBody(t, profilesPage)
	csrfToken := hiddenInputValue(t, profilesBody, "csrf_token")

	added := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/access/profiles", url.Values{
		"csrf_token": {csrfToken}, "name": {"team-a"},
	}, cookie)
	addedBody := readResponseBody(t, added)
	if added.StatusCode != http.StatusSeeOther || store.addName != "team-a" {
		t.Fatalf("新增 profile 不应要求重新验证，状态码=%d 调用=%q 页面：\n%s", added.StatusCode, store.addName, addedBody)
	}

	revoked := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/access/profiles/profile-1/grants", url.Values{
		"csrf_token": {csrfToken}, "expected": {"server-web", "server-db"}, "server": {"server-web"},
	}, cookie)
	revokedBody := readResponseBody(t, revoked)
	if revoked.StatusCode != http.StatusSeeOther || store.grantProfileID != "profile-1" {
		t.Fatalf("纯撤销不应要求重新验证，状态码=%d 调用=%q 页面：\n%s", revoked.StatusCode, store.grantProfileID, revokedBody)
	}

	store.deleteID = ""
	deleteRejected := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/access/profiles/delete", url.Values{
		"csrf_token": {csrfToken}, "profile_id": {"profile-1"},
	}, cookie)
	deleteRejectedBody := readResponseBody(t, deleteRejected)
	resumePath := "/ui/access/profiles?reverify_delete=profile-1"
	wantRedirect := `/ui/reverify?next=` + url.QueryEscape(resumePath)
	if deleteRejected.StatusCode != http.StatusSeeOther || deleteRejected.Header.Get("Location") != wantRedirect || store.deleteID != "" {
		t.Fatalf("删除 profile 应先重新验证，状态码=%d 跳转=%q 调用=%q 页面：\n%s", deleteRejected.StatusCode, deleteRejected.Header.Get("Location"), store.deleteID, deleteRejectedBody)
	}

	reverifyPage := authorityGet(t, handler, "127.0.0.1:7879", wantRedirect, cookie)
	reverifyBody := readResponseBody(t, reverifyPage)
	reverifyCSRF := hiddenInputValue(t, reverifyBody, "csrf_token")
	reverified := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/reverify", url.Values{
		"csrf_token": {reverifyCSRF}, "password": {"correct-password"}, "next": {resumePath},
	}, cookie)
	reverifiedBody := readResponseBody(t, reverified)
	if reverified.StatusCode != http.StatusSeeOther || reverified.Header.Get("Location") != resumePath {
		t.Fatalf("重新验证后应回到 Profile 页，状态码=%d 跳转=%q 页面：\n%s", reverified.StatusCode, reverified.Header.Get("Location"), reverifiedBody)
	}

	resumed := authorityGet(t, handler, "127.0.0.1:7879", resumePath, cookie)
	resumedBody := readResponseBody(t, resumed)
	if !strings.Contains(resumedBody, "删除 profile 前需要重新验证管理员口令") {
		t.Fatalf("重新验证返回页应提示删除表单已可提交：\n%s", resumedBody)
	}
	deleted := authorityPostForm(t, handler, "127.0.0.1:7879", "/ui/access/profiles/delete", url.Values{
		"csrf_token": {csrfToken}, "profile_id": {"profile-1"},
	}, cookie)
	deletedBody := readResponseBody(t, deleted)
	if deleted.StatusCode != http.StatusSeeOther || store.deleteID != "profile-1" {
		t.Fatalf("重新验证后应能删除 profile，状态码=%d 调用=%q 页面：\n%s", deleted.StatusCode, store.deleteID, deletedBody)
	}
}

func TestAccessWriteRoutesRequireOriginAndCSRFToken(t *testing.T) {
	handler, startup, err := New(Config{
		Role:    RoleStandalone,
		Servers: &stubServerSource{},
		Access:  &stubAccessStore{},
		Port:    18080,
	})
	if err != nil {
		t.Fatalf("构造单机 Web UI Handler 失败：%v", err)
	}
	session := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
	cookie := session.Cookies()[0]
	session.Body.Close()
	page := doGetWithCookie(t, handler, cookie, "/ui/access/profiles")
	pageBody := readResponseBody(t, page)
	csrfToken := hiddenInputValue(t, pageBody, "csrf_token")

	routes := []struct {
		target string
		form   url.Values
	}{
		{"/ui/access/profiles", url.Values{"name": {"team-a"}}},
		{"/ui/access/profiles/delete", url.Values{"profile_id": {"profile-1"}}},
		{"/ui/access/profiles/profile-1/grants", url.Values{"expected": {"server-web"}, "server": {"server-db"}}},
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
}

func TestWritableRolesRequireAccessStore(t *testing.T) {
	_, _, err := New(Config{
		Role:    RoleStandalone,
		Servers: &stubServerSource{},
		Port:    18080,
	})
	if err == nil || !strings.Contains(err.Error(), "访问管理来源") {
		t.Fatalf("单机缺少访问管理来源应报错，实际：%v", err)
	}

	_, _, err = NewAuthority(Config{
		Role:         RoleServer,
		Servers:      &stubServerSource{},
		Auth:         &fakeAuthorityAuth{},
		AllowedHosts: []string{"127.0.0.1"},
		Port:         7879,
	})
	if err == nil || !strings.Contains(err.Error(), "访问管理来源") {
		t.Fatalf("权威端缺少访问管理来源应报错，实际：%v", err)
	}

	_, _, err = New(Config{
		Role:    RoleClient,
		Servers: &stubServerSource{},
		Port:    18080,
	})
	if err != nil {
		t.Fatalf("客户端不需要访问管理数据，构造不应报错：%v", err)
	}
}

func postAccessWrite(t *testing.T, handler http.Handler, cookie *http.Cookie, target string, form url.Values, origin, csrfToken string) *http.Response {
	t.Helper()
	if csrfToken != "" {
		form.Set("csrf_token", csrfToken)
	}
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Host = "127.0.0.1:18080"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", origin)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Result()
}

func doGetWithCookie(t *testing.T, handler http.Handler, cookie *http.Cookie, target string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = "127.0.0.1:18080"
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Result()
}

func readResponseBody(t *testing.T, response *http.Response) string {
	t.Helper()
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatalf("读取响应正文失败：%v", err)
	}
	return string(body)
}

func postStandaloneForm(t *testing.T, handler http.Handler, cookie *http.Cookie, target string, form url.Values) *http.Response {
	t.Helper()
	page := doGetWithCookie(t, handler, cookie, "/ui/access/profiles")
	pageBody := readResponseBody(t, page)
	match := regexp.MustCompile(`<input type="hidden" name="csrf_token" value="([^"]+)"`).FindStringSubmatch(pageBody)
	if match == nil {
		t.Fatalf("Profile 页缺少防跨站请求伪造令牌：\n%s", pageBody)
	}
	form.Set("csrf_token", match[1])
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Host = "127.0.0.1:18080"
	req.RemoteAddr = "198.51.100.9:45678"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://127.0.0.1:18080")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Result()
}

func hiddenInputValue(t *testing.T, body, name string) string {
	t.Helper()
	pattern := regexp.MustCompile(`<input type="hidden" name="` + regexp.QuoteMeta(name) + `" value="([^"]*)">`)
	match := pattern.FindStringSubmatch(body)
	if match == nil {
		t.Fatalf("页面缺少隐藏字段 %q：\n%s", name, body)
	}
	return match[1]
}

func hiddenInputMetaOrHiddenValue(t *testing.T, body string) string {
	t.Helper()
	match := regexp.MustCompile(`<meta name="csrf-token" content="([^"]+)">`).FindStringSubmatch(body)
	if match == nil {
		t.Fatalf("页面缺少防跨站请求伪造令牌：\n%s", body)
	}
	return match[1]
}
