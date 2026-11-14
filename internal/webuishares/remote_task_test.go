package webuishares

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
	"ssh-manager-mcp/internal/webui"
)

func TestShareCheckTaskStartsPollsAndShowsEveryConclusion(t *testing.T) {
	st := newIntegrationStore(t)
	release := make(chan struct{})
	linuxHandler := func(command string, _ io.Reader) (string, string, int) {
		if strings.TrimSpace(command) == "uname -s" {
			return "Linux\n", "", 0
		}
		<-release
		return "SSHMGR-LINUX-CHECK mode=full\n" +
			"SSHMGR-LINUX-CHECK index=1 managed=yes share=datasets version=1 mounted=yes\n", "", 0
	}
	darwinHandler := func(command string, _ io.Reader) (string, string, int) {
		if strings.TrimSpace(command) == "uname -s" {
			return "Darwin\n", "", 0
		}
		return "SSHMGR-DARWIN-CHECK mode=limited\n" +
			"SSHMGR-DARWIN-CHECK index=1 mounted=no\n" +
			"SSHMGR-DARWIN-CHECK auto-master=no\n", "", 0
	}
	linuxAddress, linuxKey, stopLinux := testsshd.Start(t, testsshd.Options{
		Password: "login-pw", SudoPassword: "sudo-pw", Exec: linuxHandler,
	})
	t.Cleanup(stopLinux)
	darwinAddress, darwinKey, stopDarwin := testsshd.Start(t, testsshd.Options{
		Password: "login-pw", Exec: darwinHandler,
	})
	t.Cleanup(stopDarwin)
	for _, device := range []string{"web 设备", "mac 设备"} {
		if _, err := st.AddDevice(device, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	linuxID := addRemoteTaskServer(t, st, "linux-web", linuxAddress, linuxKey.Marshal(),
		models.ShellPOSIX, "web 设备", true)
	darwinID := addRemoteTaskServer(t, st, "darwin-web", darwinAddress, darwinKey.Marshal(),
		models.ShellPOSIX, "mac 设备", false)
	_ = mustServerToDevice(t, st, "windows-web", models.ShellPowerShell, "web 设备")
	offlineAdded, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: "offline-web", Host: "127.0.0.1", Port: 1, User: "deploy",
		ShellType: models.ShellPOSIX,
	}, &models.Credential{Type: models.CredPassword, Secret: []byte("login-pw")}, nil, "mac 设备")
	if err != nil {
		t.Fatal(err)
	}
	_ = offlineAdded.ServerID
	datasetsID := seedShare(t, st, "datasets", "NAS", "NAS_MOUNT_PASSWORD", "Z",
		"linux-web", "darwin-web", "windows-web", "offline-web")
	h := newIntegrationHandler(t, st, RemoteTasksFromStore(openStoreForTest(st)))
	csrf := csrfToken(t, h)

	detail := body(t, h.do(http.MethodGet, "/ui/shares/"+datasetsID, nil))
	for _, want := range []string{
		"现场运维", `action="/ui/shares/` + datasetsID + `/check"`, "现场探测",
		`action="/ui/shares/` + datasetsID + `/mount"`, `name="server_id" value="` + linuxID + `"`,
		`name="server_id" value="` + darwinID + `"`, "Windows 服务器不提供挂载按钮",
		"盘符由每条命令执行前的会话内注入提供",
	} {
		if !strings.Contains(detail, want) {
			t.Fatalf("详情页现场运维区缺少 %q：\n%s", want, detail)
		}
	}

	started := h.do(http.MethodPost, "/ui/shares/"+datasetsID+"/check", url.Values{
		"csrf_token": {csrf},
	})
	startedBody := body(t, started)
	if started.StatusCode != http.StatusSeeOther || !strings.HasPrefix(started.Header.Get("Location"), "/ui/shares/"+datasetsID+"/task/") {
		t.Fatalf("现场探测应返回任务页地址，状态码=%d 跳转=%q：%s",
			started.StatusCode, started.Header.Get("Location"), startedBody)
	}
	taskPath := started.Header.Get("Location")
	running := body(t, h.do(http.MethodGet, taskPath, nil))
	if !strings.Contains(running, "现场探测进行中") ||
		!strings.Contains(running, `hx-get="`+taskPath+`"`) ||
		!strings.Contains(running, `hx-trigger="every 1s"`) {
		t.Fatalf("运行中的任务页应定时拉取任务区块：\n%s", running)
	}
	close(release)
	completed := waitRemoteTaskDone(t, h, taskPath)
	for _, want := range []string{
		"现场探测完成", "linux-web", "已挂载", "观察时间",
		"darwin-web", "未挂载", "受限检测", "没有配置 sudo 凭据",
		"macOS 系统主配置已被重置", "windows-web", "不检测",
		"offline-web", "不可达", "重新发起现场探测",
	} {
		if !strings.Contains(completed, want) {
			t.Fatalf("完成任务缺少 %q：\n%s", want, completed)
		}
	}
	if strings.Contains(completed, "NAS_MOUNT_PASSWORD-value-不回显") || strings.Contains(completed, "127.0.0.1:") {
		t.Fatalf("现场探测结果不得包含机密值或地址：\n%s", completed)
	}
}

