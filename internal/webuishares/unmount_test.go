package webuishares

import (
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sharemounttest"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
	"ssh-manager-mcp/internal/webui"
)

// newUnmountFixture 组装单台卸载页的集成测试环境：真实临时加密 vault、
// 一台带登录与 sudo 凭据的 Linux 假服务器、一台 Windows 登记服务器，以及
// 挂上卸载任务视图的完整 Web UI。测试不连接真实服务器。
func newUnmountFixture(t *testing.T) *unmountFixture {
	t.Helper()
	st := newIntegrationStore(t)
	linux := &sharemounttest.LinuxServer{ShareName: "datasets"}
	linuxHandler := linux.Handler
	linuxExec := func(command string, stdin io.Reader) (string, string, int) {
		if strings.Contains(command, "SSHMGR-LINUX-CHECK") {
			return "SSHMGR-LINUX-CHECK mode=full\n" +
				"SSHMGR-LINUX-CHECK index=1 managed=yes share=datasets version=1 mounted=yes\n", "", 0
		}
		return linuxHandler(command, stdin)
	}
	addr, hostKey, stopServer := testsshd.Start(t, testsshd.Options{
		Password: "login-pw", SudoPassword: "sudo-pw", Exec: linuxExec,
	})
	host, portText, _ := strings.Cut(addr, ":")
	port, _ := strconv.Atoi(portText)
	if _, err := st.AddDevice("NAS", "共享存储提供方", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("挂载目标", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("windows 服务器", "", ""); err != nil {
		t.Fatal(err)
	}
	loginCredential := &models.Credential{Type: models.CredPassword, Secret: []byte("login-pw")}
	sudoCredential := &models.Credential{Type: models.CredPassword, Secret: []byte("sudo-pw")}
	server := &models.Server{
		Name: "linux-web", Host: host, Port: port, User: "deploy",
		ShellType: models.ShellPOSIX,
	}
	added, err := st.AddServerWithCredentialsToDevice(server, loginCredential, sudoCredential, "挂载目标")
	if err != nil {
		t.Fatal(err)
	}
	linuxServerID := added.ServerID
	server.ID = linuxServerID
	if err := st.SaveEntryPin(server, hostKey.Marshal()); err != nil {
		t.Fatal(err)
	}
	windowsID := mustServerToDevice(t, st, "windows-web", models.ShellPowerShell, "windows 服务器")
	mustSecret(t, st, "DATASETS_MOUNT_PASSWORD")
	if err := st.AllowSecrets(
		[]string{"DATASETS_MOUNT_PASSWORD"}, []string{"linux-web", "windows-web"}, store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	share, err := st.AddSharedStorage(store.SharedStorageInput{
		Name: "datasets", Device: "NAS", User: "mount-user",
		PasswordSecret: "DATASETS_MOUNT_PASSWORD", Drive: "Z",
	}, store.CLISource())
	if err != nil {
		t.Fatal(err)
	}
	linux.ShareID = share.ID
	linux.SeedManagedConfiguration("192.0.2.10", share.Version)
	h := startIntegrationHandler(t, st, func(config *webui.Config) {
		remoteTasks := RemoteTasksFromStore(openStoreForTest(st))
		config.SharesRemoteTasks = remoteTasks
		config.ShareLatestChecks = remoteTasks
		config.ShareUnmountRunner = UnmountTasksFromStore(openStoreForTest(st))
		config.ShareRevokeRunner = RevokeTasksFromStore(openStoreForTest(st))
	})
	return &unmountFixture{
		store: st, linux: linux, handler: h, stopServer: stopServer,
		shareID: share.ID, linuxID: linuxServerID, windowsID: windowsID,
	}
}

type unmountFixture struct {
	store      *store.Store
	linux      *sharemounttest.LinuxServer
	handler    sessionHandler
	stopServer func()
	shareID    string
	linuxID    string
	windowsID  string
}

func (f *unmountFixture) confirmPath(serverID string) string {
	return "/ui/shares/" + f.shareID + "/unmount?server=" + serverID
}

func TestShareDetailOffersUnmountForLinuxAndExplainsWindows(t *testing.T) {
	fixture := newUnmountFixture(t)

	resp := fixture.handler.do(http.MethodGet, "/ui/shares/"+fixture.shareID, nil)
	page := body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("共享存储详情状态码 = %d：\n%s", resp.StatusCode, page)
	}
	for _, want := range []string{
		"单台卸载", `href="` + fixture.confirmPath(fixture.linuxID) + `"`, "卸载",
		"Windows 服务器不提供卸载", "sshmgr 不在其上留下任何持久改动",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("详情页的单台卸载区域缺少 %q：\n%s", want, page)
		}
	}
	if strings.Contains(page, `href="`+fixture.confirmPath(fixture.windowsID)+`"`) {
		t.Fatalf("Windows 服务器不得提供卸载入口：\n%s", page)
	}
	if strings.Contains(page, "mount-password") || strings.Contains(page, "login-pw") {
		t.Fatalf("详情页不得显示密码：%s", page)
	}
}

func TestShareUnmountConfirmationPageExplainsRemoteCleanup(t *testing.T) {
	fixture := newUnmountFixture(t)

	resp := fixture.handler.do(http.MethodGet, fixture.confirmPath(fixture.linuxID), nil)
	page := body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("卸载确认页状态码 = %d：\n%s", resp.StatusCode, page)
	}
	for _, want := range []string{
		"卸载 datasets · linux-web",
		"将删除服务器上的远端持久挂载配置，再卸载挂载点",
		"被占用时会列出占用进程，由你决定强制卸载或稍后再试",
		`name="server_id" value="` + fixture.linuxID + `"`,
		"确认卸载",
		"取消",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("卸载确认页缺少 %q：\n%s", want, page)
		}
	}

	windows := fixture.handler.do(http.MethodGet, fixture.confirmPath(fixture.windowsID), nil)
	windowsPage := body(t, windows)
	if windows.StatusCode != http.StatusOK {
		t.Fatalf("Windows 卸载说明页状态码 = %d：\n%s", windows.StatusCode, windowsPage)
	}
	for _, want := range []string{
		"Windows 服务器不能执行共享存储卸载",
		"sshmgr 不在其上留下任何持久改动",
	} {
		if !strings.Contains(windowsPage, want) {
			t.Fatalf("Windows 卸载说明页缺少 %q：\n%s", want, windowsPage)
		}
	}
	if strings.Contains(windowsPage, "确认卸载") {
		t.Fatalf("Windows 服务器不得提供卸载确认按钮：\n%s", windowsPage)
	}

	unknown := fixture.handler.do(http.MethodGet, fixture.confirmPath("missing-server"), nil)
	if unknown.StatusCode != http.StatusNotFound {
		t.Fatalf("不在可挂载范围内的服务器应返回不存在，状态码 = %d：\n%s",
			unknown.StatusCode, body(t, unknown))
	}
}

