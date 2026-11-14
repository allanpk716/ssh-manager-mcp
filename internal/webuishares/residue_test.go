package webuishares

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sharemanage"
	"ssh-manager-mcp/internal/sharemounttest"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
	"ssh-manager-mcp/internal/webui"
)

// newResidueWebFixture 组装残留检查页的集成测试环境：真实临时加密
// vault、多台状态化 Linux 与 macOS 假服务器、一台离线服务器与一台
// Windows 登记服务器，覆盖有残留、仅有撤销标记、未检查、无残留与跳过
// 五类结论。
func newResidueWebFixture(t *testing.T) *residueWebFixture {
	t.Helper()
	st := newIntegrationStore(t)
	for _, device := range []string{"NAS", "挂载目标", "windows 服务器"} {
		if _, err := st.AddDevice(device, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	serverIDs := map[string]string{}
	loginCredential := &models.Credential{Type: models.CredPassword, Secret: []byte("login-pw")}
	sudoCredential := &models.Credential{Type: models.CredPassword, Secret: []byte("sudo-pw")}
	register := func(name string, handler func(string, io.Reader) (string, string, int), offline bool) string {
		t.Helper()
		addr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{
			Password: "login-pw", SudoPassword: "sudo-pw", Exec: handler,
		})
		if offline {
			cleanup()
		} else {
			t.Cleanup(cleanup)
		}
		host, portText, _ := strings.Cut(addr, ":")
		port, _ := strconv.Atoi(portText)
		added, err := st.AddServerWithCredentialsToDevice(&models.Server{
			Name: name, Host: host, Port: port, User: "deploy",
			ShellType: models.ShellPOSIX,
		}, loginCredential, sudoCredential, "挂载目标")
		if err != nil {
			t.Fatal(err)
		}
		server, err := st.GetServer(added.ServerID)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SaveEntryPin(server, hostKey.Marshal()); err != nil {
			t.Fatal(err)
		}
		return added.ServerID
	}

	out := &sharemounttest.LinuxServer{ShareName: "datasets"}
	in := &sharemounttest.LinuxServer{ShareName: "datasets"}
	media := &sharemounttest.LinuxServer{ShareName: "media"}
	marker := &sharemounttest.LinuxServer{ShareName: "datasets"}
	offline := &sharemounttest.LinuxServer{ShareName: "datasets"}
	darwin := &sharemounttest.DarwinServer{ShareName: "archive"}

	serverIDs["linux-in"] = register("linux-in", in.Handler, false)
	serverIDs["linux-out"] = register("linux-out", out.Handler, false)
	serverIDs["linux-media"] = register("linux-media", media.Handler, false)
	serverIDs["linux-marker"] = register("linux-marker", marker.Handler, false)
	serverIDs["linux-offline"] = register("linux-offline", offline.Handler, true)
	serverIDs["darwin-out"] = register("darwin-out", darwin.Handler, false)
	serverIDs["windows-web"] = mustServerToDevice(
		t, st, "windows-web", models.ShellPowerShell, "windows 服务器",
	)

	for name, value := range map[string]string{
		"DATASETS_MOUNT_PASSWORD": "datasets-password",
		"ARCHIVE_MOUNT_PASSWORD":  "archive-password",
		"MEDIA_MOUNT_PASSWORD":    "media-password",
	} {
		if err := st.AddSecret(name, "共享存储挂载密码", []byte(value), store.CLISource()); err != nil {
			t.Fatal(err)
		}
	}
	for name, drive := range map[string]string{
		"datasets": "Z", "archive": "Y", "media": "X",
	} {
		if _, err := st.AddSharedStorage(store.SharedStorageInput{
			Name: name, Device: "NAS", User: "mount-user",
			PasswordSecret: name + "_MOUNT_PASSWORD", Drive: drive,
		}, store.CLISource()); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.AllowSecrets([]string{"DATASETS_MOUNT_PASSWORD"},
		[]string{"linux-in"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := st.AllowSecrets([]string{"MEDIA_MOUNT_PASSWORD"},
		[]string{"linux-in"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := st.AllowSecrets([]string{"ARCHIVE_MOUNT_PASSWORD"},
		[]string{"darwin-out"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	datasets, err := st.GetSharedStorage("datasets")
	if err != nil {
		t.Fatal(err)
	}
	mediaShare, err := st.GetSharedStorage("media")
	if err != nil {
		t.Fatal(err)
	}
	archive, err := st.GetSharedStorage("archive")
	if err != nil {
		t.Fatal(err)
	}
	out.ShareID = datasets.ID
	out.SeedManagedConfiguration("192.0.2.10", datasets.Version)
	// 第 23 条剩余问题来自“仅有持久配置与凭据文件、当前没有挂载”的自
	// 然靶子；默认让残留靶子保持未挂载，占用流程测试再单独补回挂载。
	out.Mounted = false
	in.ShareID = datasets.ID
	in.SeedManagedConfiguration("192.0.2.10", datasets.Version)
	media.ShareID = mediaShare.ID
	media.SeedManagedConfiguration("192.0.2.11", mediaShare.Version)
	darwin.SeedManagedConfiguration("192.0.2.12", archive.Version)
	offline.ShareID = datasets.ID
	offline.SeedManagedConfiguration("192.0.2.10", datasets.Version)
	marker.ShareVersion = 3
	marker.RevokedMarker = "sshmgr-share=datasets;sshmgr-version=3;sshmgr-state=revoked"
	marker.RevokedMarkerPath = "/etc/sshmgr/shares/datasets.revoked"
	if _, err := st.DeleteSharedStorage("archive", store.CLISource()); err != nil {
		t.Fatal(err)
	}

	remoteTasks := RemoteTasksFromStore(openStoreForTest(st))
	h := startIntegrationHandler(t, st, func(config *webui.Config) {
		config.SharesRemoteTasks = remoteTasks
		config.ShareLatestChecks = remoteTasks
		config.ShareResidueRunner = ResidueTasksFromStoreWithContext(
			openStoreForTest(st), context.Background(), remoteTasks,
		)
		config.ShareUnmountRunner = UnmountTasksFromStore(openStoreForTest(st))
		config.ShareRevokeRunner = RevokeTasksFromStore(openStoreForTest(st))
	})
	return &residueWebFixture{
		store: st, handler: h, servers: serverIDs,
		out: out, media: media, marker: marker, darwin: darwin,
		shareID: datasets.ID,
	}
}

type residueWebFixture struct {
	store   *store.Store
	handler sessionHandler
	servers map[string]string
	out     *sharemounttest.LinuxServer
	media   *sharemounttest.LinuxServer
	marker  *sharemounttest.LinuxServer
	darwin  *sharemounttest.DarwinServer
	shareID string
}

func waitUntilResidueDone(t *testing.T, h sessionHandler, taskPath string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp := h.do(http.MethodGet, taskPath, nil)
		page := body(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("残留任务页状态码 = %d：\n%s", resp.StatusCode, page)
		}
		if !strings.Contains(page, "检查进行中") && !strings.Contains(page, "清理进行中") {
			return page
		}
		if time.Now().After(deadline) {
			t.Fatalf("残留任务超时未完成：\n%s", page)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestShareResidueTaskReportsMixedServersAndCounts(t *testing.T) {
	f := newResidueWebFixture(t)
	csrf := csrfToken(t, f.handler)

	detail := body(t, f.handler.do(http.MethodGet, "/ui/shares/"+f.shareID, nil))
	for _, want := range []string{
		"残留检查", `action="/ui/shares/` + f.shareID + `/residue"`,
		`name="scope" value="share"`, `name="scope" value="all"`,
		"检查本共享残留", "检查全部共享残留",
	} {
		if !strings.Contains(detail, want) {
			t.Fatalf("详情页的残留检查区域缺少 %q：\n%s", want, detail)
		}
	}

	post := f.handler.do(http.MethodPost, "/ui/shares/"+f.shareID+"/residue", url.Values{
		"csrf_token": {csrf}, "scope": {"share"},
	})
	postBody := body(t, post)
	if post.StatusCode != http.StatusSeeOther ||
		!strings.HasPrefix(post.Header.Get("Location"), "/ui/shares/"+f.shareID+"/residue?task=") {
		t.Fatalf("启动残留检查状态码 = %d 跳转 = %q：%s",
			post.StatusCode, post.Header.Get("Location"), postBody)
	}
	page := waitUntilResidueDone(t, f.handler, post.Header.Get("Location"))
	for _, want := range []string{
		"残留检查完成", "检查范围",
		"汇总：1 台有残留，1 台仅有撤销标记，1 台未检查，3 台无残留，1 台跳过",
		"linux-out", "发现残留", "datasets", "配置条目、凭据文件",
		"服务器不在可挂载范围内", "清理",
		"linux-marker", "仅有撤销标记", "撤销后的预期状态，不需要清理",
		"linux-offline", "未检查", "连接服务器或执行枚举命令失败",
		"windows-web", "跳过", "sshmgr 不在其上留下任何东西",
		"linux-in", "无残留",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("残留检查结果页缺少 %q：\n%s", want, page)
		}
	}
	for _, leak := range []string{
		"192.0.2.10", "192.0.2.11", "192.0.2.12", "mount-user",
		"datasets-password", "archive-password", "media-password",
	} {
		if strings.Contains(page, leak) {
			t.Fatalf("残留检查结果页泄露 %q：\n%s", leak, page)
		}
	}
	if got := strings.Count(page, `/residue-cleanup"`); got != 1 {
		t.Fatalf("按共享检查时只有 linux-out 提供清理入口，实际 %d 个：\n%s", got, page)
	}

	rows, err := f.store.QueryAudit(store.AuditFilter{
		OwnerOnly: true, Actions: []string{"share-residue-check"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != "ok" ||
		rows[0].Source != store.AuditSourceWebUI || rows[0].SourceIP != "192.0.2.1" {
		t.Fatalf("残留检查审计 = %+v", rows)
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(rows[0].Command), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["share"] != "datasets" {
		t.Fatalf("残留检查审计摘要 = %#v", summary)
	}
	items, _ := summary["servers"].([]any)
	if len(items) != 7 {
		t.Fatalf("残留检查审计应记录全部 7 台服务器：%#v", summary)
	}
	var sawResidue, sawUnchecked, sawSkipped bool
	for _, item := range items {
		result, _ := item.(map[string]any)
		switch result["result"] {
		case sharemanage.ResidueStatusResidue:
			sawResidue = true
		case sharemanage.ResidueStatusUnchecked:
			sawUnchecked = true
			if result["reason"] != "ssh-error" {
				t.Fatalf("未检查服务器的审计原因 = %#v", result)
			}
		case sharemanage.ResidueStatusSkipped:
			sawSkipped = true
		}
	}
	if !sawResidue || !sawUnchecked || !sawSkipped {
		t.Fatalf("残留检查审计应包含逐台固定分类：%#v", summary)
	}
	if strings.Contains(rows[0].Command, "192.0.2.10") ||
		strings.Contains(rows[0].Command, "mount-user") {
		t.Fatalf("残留检查审计摘要不得含地址或账号：%s", rows[0].Command)
	}
}

func TestShareResidueBecomesOutOfScopeRevokeEvidenceAndClears(t *testing.T) {
	f := newResidueWebFixture(t)
	csrf := csrfToken(t, f.handler)

	runResidue := func(scope string) string {
		post := f.handler.do(http.MethodPost, "/ui/shares/"+f.shareID+"/residue", url.Values{
			"csrf_token": {csrf}, "scope": {scope},
		})
		if post.StatusCode != http.StatusSeeOther {
			t.Fatalf("启动残留检查状态码 = %d：%s", post.StatusCode, body(t, post))
		}
		return waitUntilResidueDone(t, f.handler, post.Header.Get("Location"))
	}

	runResidue("share")
	detail := body(t, f.handler.do(http.MethodGet, "/ui/shares/"+f.shareID, nil))
	for _, want := range []string{
		"已移出范围、远端可能仍有配置",
		"linux-out",
		"最近登记：已有 sshmgr 挂载配置与凭据文件",
		`href="/ui/shares/` + f.shareID + `/revoke?server=` + f.servers["linux-out"] + `"`,
	} {
		if !strings.Contains(detail, want) {
			t.Fatalf("残留检查后详情页缺少 %q：\n%s", want, detail)
		}
	}

	check := f.handler.do(http.MethodPost, "/ui/shares/"+f.shareID+"/check", url.Values{
		"csrf_token": {csrf},
	})
	if check.StatusCode != http.StatusSeeOther {
		t.Fatalf("启动现场探测状态码 = %d：%s", check.StatusCode, body(t, check))
	}
	waitRemoteTaskDone(t, f.handler, check.Header.Get("Location"))
	detail = body(t, f.handler.do(http.MethodGet, "/ui/shares/"+f.shareID, nil))
	if !strings.Contains(detail, `revoke?server=`+f.servers["linux-out"]) {
		t.Fatalf("只检查仍在范围内的服务器后丢失范围外残留登记：\n%s", detail)
	}

	cleanup := f.handler.do(http.MethodPost, "/ui/shares/"+f.shareID+"/residue-cleanup", url.Values{
		"csrf_token": {csrf}, "share_name": {"datasets"},
		"server_id": {f.servers["linux-out"]}, "server_name": {"linux-out"},
		"scope": {"share"},
	})
	if cleanup.StatusCode != http.StatusSeeOther {
		t.Fatalf("启动残留清理状态码 = %d：%s", cleanup.StatusCode, body(t, cleanup))
	}
	waitUntilResidueDone(t, f.handler, cleanup.Header.Get("Location"))

	runResidue("share")
	detail = body(t, f.handler.do(http.MethodGet, "/ui/shares/"+f.shareID, nil))
	if strings.Contains(detail, `revoke?server=`+f.servers["linux-out"]) {
		t.Fatalf("确认无残留后详情页仍提供撤销入口：\n%s", detail)
	}
}

func TestAllShareResidueBecomesOutOfScopeRevokeEvidence(t *testing.T) {
	f := newResidueWebFixture(t)

	post := f.handler.do(http.MethodPost, "/ui/shares/"+f.shareID+"/residue", url.Values{
		"csrf_token": {csrfToken(t, f.handler)}, "scope": {"all"},
	})
	if post.StatusCode != http.StatusSeeOther {
		t.Fatalf("启动全部共享残留检查状态码 = %d：%s", post.StatusCode, body(t, post))
	}
	waitUntilResidueDone(t, f.handler, post.Header.Get("Location"))

	detail := body(t, f.handler.do(http.MethodGet, "/ui/shares/"+f.shareID, nil))
	if !strings.Contains(detail, "linux-out") ||
		!strings.Contains(detail, "最近登记：已有 sshmgr 挂载配置与凭据文件") {
		t.Fatalf("全部共享残留检查后详情页缺少可撤销证据：\n%s", detail)
	}
}

func TestShareResidueAllSharesIncludesEveryShare(t *testing.T) {
	f := newResidueWebFixture(t)

	post := f.handler.do(http.MethodPost, "/ui/shares/"+f.shareID+"/residue", url.Values{
		"csrf_token": {csrfToken(t, f.handler)}, "scope": {"all"},
	})
	page := waitUntilResidueDone(t, f.handler, post.Header.Get("Location"))
	for _, want := range []string{
		"检查范围", "全部共享",
		"汇总：3 台有残留，1 台仅有撤销标记，1 台未检查，1 台无残留，1 台跳过",
		"darwin-out", "archive", "共享登记已删除",
		"linux-media", "media",
		"linux-out", "datasets", "服务器不在可挂载范围内",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("全部共享残留结果页缺少 %q：\n%s", want, page)
		}
	}
	if got := strings.Count(page, `/residue-cleanup"`); got != 3 {
		t.Fatalf("全部共享检查应提供 3 个清理入口，实际 %d 个：\n%s", got, page)
	}
}

func TestShareResidueCleanupRunsSameRemoteCleanupAsUnmount(t *testing.T) {
	f := newResidueWebFixture(t)
	check := f.handler.do(http.MethodPost, "/ui/shares/"+f.shareID+"/residue", url.Values{
		"csrf_token": {csrfToken(t, f.handler)}, "scope": {"all"},
	})
	if check.StatusCode != http.StatusSeeOther {
		t.Fatalf("启动残留检查状态码 = %d：%s", check.StatusCode, body(t, check))
	}
	report := waitUntilResidueDone(t, f.handler, check.Header.Get("Location"))
	if !strings.Contains(report, "linux-out") || !strings.Contains(report, "darwin-out") {
		t.Fatalf("残留报告缺少可清理发现：\n%s", report)
	}

	cleanup := f.handler.do(http.MethodPost, "/ui/shares/"+f.shareID+"/residue-cleanup", url.Values{
		"csrf_token":  {csrfToken(t, f.handler)},
		"share_name":  {"archive"},
		"server_id":   {f.servers["darwin-out"]},
		"server_name": {"darwin-out"},
		"scope":       {"all"},
	})
	cleanupBody := body(t, cleanup)
	if cleanup.StatusCode != http.StatusSeeOther ||
		!strings.HasPrefix(cleanup.Header.Get("Location"),
			"/ui/shares/"+f.shareID+"/residue?cleanup=") ||
		!strings.Contains(cleanup.Header.Get("Location"), "&scope=all") {
		t.Fatalf("启动残留清理状态码 = %d 跳转 = %q：%s",
			cleanup.StatusCode, cleanup.Header.Get("Location"), cleanupBody)
	}
	page := waitUntilResidueDone(t, f.handler, cleanup.Header.Get("Location"))
	for _, want := range []string{
		"清理结果", "archive", "darwin-out", "卸载完成",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("残留清理结果页缺少 %q：\n%s", want, page)
		}
	}
	if strings.Contains(page, "占用挂载点的进程") || strings.Contains(page, "强制清理") {
		t.Fatalf("成功清理页不得呈现占用选择：\n%s", page)
	}
	state := f.darwin.State()
	if state.Mounted || len(state.MapLines) != 0 || state.RevokedMarker == "" {
		t.Fatalf("清理后的 macOS 状态 = %+v", state)
	}
	rows, err := f.store.QueryAudit(store.AuditFilter{
		OwnerOnly: true, Actions: []string{"share-unmount"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != "ok" ||
		rows[0].Source != store.AuditSourceWebUI || rows[0].SourceIP != "192.0.2.1" {
		t.Fatalf("残留清理审计 = %+v", rows)
	}
	if !strings.Contains(rows[0].Command, `"share":"archive"`) ||
		!strings.Contains(rows[0].Command, `"server":"darwin-out"`) {
		t.Fatalf("残留清理审计摘要 = %s", rows[0].Command)
	}
}

func TestShareResidueCleanupBusyListsProcessesAndForceCleans(t *testing.T) {
	f := newResidueWebFixture(t)
	f.out.Mounted = true
	f.out.BusyProcesses = []sharemounttest.BusyProcess{{
		ProcessID: "3456", User: "backup", Command: "rsync",
	}}

	cleanup := f.handler.do(http.MethodPost, "/ui/shares/"+f.shareID+"/residue-cleanup", url.Values{
		"csrf_token":  {csrfToken(t, f.handler)},
		"share_name":  {"datasets"},
		"server_id":   {f.servers["linux-out"]},
		"server_name": {"linux-out"},
		"scope":       {"share"},
	})
	busyPage := waitUntilResidueDone(t, f.handler, cleanup.Header.Get("Location"))
	for _, want := range []string{
		"配置已删除，挂载点被占用并保留", "占用挂载点的进程",
		"3456", "backup", "rsync", "强制清理", "稍后再试",
	} {
		if !strings.Contains(busyPage, want) {
			t.Fatalf("清理占用结果页缺少 %q：\n%s", want, busyPage)
		}
	}

	f.out.ForceSucceeds = true
	force := f.handler.do(http.MethodPost, "/ui/shares/"+f.shareID+"/residue-cleanup", url.Values{
		"csrf_token":  {csrfToken(t, f.handler)},
		"share_name":  {"datasets"},
		"server_id":   {f.servers["linux-out"]},
		"server_name": {"linux-out"},
		"scope":       {"share"},
		"force":       {"1"},
	})
	forcePage := waitUntilResidueDone(t, f.handler, force.Header.Get("Location"))
	if !strings.Contains(forcePage, "卸载完成") || !strings.Contains(forcePage, "强制执行") {
		t.Fatalf("强制清理结果页：\n%s", forcePage)
	}
	state := f.out.State()
	if state.Mounted || len(state.FstabLines) != 0 || state.Credential != "" {
		t.Fatalf("强制清理后的远端状态 = %+v", state)
	}
}

func TestShareResidueExpiredTasksPromptRestart(t *testing.T) {
	f := newResidueWebFixture(t)

	check := f.handler.do(http.MethodGet, "/ui/shares/"+f.shareID+"/residue?task=missing-task", nil)
	checkBody := body(t, check)
	if check.StatusCode != http.StatusOK {
		t.Fatalf("失效检查任务页状态码 = %d：\n%s", check.StatusCode, checkBody)
	}
	for _, want := range []string{
		"残留检查任务结果已丢失", "重新检查本共享残留", "重新检查全部共享残留",
	} {
		if !strings.Contains(checkBody, want) {
			t.Fatalf("失效检查任务页缺少 %q：\n%s", want, checkBody)
		}
	}

	cleanup := f.handler.do(
		http.MethodGet, "/ui/shares/"+f.shareID+"/residue?cleanup=missing-task", nil,
	)
	cleanupBody := body(t, cleanup)
	if cleanup.StatusCode != http.StatusOK {
		t.Fatalf("失效清理任务页状态码 = %d：\n%s", cleanup.StatusCode, cleanupBody)
	}
	for _, want := range []string{
		"清理任务结果已失效", "重新检查本共享残留",
	} {
		if !strings.Contains(cleanupBody, want) {
			t.Fatalf("失效清理任务页缺少 %q：\n%s", want, cleanupBody)
		}
	}
}