func TestShareMountTaskShowsResultAndLostTaskCanRestart(t *testing.T) {
	st := newIntegrationStore(t)
	fake := &webMountFakeServer{}
	address, hostKey, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "login-pw", SudoPassword: "sudo-pw", Exec: fake.handler,
	})
	t.Cleanup(cleanup)
	seedRemoteTaskMountTopology(t, st)
	serverID := addRemoteTaskServer(t, st, "linux-web", address, hostKey.Marshal(),
		models.ShellPOSIX, "linux-device", true)
	datasetsID := seedShare(t, st, "datasets", "NAS", "NAS_MOUNT_PASSWORD", "Z", "linux-web")
	h := newIntegrationHandler(t, st, RemoteTasksFromStore(openStoreForTest(st)))
	csrf := csrfToken(t, h)

	started := h.do(http.MethodPost, "/ui/shares/"+datasetsID+"/mount", url.Values{
		"csrf_token": {csrf}, "server_id": {serverID},
	})
	if started.StatusCode != http.StatusSeeOther {
		t.Fatalf("挂载任务应立即返回任务页，状态码=%d：%s",
			started.StatusCode, body(t, started))
	}
	completed := waitRemoteTaskDone(t, h, started.Header.Get("Location"))
	for _, want := range []string{
		"挂载完成", "/mnt/sshmgr/datasets", "实际网络", "办公虚拟局域网",
		"物理局域网地址不通", "NAS_MOUNT_PASSWORD-value-不回显",
	} {
		if want == "NAS_MOUNT_PASSWORD-value-不回显" {
			if strings.Contains(completed, want) {
				t.Fatalf("挂载结果不得包含机密值：\n%s", completed)
			}
			continue
		}
		if !strings.Contains(completed, want) {
			t.Fatalf("挂载结果缺少 %q：\n%s", want, completed)
		}
	}
	fake.setAction("no-change")
	repeat := h.do(http.MethodPost, "/ui/shares/"+datasetsID+"/mount", url.Values{
		"csrf_token": {csrf}, "server_id": {serverID},
	})
	repeatResult := waitRemoteTaskDone(t, h, repeat.Header.Get("Location"))
	if !strings.Contains(repeatResult, "无需改动") {
		t.Fatalf("重复挂载任务应呈现无需改动：\n%s", repeatResult)
	}
	rows, err := st.QueryAudit(store.AuditFilter{
		OwnerOnly: true, Source: store.AuditSourceWebUI, Actions: []string{"share-mount"},
	})
	if err != nil || len(rows) != 2 || rows[0].SourceIP != "192.0.2.1" ||
		strings.Contains(rows[0].Command, "mount-password") {
		t.Fatalf("Web UI 挂载审计不正确：%v %+v", err, rows)
	}

	lost := body(t, h.do(http.MethodGet, "/ui/shares/"+datasetsID+"/task/missing", nil))
	for _, want := range []string{"任务结果已丢失", "进程重启后任务不排队补跑", "重新发起现场探测", "返回共享存储详情"} {
		if !strings.Contains(lost, want) {
			t.Fatalf("丢失任务应提示可重新发起，缺少 %q：\n%s", want, lost)
		}
	}
}

