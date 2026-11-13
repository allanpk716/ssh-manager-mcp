package webuishares

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/access"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

// TestSharesListOffersAllSharesResidueWithoutAnyShare 覆盖共享登记全部删
// 除后的兜底入口：列表为空仍能从列表页发起全部共享残留检查并查看结果。
func TestSharesListOffersAllSharesResidueWithoutAnyShare(t *testing.T) {
	st := newIntegrationStore(t)
	mustImmediatelyClosedServer(t, st, "linux-offline")
	h := startIntegrationHandler(t, st, func(config *webui.Config) {
		config.ShareResidueRunner = ResidueTasksFromStore(openStoreForTest(st))
		config.ShareUnmountRunner = UnmountTasksFromStore(openStoreForTest(st))
	})

	list := body(t, h.do(http.MethodGet, "/ui/shares/", nil))
	for _, want := range []string{
		`action="/ui/shares/residue"`, "检查全部共享残留",
	} {
		if !strings.Contains(list, want) {
			t.Fatalf("共享存储列表为空时仍应提供全部共享残留检查入口 %q：\n%s", want, list)
		}
	}

	started := h.do(http.MethodPost, "/ui/shares/residue", url.Values{
		"csrf_token": {csrfToken(t, h)},
	})
	if started.StatusCode != http.StatusSeeOther ||
		!strings.HasPrefix(started.Header.Get("Location"), "/ui/shares/residue?task=") {
		t.Fatalf("列表页应能发起全部共享残留检查，状态码=%d 跳转=%q：%s",
			started.StatusCode, started.Header.Get("Location"), body(t, started))
	}
	page := waitUntilResidueDone(t, h, started.Header.Get("Location"))
	for _, want := range []string{
		"残留检查", "全部共享", "linux-offline", "返回共享存储列表",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("全部共享残留检查页缺少 %q：\n%s", want, page)
		}
	}
}

// mustImmediatelyClosedServer 登记一台回环地址上已经关闭的端口。相比
// TEST-NET 地址，连接会立即被操作系统拒绝，测试不依赖外部网络丢包，也
// 不需要等默认 SSH 连接超时。
func mustImmediatelyClosedServer(t *testing.T, st *store.Store, name string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, portText, splitErr := net.SplitHostPort(listener.Addr().String())
	if closeErr := listener.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if splitErr != nil {
		t.Fatal(splitErr)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	credentialID, err := st.SetCredential(&models.Credential{
		Type:   models.CredPassword,
		Secret: []byte("server-password"),
	})
	if err != nil {
		t.Fatal(err)
	}
	added, err := st.AddServer(&models.Server{
		Name: name, Host: host, Port: port, User: "owner",
		AuthMethod: models.AuthPassword, CredentialID: credentialID,
		ShellType: models.ShellPOSIX,
	})
	if err != nil {
		t.Fatal(err)
	}
	return added
}

// TestSharesListAllSharesResidueCleanupRunsFromListEntry 覆盖列表入口发起
// 的全部共享检查后，逐条残留可以直接清理，不需要回到某个共享详情页。
func TestSharesListAllSharesResidueCleanupRunsFromListEntry(t *testing.T) {
	fixture := newResidueWebFixture(t)

	started := fixture.handler.do(http.MethodPost, "/ui/shares/residue", url.Values{
		"csrf_token": {csrfToken(t, fixture.handler)},
	})
	if started.StatusCode != http.StatusSeeOther {
		t.Fatalf("列表页发起全部共享残留检查状态码 = %d：%s",
			started.StatusCode, body(t, started))
	}
	page := waitUntilResidueDone(t, fixture.handler, started.Header.Get("Location"))
	for _, want := range []string{"linux-out", "datasets", `action="/ui/shares/residue-cleanup"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("全部共享残留检查结果缺少 %q：\n%s", want, page)
		}
	}

	cleanup := fixture.handler.do(http.MethodPost, "/ui/shares/residue-cleanup", url.Values{
		"csrf_token": {csrfToken(t, fixture.handler)},
		"share_name": {"datasets"}, "server_id": {fixture.servers["linux-out"]},
		"server_name": {"linux-out"},
	})
	if cleanup.StatusCode != http.StatusSeeOther ||
		!strings.HasPrefix(cleanup.Header.Get("Location"), "/ui/shares/residue?cleanup=") {
		t.Fatalf("列表入口的残留应能直接清理，状态码=%d 跳转=%q：%s",
			cleanup.StatusCode, cleanup.Header.Get("Location"), body(t, cleanup))
	}
	cleanupPage := waitUntilResidueDone(t, fixture.handler, cleanup.Header.Get("Location"))
	if !strings.Contains(cleanupPage, "清理结果") || !strings.Contains(cleanupPage, "卸载完成") {
		t.Fatalf("列表入口的残留清理结果页：\n%s", cleanupPage)
	}
	if fixture.out.State().Mounted || len(fixture.out.State().FstabLines) != 0 {
		t.Fatalf("清理后远端状态应被清空：%+v", fixture.out.State())
	}
}

// TestClientSharesListHidesAllSharesResidueEntry 覆盖客户端不可见。
func TestClientSharesListHidesAllSharesResidueEntry(t *testing.T) {
	st := newIntegrationStore(t)
	seedShare(t, st, "datasets", "NAS", "NAS_MOUNT_PASSWORD", "Z")
	manager := access.New(st)
	profileID, err := manager.AddProfile("team-a", access.CLISource())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := st.ExportSnapshotForProfile(profileID, true, "")
	if err != nil {
		t.Fatal(err)
	}
	handler, startup, err := webui.New(webui.Config{
		Role:    webui.RoleClient,
		Servers: clientServerSource{},
		Shares:  FromClientSnapshot(func(string) (*store.Snapshot, error) { return snapshot, nil }),
		Port:    18088,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, startup.URL("127.0.0.1:18088"), nil)
	req.Host = "127.0.0.1:18088"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("应签发客户端会话 cookie，实际 %d 个", len(cookies))
	}
	h := sessionHandler{handler: handler, cookie: *cookies[0], host: "127.0.0.1:18088"}

	list := body(t, h.do(http.MethodGet, "/ui/shares/", nil))
	if strings.Contains(list, "检查全部共享残留") {
		t.Fatalf("客户端共享存储列表不得显示全部共享残留入口：\n%s", list)
	}
	resp := h.do(http.MethodPost, "/ui/shares/residue", url.Values{"csrf_token": {csrfToken(t, h)}})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("客户端全部共享残留路由不应存在，状态码=%d：\n%s", resp.StatusCode, body(t, resp))
	}
}
