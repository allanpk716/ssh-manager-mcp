package webuiservers

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/access"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/servermanage"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
	"ssh-manager-mcp/internal/webuiaccess"
)

// newIntegrationHandler 用真实 vault 构造完整的单机 Web UI：读取来源、写来
// 源与访问管理来源都挂在同一个存储上，测试走完整 HTTP 请求。返回的请求方
// 法自动携带启动令牌换来的会话。
func newIntegrationHandler(t *testing.T) (*servermanage.Manager, *store.Store, sessionHandler) {
	t.Helper()
	manager, st := newAdapterManager(t)
	handler, startup, err := webui.New(webui.Config{
		Role:         webui.RoleStandalone,
		Servers:      FromManager(manager),
		ServerWriter: WriterFromManager(manager),
		Access:       webuiaccess.FromManager(access.New(st)),
		Port:         18081,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, startup.URL("127.0.0.1:18081"), nil)
	req.Host = "127.0.0.1:18081"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("应签发会话 cookie，实际 %d", len(cookies))
	}
	return manager, st, sessionHandler{handler: handler, cookie: *cookies[0]}
}

// sessionHandler 为每个请求自动附上会话 cookie。
type sessionHandler struct {
	handler http.Handler
	cookie  http.Cookie
}

func (s sessionHandler) do(method, target string, form url.Values) *http.Response {
	body := strings.NewReader("")
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, target, body)
	req.Host = "127.0.0.1:18081"
	req.AddCookie(&s.cookie)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "http://127.0.0.1:18081")
	}
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	return rec.Result()
}

func integrationBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	data := make([]byte, 0)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		data = append(data, buf[:n]...)
		if err != nil {
			break
		}
	}
	return string(data)
}

func integrationCSRF(t *testing.T, h sessionHandler) string {
	t.Helper()
	resp := h.do(http.MethodGet, "/ui/", nil)
	body := integrationBody(t, resp)
	match := regexp.MustCompile(`<meta name="csrf-token" content="([^"]+)"`).FindStringSubmatch(body)
	if len(match) != 2 {
		t.Fatalf("页面应携带防跨站请求伪造令牌：\n%s", body)
	}
	return match[1]
}

func writeIntegrationKey(t *testing.T, path string) []byte {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return keyPEM
}