func TestShareMountTaskShowsFailureAndWindowsSubmissionIsRefused(t *testing.T) {
	st := newIntegrationStore(t)
	fake := &webMountFakeServer{mountExit: 90}
	address, hostKey, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "login-pw", SudoPassword: "sudo-pw", Exec: fake.handler,
	})
	t.Cleanup(cleanup)
	seedRemoteTaskMountTopology(t, st)
	serverID := addRemoteTaskServer(t, st, "linux-web", address, hostKey.Marshal(),
		models.ShellPOSIX, "linux-device", true)
	windowsID := mustServerToDevice(t, st, "windows-web", models.ShellPowerShell, "windows 设备")
	datasetsID := seedShare(t, st, "datasets", "NAS", "NAS_MOUNT_PASSWORD", "Z",
		"linux-web", "windows-web")
	h := newIntegrationHandler(t, st, RemoteTasksFromStore(openStoreForTest(st)))
	csrf := csrfToken(t, h)

	windows := h.do(http.MethodPost, "/ui/shares/"+datasetsID+"/mount", url.Values{
		"csrf_token": {csrf}, "server_id": {windowsID},
	})
	windowsBody := body(t, windows)
	if windows.StatusCode != http.StatusBadRequest ||
		!strings.Contains(windowsBody, "Windows 服务器不能持久挂载共享存储") ||
		!strings.Contains(windowsBody, "盘符由每条命令执行前的会话内注入提供") {
		t.Fatalf("Windows 提交应被拒绝，状态码=%d：\n%s", windows.StatusCode, windowsBody)
	}

	started := h.do(http.MethodPost, "/ui/shares/"+datasetsID+"/mount", url.Values{
		"csrf_token": {csrf}, "server_id": {serverID},
	})
	failed := waitRemoteTaskDone(t, h, started.Header.Get("Location"))
	for _, want := range []string{"挂载失败", "共享存储已从这台服务器撤销", "重新发起挂载"} {
		if !strings.Contains(failed, want) {
			t.Fatalf("失败任务应呈现错误与重新发起入口，缺少 %q：\n%s", want, failed)
		}
	}
}

// TestSuccessfulMountBecomesOutOfScopeRevokeEvidence 验证成功挂载任务也会
// 登记远端状态：服务器之后再被移出可挂载范围，详情页仍能提供撤销授权
// 入口，不必先跑一次现场探测。
func TestSuccessfulMountBecomesOutOfScopeRevokeEvidence(t *testing.T) {
	st := newIntegrationStore(t)
	fake := &webMountFakeServer{}
	address, hostKey, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "login-pw", SudoPassword: "sudo-pw", Exec: fake.handler,
	})
	t.Cleanup(cleanup)
	seedRemoteTaskMountTopology(t, st)
	serverID := addRemoteTaskServer(t, st, "linux-web", address, hostKey.Marshal(),
		models.ShellPOSIX, "linux-device", true)
	datasetsID := seedShare(t, st, "datasets", "NAS", "NAS_MOUNT_PASSWORD", "Z", "linux-web")
	remoteTasks := RemoteTasksFromStore(openStoreForTest(st))
	h := startIntegrationHandler(t, st, func(config *webui.Config) {
		config.SharesRemoteTasks = remoteTasks
		config.ShareLatestChecks = remoteTasks
		config.ShareRevokeRunner = RevokeTasksFromStore(openStoreForTest(st))
	})
	csrf := csrfToken(t, h)

	started := h.do(http.MethodPost, "/ui/shares/"+datasetsID+"/mount", url.Values{
		"csrf_token": {csrf}, "server_id": {serverID},
	})
	if started.StatusCode != http.StatusSeeOther {
		t.Fatalf("挂载任务应立即返回任务页，状态码=%d：%s",
			started.StatusCode, body(t, started))
	}
	waitRemoteTaskDone(t, h, started.Header.Get("Location"))
	if err := st.DisallowSecrets(
		[]string{"NAS_MOUNT_PASSWORD"}, []string{"linux-web"}, store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}

	detail := body(t, h.do(http.MethodGet, "/ui/shares/"+datasetsID, nil))
	for _, want := range []string{
		"已移出范围、远端可能仍有配置",
		"linux-web",
		"最近登记：已挂载，且已有 sshmgr 挂载配置",
		`href="/ui/shares/` + datasetsID + `/revoke?server=` + serverID + `"`,
	} {
		if !strings.Contains(detail, want) {
			t.Fatalf("成功挂载后的已移出范围区域缺少 %q：\n%s", want, detail)
		}
	}
	if strings.Contains(detail, "NAS_MOUNT_PASSWORD-value-不回显") {
		t.Fatalf("已移出范围区域不得显示密码：\n%s", detail)
	}
}