// waitUntilUnmountDone 轮询单台卸载任务页直到任务完成，返回最终页面。
func waitUntilUnmountDone(t *testing.T, h sessionHandler, taskPath string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp := h.do(http.MethodGet, taskPath, nil)
		page := body(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("卸载任务页状态码 = %d：\n%s", resp.StatusCode, page)
		}
		if !strings.Contains(page, "卸载进行中") {
			return page
		}
		if time.Now().After(deadline) {
			t.Fatalf("卸载任务超时未完成：\n%s", page)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestShareUnmountTaskSucceedsAndAuditsWebUISource(t *testing.T) {
	fixture := newUnmountFixture(t)

	post := fixture.handler.do(http.MethodPost, "/ui/shares/"+fixture.shareID+"/unmount", url.Values{
		"csrf_token": {csrfToken(t, fixture.handler)},
		"server_id":  {fixture.linuxID},
	})
	if post.StatusCode != http.StatusSeeOther {
		t.Fatalf("启动卸载任务状态码 = %d：\n%s", post.StatusCode, body(t, post))
	}
	taskPath := post.Header.Get("Location")
	if !strings.Contains(taskPath, "task=") {
		t.Fatalf("启动卸载任务应跳转到任务页：%s", taskPath)
	}
	page := waitUntilUnmountDone(t, fixture.handler, taskPath)
	for _, want := range []string{
		"卸载完成", "服务器仍在可挂载范围内，agent 之后仍可能重新挂载",
		"彻底撤下需先取消勾选",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("卸载成功页缺少 %q：\n%s", want, page)
		}
	}
	for _, forbidden := range []string{"mount-password", "login-pw", "sudo-pw", "192.0.2.10"} {
		if strings.Contains(page, forbidden) {
			t.Fatalf("卸载结果页泄露 %q：\n%s", forbidden, page)
		}
	}
	state := fixture.linux.State()
	if state.Mounted || len(state.FstabLines) != 0 || state.Credential != "" {
		t.Fatalf("卸载后的服务器状态 = %+v", state)
	}
	rows, err := fixture.store.QueryAudit(store.AuditFilter{
		OwnerOnly: true, Actions: []string{"share-unmount"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("应有且仅有一条卸载审计行：%+v", rows)
	}
	row := rows[0]
	if row.Source != store.AuditSourceWebUI || row.SourceIP != "192.0.2.1" || row.Status != "ok" {
		t.Fatalf("卸载审计来源 = %q/%q，状态 = %q", row.Source, row.SourceIP, row.Status)
	}
	if !strings.Contains(row.Command, `"share":"datasets"`) ||
		!strings.Contains(row.Command, `"server":"linux-web"`) ||
		!strings.Contains(row.Command, `"result":"success"`) {
		t.Fatalf("卸载审计摘要 = %s", row.Command)
	}
}

func TestShareUnmountBusyListsProcessesAndForceSucceeds(t *testing.T) {
	fixture := newUnmountFixture(t)
	fixture.linux.BusyProcesses = []sharemounttest.BusyProcess{
		{ProcessID: "1234", User: "worker", Command: "python"},
	}
	csrf := csrfToken(t, fixture.handler)

	post := fixture.handler.do(http.MethodPost, "/ui/shares/"+fixture.shareID+"/unmount", url.Values{
		"csrf_token": {csrf}, "server_id": {fixture.linuxID},
	})
	if post.StatusCode != http.StatusSeeOther {
		t.Fatalf("启动占用场景卸载状态码 = %d：\n%s", post.StatusCode, body(t, post))
	}
	page := waitUntilUnmountDone(t, fixture.handler, post.Header.Get("Location"))
	for _, want := range []string{
		"配置已删除，挂载点被占用并保留", "占用挂载点的进程",
		"1234", "worker", "python", "强制卸载", "稍后再试",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("占用结果页缺少 %q：\n%s", want, page)
		}
	}
	if !fixture.linux.State().Mounted {
		t.Fatalf("不强制时应保留挂载：%+v", fixture.linux.State())
	}
	if strings.Contains(page, "卸载完成") {
		t.Fatalf("占用时不得显示卸载完成：\n%s", page)
	}

	fixture.linux.ForceSucceeds = true
	force := fixture.handler.do(http.MethodPost, "/ui/shares/"+fixture.shareID+"/unmount", url.Values{
		"csrf_token": {csrf}, "server_id": {fixture.linuxID}, "force": {"1"},
	})
	if force.StatusCode != http.StatusSeeOther {
		t.Fatalf("启动强制卸载状态码 = %d：\n%s", force.StatusCode, body(t, force))
	}
	forcePage := waitUntilUnmountDone(t, fixture.handler, force.Header.Get("Location"))
	if !strings.Contains(forcePage, "卸载完成") || !strings.Contains(forcePage, "强制执行") {
		t.Fatalf("强制卸载结果页：\n%s", forcePage)
	}
	if fixture.linux.State().Mounted {
		t.Fatalf("强制卸载后挂载应被移除：%+v", fixture.linux.State())
	}
	rows, err := fixture.store.QueryAudit(store.AuditFilter{
		OwnerOnly: true, Actions: []string{"share-unmount"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("应有占用与强制两条审计行：%+v", rows)
	}
	for _, row := range rows {
		if row.Source != store.AuditSourceWebUI || row.Status != "ok" {
			t.Fatalf("占用与强制的审计来源或状态不正确：%+v", row)
		}
	}
	busySeen, forcedSeen := false, false
	for _, row := range rows {
		if strings.Contains(row.Command, `"result":"busy"`) && strings.Contains(row.Command, `"force":false`) {
			busySeen = true
		}
		if strings.Contains(row.Command, `"result":"success"`) && strings.Contains(row.Command, `"force":true`) {
			forcedSeen = true
		}
	}
	if !busySeen || !forcedSeen {
		t.Fatalf("占用与强制的审计摘要不正确：%s / %s", rows[0].Command, rows[1].Command)
	}
}

func TestShareUnmountUnavailableServerFailsImmediatelyWithAudit(t *testing.T) {
	fixture := newUnmountFixture(t)
	// 关闭假服务器后再发起，模拟目标服务器不可达；页面不得排队重试。
	fixture.stopServer()
	t.Cleanup(fixture.stopServer)
	post := fixture.handler.do(http.MethodPost, "/ui/shares/"+fixture.shareID+"/unmount", url.Values{
		"csrf_token": {csrfToken(t, fixture.handler)},
		"server_id":  {fixture.linuxID},
	})
	if post.StatusCode != http.StatusSeeOther {
		t.Fatalf("不可达场景仍应先启动任务，状态码 = %d：\n%s", post.StatusCode, body(t, post))
	}
	page := waitUntilUnmountDone(t, fixture.handler, post.Header.Get("Location"))
	for _, want := range []string{
		"连接目标服务器或执行固定卸载命令失败",
		"任务已当场失败，未排队重试", "「检查残留」",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("不可达结果页缺少 %q：\n%s", want, page)
		}
	}
	if strings.Contains(page, "占用挂载点的进程") || strings.Contains(page, "强制卸载") {
		t.Fatalf("不可达失败不得呈现占用选择：\n%s", page)
	}
	rows, err := fixture.store.QueryAudit(store.AuditFilter{
		OwnerOnly: true, Actions: []string{"share-unmount"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != "error" ||
		rows[0].Source != store.AuditSourceWebUI || rows[0].SourceIP != "192.0.2.1" {
		t.Fatalf("不可达时的卸载审计 = %+v", rows)
	}
	if !strings.Contains(rows[0].Command, `"result":"ssh-error"`) {
		t.Fatalf("不可达时的审计摘要 = %s", rows[0].Command)
	}
}

func TestShareUnmountWindowsServerIsRejectedAndAudited(t *testing.T) {
	fixture := newUnmountFixture(t)

	post := fixture.handler.do(http.MethodPost, "/ui/shares/"+fixture.shareID+"/unmount", url.Values{
		"csrf_token": {csrfToken(t, fixture.handler)},
		"server_id":  {fixture.windowsID},
	})
	if post.StatusCode != http.StatusSeeOther {
		t.Fatalf("Windows 卸载请求状态码 = %d：\n%s", post.StatusCode, body(t, post))
	}
	page := waitUntilUnmountDone(t, fixture.handler, post.Header.Get("Location"))
	for _, want := range []string{
		"Windows 服务器不能执行共享存储卸载", "sshmgr 不在其上留下任何持久改动",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("Windows 拒绝页缺少 %q：\n%s", want, page)
		}
	}
	if strings.Contains(page, "确认卸载") {
		t.Fatalf("Windows 拒绝页不得提供确认按钮：\n%s", page)
	}
	rows, err := fixture.store.QueryAudit(store.AuditFilter{
		OwnerOnly: true, Actions: []string{"share-unmount"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != "error" ||
		rows[0].Source != store.AuditSourceWebUI || rows[0].SourceIP != "192.0.2.1" {
		t.Fatalf("Windows 拒绝的卸载审计 = %+v", rows)
	}
	if !strings.Contains(rows[0].Command, `"result":"windows-unsupported"`) {
		t.Fatalf("Windows 拒绝的审计摘要 = %s", rows[0].Command)
	}
}

func TestShareUnmountExpiredTaskPromptsRestart(t *testing.T) {
	fixture := newUnmountFixture(t)

	resp := fixture.handler.do(
		http.MethodGet, fixture.confirmPath(fixture.linuxID)+"&task=missing-task", nil,
	)
	page := body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("失效任务页状态码 = %d：\n%s", resp.StatusCode, page)
	}
	for _, want := range []string{
		"卸载任务结果已失效", "重新确认并发起", "确认卸载",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("失效任务页缺少 %q：\n%s", want, page)
		}
	}
}
