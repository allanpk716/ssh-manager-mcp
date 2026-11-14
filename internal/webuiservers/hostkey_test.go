package webuiservers

import (
	"crypto/ed25519"
	"crypto/rand"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/access"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/servermanage"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
	"ssh-manager-mcp/internal/webui"
	"ssh-manager-mcp/internal/webuiaccess"
)

// hostKeyFixture 是主机密钥锚测试用的一把主机密钥与指纹。
type hostKeyFixture struct {
	key         ssh.PublicKey
	fingerprint string
}

func newHostKeyFixture(t *testing.T) hostKeyFixture {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return hostKeyFixture{key: key, fingerprint: ssh.FingerprintSHA256(key)}
}

// newHostKeyVault 建一个带真实临时 vault 的测试环境，并登记同一台设备上的
// 三个服务器条目：root、worker 与 bare（尚无锚）。
func newHostKeyVault(t *testing.T) (*store.Store, string, string, string) {
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
	device, err := st.AddDevice("gpu-box", "GPU 工作站", "")
	if err != nil {
		t.Fatal(err)
	}
	newServer := func(name, host string) string {
		id, err := st.AddServer(&models.Server{
			Name: name, Host: host, Port: 22, User: "root", DeviceID: device.ID,
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	return st, newServer("root-entry", "192.0.2.10"), newServer("worker-entry", "192.0.2.11"), newServer("bare-entry", "192.0.2.12")
}

func hostKeyServerOf(t *testing.T, st *store.Store, id string) *models.Server {
	t.Helper()
	srv, err := st.GetServer(id)
	if err != nil || srv == nil {
		t.Fatalf("取服务器条目 %s 失败：%v %v", id, srv, err)
	}
	return srv
}

func hostKeyPage(t *testing.T, handler http.Handler, cookie *http.Cookie, path string) string {
	t.Helper()
	// 指纹里的加号会被 html/template 转义成实体；按页面可见内容断言前先
	// 还原。
	return html.UnescapeString(authorizationBody(t, authorizationRequest(handler, cookie, http.MethodGet, path, nil)))
}

func hostKeyCSRF(t *testing.T, handler http.Handler, cookie *http.Cookie, serverID string) string {
	t.Helper()
	page := hostKeyPage(t, handler, cookie, "/ui/servers/"+serverID)
	return authorizationHiddenOne(page, "csrf_token")
}

func hostKeyPost(
	t *testing.T, handler http.Handler, cookie *http.Cookie, path string, form url.Values,
) *http.Response {
	t.Helper()
	return authorizationRequest(handler, cookie, http.MethodPost, path, form)
}

func hostKeyForm(csrf string, extra ...[2]string) url.Values {
	form := url.Values{"csrf_token": {csrf}}
	for _, item := range extra {
		form.Set(item[0], item[1])
	}
	return form
}

func hostKeyAuditRows(t *testing.T, st *store.Store, action string) []store.AuditRow {
	t.Helper()
	rows, err := st.QueryAudit(store.AuditFilter{Actions: []string{action}})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// TestServerDetailOverwritesAndClearsHostKeyAnchor 校验单机详情页的锚覆盖
// 与删除：覆盖语义与 servers pin-hostkey --fingerprint --force 一致，删除
// 要先确认；两者审计来源都是 Web UI，兄弟条目的锚不受影响。
func TestServerDetailOverwritesAndClearsHostKeyAnchor(t *testing.T) {
	st, rootID, workerID, _ := newHostKeyVault(t)
	oldAnchor := newHostKeyFixture(t)
	newAnchor := newHostKeyFixture(t)
	workerAnchor := newHostKeyFixture(t)
	seed := func(id string, anchor hostKeyFixture) {
		server := hostKeyServerOf(t, st, id)
		if err := st.SaveEntryPinAtHandshake(server, server.Host+":22", anchor.key.Marshal()); err != nil {
			t.Fatal(err)
		}
	}
	seed(rootID, oldAnchor)
	seed(workerID, workerAnchor)
	handler, cookie := newAuthorizationTestHandler(t, st)
	csrf := hostKeyCSRF(t, handler, cookie, rootID)

	page := hostKeyPage(t, handler, cookie, "/ui/servers/"+rootID)
	for _, want := range []string{
		"/ui/servers/" + rootID + "/hostkey-pin", "覆盖主机密钥锚",
		"/ui/servers/" + rootID + "/hostkey-clear", "确认删除主机密钥锚",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("详情页应提供主机密钥锚覆盖与删除表单 %q：\n%s", want, page)
		}
	}

	unconfirmed := hostKeyPost(t, handler, cookie, "/ui/servers/"+rootID+"/hostkey-clear", hostKeyForm(csrf))
	unconfirmedBody := authorizationBody(t, unconfirmed)
	if unconfirmed.StatusCode != http.StatusBadRequest || !strings.Contains(unconfirmedBody, "请先勾选确认删除主机密钥锚") {
		t.Fatalf("未确认的删除应被拒绝，状态码=%d：\n%s", unconfirmed.StatusCode, unconfirmedBody)
	}
	if pin, err := st.GetEntryPinMeta(rootID); err != nil || pin == nil {
		t.Fatalf("未确认的删除不得改动锚：%v %+v", err, pin)
	}

	invalid := hostKeyPost(t, handler, cookie, "/ui/servers/"+rootID+"/hostkey-pin",
		hostKeyForm(csrf, [2]string{"fingerprint", "notafp"}))
	invalidBody := authorizationBody(t, invalid)
	if invalid.StatusCode != http.StatusBadRequest || !strings.Contains(invalidBody, "主机密钥指纹必须是") {
		t.Fatalf("非法指纹应被拒绝并说明格式，状态码=%d：\n%s", invalid.StatusCode, invalidBody)
	}
	if rows := hostKeyAuditRows(t, st, "pin-manual"); len(rows) != 0 {
		t.Fatalf("被拒绝的覆盖不得写审计：%+v", rows)
	}

	overwritten := hostKeyPost(t, handler, cookie, "/ui/servers/"+rootID+"/hostkey-pin",
		hostKeyForm(csrf, [2]string{"fingerprint", newAnchor.fingerprint}))
	if overwritten.StatusCode != http.StatusSeeOther ||
		overwritten.Header.Get("Location") != "/ui/servers/"+rootID+"?hostkey=updated" {
		t.Fatalf("覆盖锚后应回到详情页，状态码=%d 跳转=%q", overwritten.StatusCode, overwritten.Header.Get("Location"))
	}
	result := hostKeyPage(t, handler, cookie, overwritten.Header.Get("Location"))
	for _, want := range []string{"已覆盖主机密钥锚。", newAnchor.fingerprint, "手工锚定"} {
		if !strings.Contains(result, want) {
			t.Fatalf("覆盖后的详情页缺少 %q：\n%s", want, result)
		}
	}
	pin, err := st.GetEntryPinMeta(rootID)
	if err != nil || pin == nil || pin.Format != store.PinFormatFingerprint || string(pin.Blob) != newAnchor.fingerprint {
		t.Fatalf("覆盖后的锚存储不正确：%v %+v", err, pin)
	}
	if workerPin, err := st.GetEntryPinMeta(workerID); err != nil || workerPin == nil ||
		!workerPin.Matches(workerAnchor.key.Marshal()) {
		t.Fatalf("兄弟条目的锚不受覆盖影响：%v %+v", err, workerPin)
	}
	rows := hostKeyAuditRows(t, st, "pin-manual")
	if len(rows) != 1 || rows[0].Source != store.AuditSourceWebUI ||
		!strings.Contains(rows[0].Command, "fingerprint="+newAnchor.fingerprint) ||
		!strings.Contains(rows[0].Command, "forced=true") ||
		!strings.Contains(rows[0].Command, "was="+oldAnchor.fingerprint) {
		t.Fatalf("覆盖审计应沿用 pin-hostkey 习惯并标记 Web UI 来源：%+v", rows)
	}

	cleared := hostKeyPost(t, handler, cookie, "/ui/servers/"+rootID+"/hostkey-clear",
		hostKeyForm(csrf, [2]string{"confirm", "1"}))
	if cleared.StatusCode != http.StatusSeeOther ||
		cleared.Header.Get("Location") != "/ui/servers/"+rootID+"?hostkey=cleared" {
		t.Fatalf("删除锚后应回到详情页，状态码=%d 跳转=%q", cleared.StatusCode, cleared.Header.Get("Location"))
	}
	clearedPage := hostKeyPage(t, handler, cookie, cleared.Header.Get("Location"))
	for _, want := range []string{"已删除主机密钥锚。", "尚未锚定"} {
		if !strings.Contains(clearedPage, want) {
			t.Fatalf("删除后的详情页缺少 %q：\n%s", want, clearedPage)
		}
	}
	if pin, err := st.GetEntryPinMeta(rootID); err != nil || pin != nil {
		t.Fatalf("删除后条目不应再有锚：%v %+v", err, pin)
	}
	clearRows := hostKeyAuditRows(t, st, "pin-clear")
	if len(clearRows) != 1 || clearRows[0].Source != store.AuditSourceWebUI ||
		!strings.Contains(clearRows[0].Command, "fingerprint="+newAnchor.fingerprint) ||
		!strings.HasSuffix(clearRows[0].Command, "handshake=-") {
		t.Fatalf("删除审计应携带被删指纹并沿用命令行的空地址占位：%+v", clearRows)
	}

	// 条目尚无锚时不提供首次锚定入口；覆盖路由保留，但必须拒绝非覆盖提交。
	created := hostKeyPost(t, handler, cookie, "/ui/servers/"+rootID+"/hostkey-pin",
		hostKeyForm(csrf, [2]string{"fingerprint", oldAnchor.fingerprint}))
	createdBody := authorizationBody(t, created)
	if created.StatusCode != http.StatusBadRequest ||
		!strings.Contains(createdBody, "条目尚无主机密钥锚，不能覆盖") {
		t.Fatalf("尚无锚时不应提供首次锚定入口，状态码=%d：\n%s", created.StatusCode, createdBody)
	}
	if rows = hostKeyAuditRows(t, st, "pin-manual"); len(rows) != 1 {
		t.Fatalf("尚无锚时被拒绝的提交不得写锚定审计：%+v", rows)
	}
	if pin, err := st.GetEntryPinMeta(rootID); err != nil || pin != nil {
		t.Fatalf("尚无锚时被拒绝的提交不得创建锚：%v %+v", err, pin)
	}
}

// TestServerDetailShowsOwnHostKeyAnchor 校验详情页主机密钥锚信息区：显示本
// 条目自己的指纹、来源、学到锚的地址与锚定时间；同设备兄弟条目不合并；尚
// 未锚定的条目明确说明。
func TestServerDetailShowsOwnHostKeyAnchor(t *testing.T) {
	st, rootID, workerID, bareID := newHostKeyVault(t)
	rootAnchor := newHostKeyFixture(t)
	workerAnchor := newHostKeyFixture(t)
	if err := st.SaveEntryPinAtHandshake(hostKeyServerOf(t, st, rootID), "192.0.2.10:22", rootAnchor.key.Marshal()); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveEntryPinAtHandshake(hostKeyServerOf(t, st, workerID), "192.0.2.11:2200", workerAnchor.key.Marshal()); err != nil {
		t.Fatal(err)
	}
	handler, cookie := newAuthorizationTestHandler(t, st)

	rootPage := hostKeyPage(t, handler, cookie, "/ui/servers/"+rootID)
	for _, want := range []string{
		"主机密钥锚", rootAnchor.fingerprint, "主机密钥算法", rootAnchor.key.Type(),
		"首次信任", "学到锚的地址", "192.0.2.10:22", "锚定时间",
	} {
		if !strings.Contains(rootPage, want) {
			t.Fatalf("root 条目详情页主机密钥锚信息缺少 %q：\n%s", want, rootPage)
		}
	}
	if strings.Contains(rootPage, workerAnchor.fingerprint) {
		t.Fatalf("root 条目不得显示兄弟条目的锚：\n%s", rootPage)
	}

	workerPage := hostKeyPage(t, handler, cookie, "/ui/servers/"+workerID)
	for _, want := range []string{workerAnchor.fingerprint, "192.0.2.11:2200"} {
		if !strings.Contains(workerPage, want) {
			t.Fatalf("worker 条目详情页应显示自己的锚与学到锚的地址 %q：\n%s", want, workerPage)
		}
	}
	if strings.Contains(workerPage, rootAnchor.fingerprint) {
		t.Fatalf("worker 条目不得显示兄弟条目的锚：\n%s", workerPage)
	}

	barePage := hostKeyPage(t, handler, cookie, "/ui/servers/"+bareID)
	if !strings.Contains(barePage, "尚未锚定") {
		t.Fatalf("尚无锚的条目必须明确显示尚未锚定：\n%s", barePage)
	}
	if strings.Contains(barePage, rootAnchor.fingerprint) {
		t.Fatalf("尚无锚的条目不得显示兄弟条目的锚：\n%s", barePage)
	}
	if !strings.Contains(barePage, "尚未锚定。首次信任或锚定转发") ||
		strings.Contains(barePage, "首次连接即信任") {
		t.Fatalf("尚无锚的说明必须使用领域词「首次信任」：\n%s", barePage)
	}
	for _, forbidden := range []string{"锚定主机密钥", "hostkey-pin"} {
		if strings.Contains(barePage, forbidden) {
			t.Fatalf("尚无锚的条目不得提供首次锚定入口 %q：\n%s", forbidden, barePage)
		}
	}
}

// hostKeyAuthorityEnv 是权威端主机密钥锚测试环境：带可调时钟、已登录的
// 管理员会话与写路由。
type hostKeyAuthorityEnv struct {
	handler http.Handler
	now     time.Time
	session *http.Cookie
}

func newHostKeyAuthorityEnv(t *testing.T, st *store.Store) *hostKeyAuthorityEnv {
	t.Helper()
	env := &hostKeyAuthorityEnv{now: time.Now()}
	authStore := vault.NewWebUIAuthStore(st)
	if _, err := authStore.SetAdminPasswordWithAudit([]byte("correct-password"), webui.AuditEvent{
		Action: "admin.password_change", Status: "ok",
	}); err != nil {
		t.Fatal(err)
	}
	handler, _, err := webui.NewAuthority(webui.Config{
		Role: webui.RoleServer, Port: 18086,
		Servers:                FromManager(servermanage.New(st)),
		ServerWriter:           WriterFromManager(servermanage.New(st)),
		Access:                 webuiaccess.FromManager(access.New(st)),
		Auth:                   authStore,
		AllowedHosts:           []string{"127.0.0.1"},
		Now:                    func() time.Time { return env.now },
		CertificateFingerprint: "sha256:0123456789abcdef",
	})
	if err != nil {
		t.Fatal(err)
	}
	env.handler = handler
	loginPage := hostKeyAuthorityRequest(env, nil, http.MethodGet, "/ui/login", nil)
	loginCSRF := authorizationHiddenOne(authorizationBody(t, loginPage), "csrf_token")
	var loginCookie *http.Cookie
	for _, cookie := range loginPage.Cookies() {
		loginCookie = cookie
	}
	loggedIn := hostKeyAuthorityRequest(env, loginCookie, http.MethodPost, "/ui/login", url.Values{
		"csrf_token": {loginCSRF}, "password": {"correct-password"},
	})
	if loggedIn.StatusCode != http.StatusSeeOther {
		t.Fatalf("权威端登录失败，状态码=%d：%s", loggedIn.StatusCode, authorizationBody(t, loggedIn))
	}
	for _, cookie := range loggedIn.Cookies() {
		if cookie.Name == "__Host-sshmgr_ui" {
			env.session = cookie
		}
	}
	if env.session == nil {
		t.Fatal("权威端登录后应得到会话 cookie")
	}
	return env
}

// hostKeyAuthorityRequest 按权威端监听端口发起请求；来源地址固定为远程客
// 户端地址，写请求带同源 Origin。
func hostKeyAuthorityRequest(
	env *hostKeyAuthorityEnv, cookie *http.Cookie, method, target string, form url.Values,
) *http.Response {
	body := strings.NewReader("")
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	request := httptest.NewRequest(method, target, body)
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
	env.handler.ServeHTTP(recorder, request)
	return recorder.Result()
}

// TestAuthorityHostKeyAnchorWritesRequireRecentPasswordVerification 校验权威
// 端覆盖与删除锚都要求最近 5 分钟内重新输入过管理员口令；重新验证后回到
// 详情页，重新提交即可生效。
func TestAuthorityHostKeyAnchorWritesRequireRecentPasswordVerification(t *testing.T) {
	st, rootID, _, _ := newHostKeyVault(t)
	anchor := newHostKeyFixture(t)
	replacement := newHostKeyFixture(t)
	if err := st.SaveEntryPinAtHandshake(hostKeyServerOf(t, st, rootID), "192.0.2.10:22", anchor.key.Marshal()); err != nil {
		t.Fatal(err)
	}
	env := newHostKeyAuthorityEnv(t, st)
	detail := hostKeyAuthorityRequest(env, env.session, http.MethodGet, "/ui/servers/"+rootID, nil)
	csrf := authorizationHiddenOne(authorizationBody(t, detail), "csrf_token")
	env.now = env.now.Add(6 * time.Minute)

	for _, target := range []string{"/ui/servers/" + rootID + "/hostkey-pin", "/ui/servers/" + rootID + "/hostkey-clear"} {
		form := hostKeyForm(csrf)
		if strings.HasSuffix(target, "hostkey-pin") {
			form.Set("fingerprint", replacement.fingerprint)
		} else {
			form.Set("confirm", "1")
		}
		rejected := hostKeyAuthorityRequest(env, env.session, http.MethodPost, target, form)
		rejectedBody := authorizationBody(t, rejected)
		if rejected.StatusCode != http.StatusSeeOther ||
			rejected.Header.Get("Location") != "/ui/reverify?next="+url.QueryEscape("/ui/servers/"+rootID) {
			t.Fatalf("超过重新验证窗口的 %s 应跳转重新验证，状态码=%d 跳转=%q：\n%s",
				target, rejected.StatusCode, rejected.Header.Get("Location"), rejectedBody)
		}
	}
	if pin, err := st.GetEntryPinMeta(rootID); err != nil || pin == nil || !pin.Matches(anchor.key.Marshal()) {
		t.Fatalf("重新验证前不得改动锚：%v %+v", err, pin)
	}
	for _, action := range []string{"pin-manual", "pin-clear"} {
		if rows := hostKeyAuditRows(t, st, action); len(rows) != 0 {
			t.Fatalf("重新验证前不得写 %s 审计：%+v", action, rows)
		}
	}

	next := "/ui/servers/" + rootID
	reverifyPage := hostKeyAuthorityRequest(env, env.session, http.MethodGet,
		"/ui/reverify?next="+url.QueryEscape(next), nil)
	reverifyCSRF := authorizationHiddenOne(authorizationBody(t, reverifyPage), "csrf_token")
	verified := hostKeyAuthorityRequest(env, env.session, http.MethodPost, "/ui/reverify", hostKeyForm(
		reverifyCSRF, [2]string{"password", "correct-password"}, [2]string{"next", next},
	))
	if verified.StatusCode != http.StatusSeeOther || verified.Header.Get("Location") != next {
		t.Fatalf("重新验证后应回到服务器详情页，状态码=%d 跳转=%q", verified.StatusCode, verified.Header.Get("Location"))
	}

	confirmed := hostKeyAuthorityRequest(env, env.session, http.MethodPost, "/ui/servers/"+rootID+"/hostkey-pin",
		hostKeyForm(csrf, [2]string{"fingerprint", replacement.fingerprint}))
	if confirmed.StatusCode != http.StatusSeeOther ||
		confirmed.Header.Get("Location") != "/ui/servers/"+rootID+"?hostkey=updated" {
		t.Fatalf("重新验证后覆盖仍失败，状态码=%d 跳转=%q", confirmed.StatusCode, confirmed.Header.Get("Location"))
	}
	if pin, err := st.GetEntryPinMeta(rootID); err != nil || pin == nil || string(pin.Blob) != replacement.fingerprint {
		t.Fatalf("重新验证后覆盖结果不正确：%v %+v", err, pin)
	}
}

// TestClientServerDetailShowsReadOnlyHostKeyAnchorArea 校验客户端详情页按
// 本地快照只读显示主机密钥锚，覆盖与删除写路由不存在。
func TestClientServerDetailShowsReadOnlyHostKeyAnchorArea(t *testing.T) {
	detail := &webui.ServerDetailView{
		ID: "srv-web", Name: "web", User: "deploy", Host: "192.0.2.10", Port: 22,
		ShowHostKeyAnchor: true,
		HostKeyAnchor: &webui.ServerDetailHostKeyAnchor{
			Fingerprint:   "SHA256:" + strings.Repeat("a", 43),
			KeyAlgorithm:  store.PinUndeterminedKeyAlgorithm,
			SourceLabel:   "首次信任",
			Device:        "office-laptop",
			HandshakeAddr: "192.0.2.10:22",
			PinnedAt:      time.Unix(1760000800, 0),
		},
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
		t.Fatalf("应签发客户端会话 cookie，实际 %d", len(cookies))
	}
	page := hostKeyPage(t, handler, cookies[0], "/ui/servers/srv-web")
	for _, want := range []string{
		"主机密钥锚", "SHA256:" + strings.Repeat("a", 43), "主机密钥算法",
		store.PinUndeterminedKeyAlgorithm, "首次信任",
		"office-laptop", "192.0.2.10:22", "锚定时间",
		"仅显示本地快照中的主机密钥锚，修改请到权威端。",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("客户端详情页缺少只读主机密钥锚信息 %q：\n%s", want, page)
		}
	}
	for _, forbidden := range []string{"hostkey-pin", "hostkey-clear", "覆盖主机密钥锚", "删除主机密钥锚"} {
		if strings.Contains(page, forbidden) {
			t.Fatalf("客户端详情页不得显示 %q：\n%s", forbidden, page)
		}
	}
	for _, target := range []string{"/ui/servers/srv-web/hostkey-pin", "/ui/servers/srv-web/hostkey-clear"} {
		response := hostKeyPost(t, handler, cookies[0], target, url.Values{
			"csrf_token": {authorizationCSRFValue(page)}, "fingerprint": {"SHA256:not-used"}, "confirm": {"1"},
		})
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("客户端写路由 %s 不应存在，状态码=%d：\n%s",
				target, response.StatusCode, authorizationBody(t, response))
		}
	}
}