func waitRemoteTaskDone(t *testing.T, h sessionHandler, taskPath string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		page := body(t, h.do(http.MethodGet, taskPath, nil))
		if strings.Contains(page, "现场探测完成") || strings.Contains(page, "挂载完成") ||
			strings.Contains(page, "挂载失败") || strings.Contains(page, "现场探测失败") {
			return page
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("任务未及时完成：%s", taskPath)
	return ""
}

func addRemoteTaskServer(
	t *testing.T, st *store.Store, name, address string, hostKey []byte,
	shell models.ShellType, device string, withSudo bool,
) string {
	t.Helper()
	loginCredential := &models.Credential{Type: models.CredPassword, Secret: []byte("login-pw")}
	var sudoCredential *models.Credential
	if withSudo {
		sudoCredential = &models.Credential{Type: models.CredPassword, Secret: []byte("sudo-pw")}
	}
	added, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: name, Host: address[:strings.LastIndex(address, ":")],
		Port: remoteTaskPort(t, address), User: "deploy", ShellType: shell,
	}, loginCredential, sudoCredential, device)
	if err != nil {
		t.Fatal(err)
	}
	server, err := st.GetServer(added.ServerID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveEntryPin(server, hostKey); err != nil {
		t.Fatal(err)
	}
	return added.ServerID
}

func remoteTaskPort(t *testing.T, address string) int {
	t.Helper()
	value := address[strings.LastIndex(address, ":")+1:]
	port, err := strconv.Atoi(value)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func seedRemoteTaskMountTopology(t *testing.T, st *store.Store) {
	t.Helper()
	if _, err := st.AddDevice("NAS", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("linux-device", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("windows 设备", "", ""); err != nil {
		t.Fatal(err)
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
		device  string
		network string
		address string
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
}

type webMountFakeServer struct {
	mu         sync.Mutex
	mountExit  int
	nextAction string
}

func (f *webMountFakeServer) setAction(action string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextAction = action
}

func (f *webMountFakeServer) handler(command string, stdin io.Reader) (string, string, int) {
	_, _ = io.ReadAll(stdin)
	switch {
	case strings.TrimSpace(command) == "uname -s":
		return "Linux\n", "", 0
	case strings.Contains(command, "sshmgr_probe_one"):
		return "SSHMGR-PROBE 0 fail 2 no-route\nSSHMGR-PROBE 1 ok 2 \n", "", 0
	case strings.Contains(command, "SSHMGR-LINUX-MOUNT"):
		f.mu.Lock()
		defer f.mu.Unlock()
		action := "first-mount-success"
		if f.nextAction != "" {
			action = f.nextAction
		}
		if f.mountExit != 0 {
			return "", "SSHMGR-LINUX-MOUNT share-revoked", f.mountExit
		}
		return "SSHMGR-LINUX-MOUNT result=" + action + " selected=1\n", "", 0
	default:
		return "", fmt.Sprintf("unexpected command: %s", command), 90
	}
}
