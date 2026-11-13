package webuishares

import (
	"encoding/json"
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

// newReselectWebFixture 组装重选地址页的集成测试环境：真实临时加密
// vault、带物理与虚拟两套网络的拓扑、一台状态化 Linux 假服务器与一台
// Windows 登记服务器。currentAddress 是假服务器当前使用的共享地址。
func newReselectWebFixture(t *testing.T, currentAddress string) *reselectWebFixture {
	t.Helper()
	st := newIntegrationStore(t)
	linux := &sharemounttest.LinuxServer{
		ShareName:          "datasets",
		ReachableAddresses: []string{"192.0.2.10", "100.64.10.10"},
	}
	addr, hostKey, stopServer := testsshd.Start(t, testsshd.Options{
		Password: "login-pw", SudoPassword: "sudo-pw", Exec: linux.Handler,
	})
	t.Cleanup(stopServer)
	host, portText, _ := strings.Cut(addr, ":")
	port, _ := strconv.Atoi(portText)
	for _, device := range []string{"NAS", "linux-device", "windows 服务器"} {
		if _, err := st.AddDevice(device, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.AddSite("办公室", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{
		Name: "办公物理局域网", Type: models.NetworkPhysical, SiteName: "办公室",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{
		Name: "办公虚拟局域网", Type: models.NetworkVirtual,
	}); err != nil {
		t.Fatal(err)
	}
	for _, membership := range []struct {
		device, network, address string
	}{
		{"NAS", "办公物理局域网", "192.0.2.10"},
		{"NAS", "办公虚拟局域网", "100.64.10.10"},
		{"linux-device", "办公物理局域网", "192.0.2.20"},
		{"linux-device", "办公虚拟局域网", "100.64.10.20"},
	} {
		if _, err := st.AddNetworkMembership(
			membership.device, membership.network, membership.address,
		); err != nil {
			t.Fatal(err)
		}
	}
	loginCredential := &models.Credential{Type: models.CredPassword, Secret: []byte("login-pw")}
	sudoCredential := &models.Credential{Type: models.CredPassword, Secret: []byte("sudo-pw")}
	added, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: "linux-web", Host: host, Port: port, User: "deploy",
		ShellType: models.ShellPOSIX,
	}, loginCredential, sudoCredential, "linux-device")
	if err != nil {
		t.Fatal(err)
	}
	linuxID := added.ServerID
	server, err := st.GetServer(linuxID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveEntryPin(server, hostKey.Marshal()); err != nil {
		t.Fatal(err)
	}
	windowsID := mustServerToDevice(t, st, "windows-web", models.ShellPowerShell, "windows 服务器")
	shareID := seedShare(t, st, "datasets", "NAS", "DATASETS_MOUNT_PASSWORD", "Z",
		"linux-web", "windows-web")
	share, err := st.GetSharedStorage("datasets")
	if err != nil {
		t.Fatal(err)
	}
	linux.ShareID = share.ID
	linux.SeedManagedConfiguration(currentAddress, share.Version)
	h := startIntegrationHandler(t, st, func(config *webui.Config) {
		config.ShareReselectRunner = ReselectTasksFromStore(openStoreForTest(st))
	})
	return &reselectWebFixture{
		store: st, linux: linux, handler: h, shareID: shareID,
		linuxID: linuxID, windowsID: windowsID,
	}
}

type reselectWebFixture struct {
	store     *store.Store
	linux     *sharemounttest.LinuxServer
	handler   sessionHandler
	shareID   string
	linuxID   string
	windowsID string
}

func (f *reselectWebFixture) confirmPath(serverID string) string {
	return "/ui/shares/" + f.shareID + "/reselect?server=" + serverID
}

func waitUntilReselectDone(t *testing.T, h sessionHandler, taskPath string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp := h.do(http.MethodGet, taskPath, nil)
		page := body(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("重选任务页状态码 = %d：\n%s", resp.StatusCode, page)
		}
		if !strings.Contains(page, "重选进行中") {
			return page
		}
		if time.Now().After(deadline) {
			t.Fatalf("重选任务超时未完成：\n%s", page)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestShareReselectBestAddressShowsSafeResultAndAudit(t *testing.T) {
	f := newReselectWebFixture(t, "192.0.2.10")

	detail := body(t, f.handler.do(http.MethodGet, "/ui/shares/"+f.shareID, nil))
	for _, want := range []string{
		"重选地址", `href="` + f.confirmPath(f.linuxID) + `"`,
		"优先切回物理局域网地址",
		"Windows 服务器不提供重选", "每条命令执行前本来就会重新选址",
	} {
		if !strings.Contains(detail, want) {
			t.Fatalf("详情页的重选地址区域缺少 %q：\n%s", want, detail)
		}
	}
	if strings.Contains(detail, `href="`+f.confirmPath(f.windowsID)+`"`) ||
		strings.Contains(detail, "DATASETS_MOUNT_PASSWORD-value-不回显") {
		t.Fatalf("详情页不得给 Windows 重选入口或显示机密项值：\n%s", detail)
	}

	confirm := body(t, f.handler.do(http.MethodGet, f.confirmPath(f.linuxID), nil))
	for _, want := range []string{
		"重选地址 datasets · linux-web", "将优先选择物理局域网中仍可达的更优地址",
		`name="server_id" value="` + f.linuxID + `"`, "确认重选", "取消",
	} {
		if !strings.Contains(confirm, want) {
			t.Fatalf("重选确认页缺少 %q：\n%s", want, confirm)
		}
	}

	post := f.handler.do(http.MethodPost, "/ui/shares/"+f.shareID+"/reselect", url.Values{
		"csrf_token": {csrfToken(t, f.handler)}, "server_id": {f.linuxID},
	})
	postBody := body(t, post)
	if post.StatusCode != http.StatusSeeOther ||
		!strings.HasPrefix(post.Header.Get("Location"), f.confirmPath(f.linuxID)+"&task=") {
		t.Fatalf("启动重选状态码 = %d 跳转 = %q：%s",
			post.StatusCode, post.Header.Get("Location"), postBody)
	}
	page := waitUntilReselectDone(t, f.handler, post.Header.Get("Location"))
	for _, want := range []string{
		"已是更优地址，无需切换", "办公物理局域网", "共享存储版本号：1",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("已是最优地址结果页缺少 %q：\n%s", want, page)
		}
	}
	for _, leak := range []string{"192.0.2.10", "100.64.10.10", "mount-user", "DATASETS_MOUNT_PASSWORD-value-不回显"} {
		if strings.Contains(page, leak) {
			t.Fatalf("重选结果页泄露 %q：\n%s", leak, page)
		}
	}
	if len(f.linux.State().History) != 0 {
		t.Fatalf("已是最优地址时不应改动远端状态：%v", f.linux.State().History)
	}

	rows, err := f.store.QueryAudit(store.AuditFilter{
		OwnerOnly: true, Actions: []string{"share-reselect"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != "ok" ||
		rows[0].Source != store.AuditSourceWebUI || rows[0].SourceIP != "192.0.2.1" {
		t.Fatalf("重选审计来源不正确：%+v", rows)
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(rows[0].Command), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["share"] != "datasets" || summary["server"] != "linux-web" ||
		summary["result"] != "best-address-unchanged" ||
		summary["version"] != float64(1) || summary["force"] != false {
		t.Fatalf("重选审计摘要 = %#v", summary)
	}
	if strings.Contains(rows[0].Command, "192.0.2.10") ||
		strings.Contains(rows[0].Command, "mount-user") {
		t.Fatalf("重选审计摘要不得含地址或账号：%s", rows[0].Command)
	}
}

func TestShareReselectSwitchesToPhysicalAddressWithoutLeakingIt(t *testing.T) {
	f := newReselectWebFixture(t, "100.64.10.10")

	post := f.handler.do(http.MethodPost, "/ui/shares/"+f.shareID+"/reselect", url.Values{
		"csrf_token": {csrfToken(t, f.handler)}, "server_id": {f.linuxID},
	})
	if post.StatusCode != http.StatusSeeOther {
		t.Fatalf("启动重选状态码 = %d：%s", post.StatusCode, body(t, post))
	}
	page := waitUntilReselectDone(t, f.handler, post.Header.Get("Location"))
	for _, want := range []string{
		"已切换到更优地址", "办公物理局域网", "（物理局域网）", "共享存储版本号：1",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("切换成功结果页缺少 %q：\n%s", want, page)
		}
	}
	for _, leak := range []string{"192.0.2.10", "100.64.10.10", "mount-user", "DATASETS_MOUNT_PASSWORD-value-不回显"} {
		if strings.Contains(page, leak) {
			t.Fatalf("切换成功结果页泄露 %q：\n%s", leak, page)
		}
	}
	state := f.linux.State()
	if !state.Mounted || len(state.FstabLines) != 1 ||
		!strings.Contains(state.FstabLines[0], "//192.0.2.10/datasets") {
		t.Fatalf("切换后的远端状态 = %+v", state)
	}
}

func TestShareReselectBusyListsProcessesAndForceSwitches(t *testing.T) {
	f := newReselectWebFixture(t, "100.64.10.10")
	f.linux.BusyProcesses = []sharemounttest.BusyProcess{{
		ProcessID: "2345", User: "worker", Command: "tar",
	}}

	post := f.handler.do(http.MethodPost, "/ui/shares/"+f.shareID+"/reselect", url.Values{
		"csrf_token": {csrfToken(t, f.handler)}, "server_id": {f.linuxID},
	})
	busyPage := waitUntilReselectDone(t, f.handler, post.Header.Get("Location"))
	for _, want := range []string{
		"挂载被占用，保留旧地址与旧配置", "占用挂载点的进程",
		"2345", "worker", "tar", "强制切换地址", "稍后再试",
	} {
		if !strings.Contains(busyPage, want) {
			t.Fatalf("占用结果页缺少 %q：\n%s", want, busyPage)
		}
	}
	state := f.linux.State()
	if !state.Mounted || !strings.Contains(state.FstabLines[0], "//100.64.10.10/datasets") {
		t.Fatalf("不带强制时不应改动旧状态：%+v", state)
	}

	f.linux.ForceSucceeds = true
	force := f.handler.do(http.MethodPost, "/ui/shares/"+f.shareID+"/reselect", url.Values{
		"csrf_token": {csrfToken(t, f.handler)},
		"server_id":  {f.linuxID}, "force": {"1"},
	})
	forcePage := waitUntilReselectDone(t, f.handler, force.Header.Get("Location"))
	if !strings.Contains(forcePage, "已切换到更优地址") || !strings.Contains(forcePage, "强制执行") {
		t.Fatalf("强制切换结果页：\n%s", forcePage)
	}
	state = f.linux.State()
	if !state.Mounted || !strings.Contains(state.FstabLines[0], "//192.0.2.10/datasets") {
		t.Fatalf("强制切换后的远端状态 = %+v", state)
	}
	rows, err := f.store.QueryAudit(store.AuditFilter{
		OwnerOnly: true, Actions: []string{"share-reselect"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("应有占用与强制两条审计行：%+v", rows)
	}
	busySeen, forcedSeen := false, false
	for _, row := range rows {
		if row.Source != store.AuditSourceWebUI || row.Status != "ok" {
			t.Fatalf("占用与强制的审计来源或状态不正确：%+v", row)
		}
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

func TestShareReselectWindowsServerIsRejectedAndAudited(t *testing.T) {
	f := newReselectWebFixture(t, "192.0.2.10")

	post := f.handler.do(http.MethodPost, "/ui/shares/"+f.shareID+"/reselect", url.Values{
		"csrf_token": {csrfToken(t, f.handler)}, "server_id": {f.windowsID},
	})
	if post.StatusCode != http.StatusSeeOther {
		t.Fatalf("Windows 重选请求状态码 = %d：%s", post.StatusCode, body(t, post))
	}
	page := waitUntilReselectDone(t, f.handler, post.Header.Get("Location"))
	for _, want := range []string{
		"Windows 服务器不能重选共享存储地址", "每条命令执行前本来就会重新选址",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("Windows 拒绝页缺少 %q：\n%s", want, page)
		}
	}
	if strings.Contains(page, "确认重选") || strings.Contains(page, "强制切换地址") {
		t.Fatalf("Windows 拒绝页不得提供确认或强制按钮：\n%s", page)
	}
	rows, err := f.store.QueryAudit(store.AuditFilter{
		OwnerOnly: true, Actions: []string{"share-reselect"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != "error" ||
		rows[0].Source != store.AuditSourceWebUI || rows[0].SourceIP != "192.0.2.1" {
		t.Fatalf("Windows 拒绝的重选审计 = %+v", rows)
	}
	if !strings.Contains(rows[0].Command, `"result":"windows-unsupported"`) {
		t.Fatalf("Windows 拒绝的审计摘要 = %s", rows[0].Command)
	}
}

func TestShareReselectExpiredTaskPromptsRestart(t *testing.T) {
	f := newReselectWebFixture(t, "192.0.2.10")

	resp := f.handler.do(
		http.MethodGet, f.confirmPath(f.linuxID)+"&task=missing-task", nil,
	)
	page := body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("失效任务页状态码 = %d：\n%s", resp.StatusCode, page)
	}
	for _, want := range []string{
		"重选任务结果已失效", "重新确认并发起", "确认重选",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("重选失效任务页缺少 %q：\n%s", want, page)
		}
	}
}