// TestHandlerEditDetectsConcurrentChange 用真实 vault 证明编辑页的乐观锁有
// 效：表单打开后别处修改条目，旧表单提交被拒绝并显示最新值。
func TestHandlerEditDetectsConcurrentChange(t *testing.T) {
	manager, st, h := newIntegrationHandler(t)
	id, err := st.AddServerWithCredentials(&models.Server{
		Name: "web", Host: "192.0.2.10", Port: 22, User: "root", Role: "旧角色",
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	page := h.do(http.MethodGet, "/ui/servers/"+id+"/edit", nil)
	pageBody := integrationBody(t, page)
	revision := regexp.MustCompile(`name="revision" value="(\d+)"`).FindStringSubmatch(pageBody)
	if len(revision) != 2 {
		t.Fatalf("编辑页应携带修订号：\n%s", pageBody)
	}

	// 表单打开后，别的入口先修改这台服务器。
	newRole := "别的入口改的角色"
	if _, _, err := WriterFromManager(manager).EditServer(id, webui.ServerEditForm{
		Role: &newRole,
	}, webui.ServerWriteSource{RemoteAddress: "192.0.2.9"}); err != nil {
		t.Fatal(err)
	}

	form := url.Values{
		"csrf_token": {integrationCSRF(t, h)},
		"server_id":  {id}, "name": {"web"}, "host": {"192.0.2.10"}, "port": {"22"},
		"user": {"root"}, "shell": {"posix"}, "revision": {revision[1]},
		"role": {"页面上的角色"},
	}
	resp := h.do(http.MethodPost, "/ui/servers/"+id+"/edit", form)
	body := integrationBody(t, resp)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("过期表单提交应被拒绝，状态码=%d：\n%s", resp.StatusCode, body)
	}
	for _, want := range []string{"已被其他入口修改", "别的入口改的角色"} {
		if !strings.Contains(body, want) {
			t.Fatalf("冲突页面缺少 %q：\n%s", want, body)
		}
	}
	srv, _ := st.GetServer(id)
	if srv.Role != "别的入口改的角色" {
		t.Fatalf("过期提交不得写入：%+v", srv)
	}
}

// TestHandlerImportGrantsProfileByID 用真实 vault 覆盖 Web UI 导入：候选页提
// 交的是 profile 标识符，执行后这批服务器真的授予该 profile。
func TestHandlerImportGrantsProfileByID(t *testing.T) {
	_, st, h := newIntegrationHandler(t)
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	writeIntegrationKey(t, keyPath)
	config := filepath.Join(dir, "config")
	body := fmt.Sprintf("Host web\n  HostName 192.0.2.20\n  User deploy\n  IdentityFile %q\n", filepath.ToSlash(keyPath))
	if err := os.WriteFile(config, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	profileID, err := st.AddProfile("team")
	if err != nil {
		t.Fatal(err)
	}

	planForm := url.Values{
		"csrf_token": {integrationCSRF(t, h)}, "path": {config}, "profile": {profileID},
	}
	plan := h.do(http.MethodPost, "/ui/servers/import/plan", planForm)
	planBody := integrationBody(t, plan)
	if plan.StatusCode != http.StatusOK || !strings.Contains(planBody, "web") {
		t.Fatalf("候选页应显示候选 web，状态码=%d：\n%s", plan.StatusCode, planBody)
	}
	if !strings.Contains(planBody, `value="`+profileID+`" selected`) {
		t.Fatalf("候选页应按标识符选中 profile：%s", planBody)
	}

	runForm := url.Values{
		"csrf_token": {integrationCSRF(t, h)}, "path": {config},
		"candidate": {"web"}, "profile": {profileID},
	}
	run := h.do(http.MethodPost, "/ui/servers/import/run", runForm)
	runBody := integrationBody(t, run)
	if run.StatusCode != http.StatusOK || !strings.Contains(runBody, "已导入") {
		t.Fatalf("导入执行应成功，状态码=%d：\n%s", run.StatusCode, runBody)
	}
	granted, err := st.ServersForProfile(profileID)
	if err != nil {
		t.Fatal(err)
	}
	if len(granted) != 1 {
		t.Fatalf("导入应按标识符授权 profile，实际 %d 台：%v", len(granted), granted)
	}
	grantedServer, err := st.GetServer(granted[0])
	if err != nil || grantedServer == nil || grantedServer.Name != "web" {
		t.Fatalf("导入应把 web 授予 profile：%v %v", grantedServer, err)
	}
}

// TestHandlerAddServerSavesDeviceMembershipAndAuthorization 用真实 vault 覆
// 盖新增表单：粘贴私钥、新建设备、多个网络成员与 profile 授权一次保存，结
// 果页停留在待连接测试状态。
func TestHandlerAddServerSavesDeviceMembershipAndAuthorization(t *testing.T) {
	_, st, h := newIntegrationHandler(t)
	if _, err := st.AddSite("家A", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "家A局域网", Type: models.NetworkPhysical, SiteName: "家A"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "虚拟网", Type: models.NetworkVirtual}); err != nil {
		t.Fatal(err)
	}
	profileID, err := st.AddProfile("运维")
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "private-key")
	keyPEM := writeIntegrationKey(t, keyPath)

	typePage := h.do(http.MethodGet, "/ui/add", nil)
	typeBody := integrationBody(t, typePage)
	if typePage.StatusCode != http.StatusOK || !strings.Contains(typeBody, ">服务器</strong>") {
		t.Fatalf("类型选择页应先提供服务器类型，状态码=%d：\n%s", typePage.StatusCode, typeBody)
	}

	formPage := h.do(http.MethodGet, "/ui/servers/new", nil)
	formBody := integrationBody(t, formPage)
	if formPage.StatusCode != http.StatusOK || !strings.Contains(formBody, "保存后自动探测") ||
		!strings.Contains(formBody, "粘贴私钥") || !strings.Contains(formBody, "profile 授权") ||
		!strings.Contains(formBody, "保存并进入待连接测试") {
		t.Fatalf("新增表单缺少关键字段，状态码=%d：\n%s", formPage.StatusCode, formBody)
	}
	csrf := regexp.MustCompile(`<input type="hidden" name="csrf_token" value="([^"]+)"`).FindStringSubmatch(formBody)
	if len(csrf) != 2 {
		t.Fatalf("新增表单应携带防跨站请求伪造令牌：\n%s", formBody)
	}

	resp := h.do(http.MethodPost, "/ui/servers/new", url.Values{
		"csrf_token": {csrf[1]},
		"name":       {"web"}, "user": {"deploy"}, "port": {"2202"},
		"host": {"192.168.1.10"}, "shell": {"auto"},
		"credential_action": {"key"}, "key_source": {"paste"}, "key": {string(keyPEM)},
		"sudo_password": {"sudo-secret"}, "description": {"网站服务器"},
		"location": {"机柜 A"}, "hardware": {"2 核 4GB"}, "services": {"nginx"},
		"role": {"网站"}, "special_handling": {"维护窗口每周三"}, "tags": {"prod,web"},
		"expose_host": {"1"}, "device_choice": {"new"}, "device_name": {"工作站"},
		"physical_network": {"家A局域网"}, "physical_address": {"192.168.1.10"},
		"virtual_network": {"虚拟网"}, "virtual_address": {"100.64.0.10"},
		"profile_id": {profileID},
	})
	integrationBody(t, resp)
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/ui/servers/new/result?") ||
		!strings.Contains(resp.Header.Get("Location"), "server=") {
		t.Fatalf("新增成功应进入结果页，状态码=%d 跳转=%s", resp.StatusCode, resp.Header.Get("Location"))
	}
	result := h.do(http.MethodGet, resp.Header.Get("Location"), nil)
	resultBody := integrationBody(t, result)
	if result.StatusCode != http.StatusOK || !strings.Contains(resultBody, "待连接测试") ||
		!strings.Contains(resultBody, "工作站") || strings.Contains(resultBody, string(keyPEM)) ||
		strings.Contains(resultBody, "sudo-secret") {
		t.Fatalf("结果页应待连接测试且不回显凭据，状态码=%d：\n%s", result.StatusCode, resultBody)
	}

	srv, err := st.GetServerByName("web")
	if err != nil || srv == nil {
		t.Fatalf("新增服务器应落库：%v %+v", err, srv)
	}
	if srv.AuthMethod != models.AuthPrivateKey || srv.Host != "192.168.1.10" || !srv.ExposeHost ||
		srv.ShellType != models.ShellPOSIX || srv.Role != "网站" {
		t.Fatalf("服务器字段保存不正确：%+v", srv)
	}
	device, err := st.GetDevice("工作站")
	if err != nil || device == nil {
		t.Fatalf("新设备应落库：%v %+v", err, device)
	}
	memberships, err := st.ListNetworkMemberships("工作站")
	if err != nil || len(memberships) != 2 {
		t.Fatalf("新设备应有物理与虚拟网络成员：%v %+v", err, memberships)
	}
	granted, err := st.ServersForProfile(profileID)
	if err != nil || len(granted) != 1 || granted[0] != srv.ID {
		t.Fatalf("新增服务器应授权 profile：%v %v", granted, err)
	}
}

// TestHandlerAddServerShowsConflictAndKeepsFields 覆盖处理器层的字段校验
// 呈现：同一网络地址被占用时不落库，页面说明占用设备与网络，并保留非凭据
// 字段、不回显凭据。
func TestHandlerAddServerShowsConflictAndKeepsFields(t *testing.T) {
	_, st, h := newIntegrationHandler(t)
	if _, err := st.AddSite("家A", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "家A局域网", Type: models.NetworkPhysical, SiteName: "家A"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("占用设备", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership("占用设备", "家A局域网", "192.168.1.30"); err != nil {
		t.Fatal(err)
	}

	resp := h.do(http.MethodPost, "/ui/servers/new", url.Values{
		"csrf_token": {integrationCSRF(t, h)},
		"name":       {"conflict"}, "user": {"deploy"}, "port": {"22"}, "shell": {"auto"},
		"credential_action": {"password"}, "password": {"login-secret"},
		"description": {"页面保留说明"}, "role": {"网站"},
		"physical_network": {"家A局域网"}, "physical_address": {"192.168.1.30"},
	})
	body := integrationBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "已被设备 占用设备 占用") ||
		!strings.Contains(body, "家A局域网") {
		t.Fatalf("地址冲突应显示占用设备与网络，状态码=%d：\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `value="conflict"`) || !strings.Contains(body, `value="页面保留说明"`) ||
		strings.Contains(body, "login-secret") {
		t.Fatalf("校验失败页面应保留非凭据字段且不回显凭据：\n%s", body)
	}
	if servers, _ := st.ListServers(); len(servers) != 0 {
		t.Fatalf("校验失败不得写入服务器条目：%v", servers)
	}
}

// TestHandlerCompleteClearsMetadataFields 用真实 vault 覆盖补全表单：表单预
// 填当前值，空提交应清空服务器元数据字段。
func TestHandlerCompleteClearsMetadataFields(t *testing.T) {
	_, st, h := newIntegrationHandler(t)
	id, err := st.AddServerWithCredentials(&models.Server{
		Name: "web", Host: "192.0.2.10", Port: 22, User: "root",
		Role: "旧角色", Location: "旧机房",
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	form := url.Values{
		"csrf_token": {integrationCSRF(t, h)},
		"queue":      {id}, "position": {"0"}, "action": {"save"},
		"description": {""}, "location": {""}, "hardware": {""},
		"services": {""}, "role": {""}, "special_handling": {""},
		"sudo_password": {""}, "password": {""}, "key_passphrase": {""},
	}
	resp := h.do(http.MethodPost, "/ui/servers/complete", form)
	integrationBody(t, resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("补全提交状态码 = %d，应为 303", resp.StatusCode)
	}
	srv, _ := st.GetServer(id)
	if srv.Role != "" || srv.Location != "" {
		t.Fatalf("空提交应清空服务器元数据字段：%+v", srv)
	}
}

// TestHandlerDeletePreviewChangedRendersFreshPreview 用真实 vault 覆盖删除
// 的乐观确认：预览变化后提交被拒绝，页面显示执行时重新组装的新预览与新摘
// 要，可以再次确认。
func TestHandlerDeletePreviewChangedRendersFreshPreview(t *testing.T) {
	_, st, h := newIntegrationHandler(t)
	cred := &models.Credential{Type: models.CredPassword, Secret: []byte("login-secret")}
	first, err := st.AddServerWithCredentials(&models.Server{
		Name: "first", Host: "192.0.2.10", Port: 22, User: "root", Role: "网站",
	}, cred, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddServerWithCredentials(&models.Server{
		Name: "second", Host: "192.0.2.11", Port: 22, User: "root", Role: "数据库",
	}, cred, nil); err != nil {
		t.Fatal(err)
	}

	page := h.do(http.MethodGet, "/ui/servers/"+first+"/delete", nil)
	pageBody := integrationBody(t, page)
	digest := regexp.MustCompile(`name="digest" value="([^"]+)"`).FindStringSubmatch(pageBody)
	if len(digest) != 2 {
		t.Fatalf("删除预览页应携带摘要：\n%s", pageBody)
	}

	// 预览打开后，别的入口修改另一台服务器，使预览内容变化。
	second, err := st.GetServerByName("second")
	if err != nil || second == nil {
		t.Fatal(err)
	}
	if err := st.ClearServerCredential(second.ID); err != nil {
		t.Fatal(err)
	}

	resp := h.do(http.MethodPost, "/ui/servers/"+first+"/delete", url.Values{
		"csrf_token": {integrationCSRF(t, h)}, "server_id": {first}, "digest": {digest[1]},
	})
	body := integrationBody(t, resp)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("预览变化应拒绝删除，状态码=%d：\n%s", resp.StatusCode, body)
	}
	for _, want := range []string{"删除预览已变化", "first"} {
		if !strings.Contains(body, want) {
			t.Fatalf("新预览页缺少 %q：\n%s", want, body)
		}
	}
	if srv, _ := st.GetServerByName("first"); srv == nil {
		t.Fatal("预览变化时不得删除服务器")
	}
	newDigest := regexp.MustCompile(`name="digest" value="([^"]+)"`).FindStringSubmatch(body)
	if len(newDigest) != 2 || newDigest[1] == digest[1] {
		t.Fatalf("新预览页应携带新摘要：%q", newDigest)
	}

	// 用新摘要再次确认，删除真正执行。
	confirmed := h.do(http.MethodPost, "/ui/servers/"+first+"/delete", url.Values{
		"csrf_token": {integrationCSRF(t, h)}, "server_id": {first}, "digest": {newDigest[1]},
	})
	if confirmed.StatusCode != http.StatusSeeOther || confirmed.Header.Get("Location") != "/ui/" {
		t.Fatalf("新摘要确认应执行删除，状态码=%d 跳转=%s", confirmed.StatusCode, confirmed.Header.Get("Location"))
	}
	if srv, _ := st.GetServerByName("first"); srv != nil {
		t.Fatal("确认后服务器应已删除")
	}
}

// TestHandlerEditValidationErrorKeepsFields 用真实 vault 覆盖编辑校验失败：
// 页面显示指向字段的中文提示，保留非凭据字段，不出现命令行参数名。
func TestHandlerEditValidationErrorKeepsFields(t *testing.T) {
	_, st, h := newIntegrationHandler(t)
	id, err := st.AddServerWithCredentials(&models.Server{
		Name: "web", Host: "192.0.2.10", Port: 22, User: "root", Role: "旧角色",
	}, &models.Credential{Type: models.CredPassword, Secret: []byte("login-secret")}, nil)
	if err != nil {
		t.Fatal(err)
	}

	resp := h.do(http.MethodPost, "/ui/servers/"+id+"/edit", url.Values{
		"csrf_token": {integrationCSRF(t, h)},
		"server_id":  {id}, "name": {"web"}, "host": {"192.0.2.10"}, "port": {"22"},
		"user": {"root"}, "shell": {"posix"}, "revision": {"0"},
		"role": {"页面上的角色"}, "credential_action": {"password"}, "password": {" "},
	})
	body := integrationBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("校验失败状态码 = %d，应为 400：\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "密码不能为空") || strings.Contains(body, "--password") {
		t.Fatalf("校验提示应指向字段且不含命令行参数名：\n%s", body)
	}
	if !strings.Contains(body, `value="页面上的角色"`) {
		t.Fatalf("校验页面应保留提交的字段值：\n%s", body)
	}
	if srv, _ := st.GetServer(id); srv.Role != "旧角色" {
		t.Fatalf("校验失败不得写入：%+v", srv)
	}
}

// TestHandlerCompleteValidationErrorUsesPageText 用真实 vault 覆盖补全校验
// 失败：提示是面向页面的中文，不含命令行参数名。
func TestHandlerCompleteValidationErrorUsesPageText(t *testing.T) {
	_, st, h := newIntegrationHandler(t)
	id, err := st.AddServerWithCredentials(&models.Server{
		Name: "web", Host: "192.0.2.10", Port: 22, User: "root", Role: "网站",
	}, &models.Credential{Type: models.CredPassword, Secret: []byte("login-secret")}, nil)
	if err != nil {
		t.Fatal(err)
	}

	resp := h.do(http.MethodPost, "/ui/servers/complete", url.Values{
		"csrf_token": {integrationCSRF(t, h)},
		"queue":      {id}, "position": {"0"}, "action": {"save"},
		"description": {"页面说明"}, "location": {}, "hardware": {},
		"services": {}, "role": {"网站"}, "special_handling": {},
		"sudo_password": {}, "password": {"new-login"}, "key_passphrase": {},
	})
	body := integrationBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("补全校验失败状态码 = %d，应为 400：\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "已有登录凭据") || strings.Contains(body, "--") {
		t.Fatalf("补全提示应面向页面：\n%s", body)
	}
	if !strings.Contains(body, `value="页面说明"`) {
		t.Fatalf("补全校验页面应保留提交的字段值：\n%s", body)
	}
}

// TestHandlerEditCombinesPassphraseWithFields 用真实 vault 覆盖编辑页组合提
// 交：「只补私钥口令」与其他字段一起提交时都生效。
func TestHandlerEditCombinesPassphraseWithFields(t *testing.T) {
	_, st, h := newIntegrationHandler(t)
	keyPEM := genIntegrationEncryptedKey(t, "secret-pass")
	cred := &models.Credential{Type: models.CredPrivateKey, Secret: keyPEM}
	id, err := st.AddServerWithCredentials(&models.Server{
		Name: "enc", Host: "192.0.2.30", Port: 22, User: "root", Role: "旧角色",
		Tags: []string{models.TagNeedsPassphrase},
	}, cred, nil)
	if err != nil {
		t.Fatal(err)
	}

	resp := h.do(http.MethodPost, "/ui/servers/"+id+"/edit", url.Values{
		"csrf_token": {integrationCSRF(t, h)},
		"server_id":  {id}, "name": {"enc"}, "host": {"192.0.2.30"}, "port": {"22"},
		"user": {"root"}, "shell": {"posix"}, "revision": {"0"},
		"role": {"新角色"}, "credential_action": {"key-passphrase"}, "key_passphrase": {"secret-pass"},
	})
	integrationBody(t, resp)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/ui/servers/"+id {
		t.Fatalf("组合提交应成功，状态码=%d 跳转=%s", resp.StatusCode, resp.Header.Get("Location"))
	}
	srv, _ := st.GetServer(id)
	if srv == nil || srv.Role != "新角色" || models.ServerHasTag(srv.Tags, models.TagNeedsPassphrase) {
		t.Fatalf("组合提交应同时保存角色与私钥口令：%+v", srv)
	}
	savedCred, _ := st.GetCredential(srv.CredentialID)
	if savedCred == nil || string(savedCred.Passphrase) != "secret-pass" {
		t.Fatalf("口令应写进凭据：%+v", savedCred)
	}
}

// genIntegrationEncryptedKey 生成一把带口令的加密私钥，供 Web UI 集成测试
// 使用。
func genIntegrationEncryptedKey(t *testing.T, passphrase string) []byte {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	block, err := x509.EncryptPEMBlock(rand.Reader, "PRIVATE KEY", der, []byte(passphrase), x509.PEMCipherAES256)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(block)
}
