package webuishares

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/sharemounttest"
	"ssh-manager-mcp/internal/store"
)

func TestShareRevokeConfirmationExplainsRegistrationAndRemoteCleanup(t *testing.T) {
	fixture := newUnmountFixture(t)

	detail := body(t, fixture.handler.do(http.MethodGet, "/ui/shares/"+fixture.shareID, nil))
	for _, want := range []string{
		"撤销授权", `href="` + fixture.revokePath(fixture.linuxID) + `"`,
	} {
		if !strings.Contains(detail, want) {
			t.Fatalf("共享存储详情页的撤销授权区域缺少 %q：\n%s", want, detail)
		}
	}

	page := body(t, fixture.handler.do(http.MethodGet, fixture.revokePath(fixture.linuxID), nil))
	for _, want := range []string{
		"撤销 datasets · linux-web",
		"共享存储版本号将加一",
		"共享已从服务器撤销",
		"Linux 与 macOS 服务器会立即连接并清理远端持久配置",
		"不可达时当场失败，不排队重试",
		`name="server_id" value="` + fixture.linuxID + `"`,
		"同时卸载现有挂载",
		"同时更换共享密码",
		`type="password" name="new_password"`,
		`type="file" name="new_password_file"`,
		"确认撤销授权",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("撤销授权确认页缺少 %q：\n%s", want, page)
		}
	}
	if strings.Contains(page, "mount-password") || strings.Contains(page, "login-pw") {
		t.Fatalf("撤销授权确认页不得显示密码：\n%s", page)
	}
}

func TestShareRevokeWithUnmountSucceedsAndAuditsWebUISource(t *testing.T) {
	fixture := newUnmountFixture(t)

	post := fixture.handler.do(http.MethodPost, "/ui/shares/"+fixture.shareID+"/revoke", url.Values{
		"csrf_token": {csrfToken(t, fixture.handler)},
		"server_id":  {fixture.linuxID},
		"unmount":    {"1"},
	})
	if post.StatusCode != http.StatusSeeOther {
		t.Fatalf("启动撤销授权任务状态码 = %d：\n%s", post.StatusCode, body(t, post))
	}
	taskPath := post.Header.Get("Location")
	if !strings.Contains(taskPath, "task=") {
		t.Fatalf("启动撤销授权任务应跳转到任务页：%s", taskPath)
	}
	page := waitUntilRevokeDone(t, fixture.handler, taskPath)
	for _, want := range []string{
		"撤销授权完成", "共享已从服务器撤销", "共享存储版本号", "2",
		"远端持久配置已删除，挂载已卸载",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("撤销授权结果页缺少 %q：\n%s", want, page)
		}
	}
	for _, forbidden := range []string{
		"DATASETS_MOUNT_PASSWORD-value-不回显", "mount-password", "login-pw", "sudo-pw", "192.0.2.10",
	} {
		if strings.Contains(page, forbidden) {
			t.Fatalf("撤销授权结果页泄露 %q：\n%s", forbidden, page)
		}
	}

	share, err := fixture.store.GetSharedStorage("datasets")
	if err != nil {
		t.Fatal(err)
	}
	if share.Version != 2 || share.ChangeNote != "共享已从服务器撤销" ||
		len(share.MountableServers) != 1 || share.MountableServers[0].Name != "windows-web" {
		t.Fatalf("撤销授权后的登记不正确：%+v", share)
	}
	state := fixture.linux.State()
	if state.Mounted || len(state.FstabLines) != 0 || state.Credential != "" {
		t.Fatalf("撤销授权后的远端状态不正确：%+v", state)
	}
	for _, action := range []string{"secret-disallow", "share-revoke", "share-revoke-remote"} {
		rows, err := fixture.store.QueryAudit(store.AuditFilter{
			OwnerOnly: true, Actions: []string{action},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].Source != store.AuditSourceWebUI ||
			rows[0].SourceIP != "192.0.2.1" || rows[0].Status != "ok" {
			t.Fatalf("撤销授权审计 %s 不正确：%+v", action, rows)
		}
	}
}

func TestShareRevokeBusyListsProcessesAndForceCleanupSucceeds(t *testing.T) {
	fixture := newUnmountFixture(t)
	fixture.linux.BusyProcesses = []sharemounttest.BusyProcess{
		{ProcessID: "1234", User: "worker", Command: "python"},
	}
	csrf := csrfToken(t, fixture.handler)

	post := fixture.handler.do(http.MethodPost, "/ui/shares/"+fixture.shareID+"/revoke", url.Values{
		"csrf_token": {csrf}, "server_id": {fixture.linuxID}, "unmount": {"1"},
	})
	page := waitUntilRevokeDone(t, fixture.handler, post.Header.Get("Location"))
	for _, want := range []string{
		"配置已删除，挂载点被占用并保留", "占用挂载点的进程",
		"1234", "worker", "python", "强制清理挂载点", "稍后再试",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("撤销授权占用结果页缺少 %q：\n%s", want, page)
		}
	}
	if !fixture.linux.State().Mounted {
		t.Fatalf("不强制时应保留挂载：%+v", fixture.linux.State())
	}

	fixture.linux.ForceSucceeds = true
	force := fixture.handler.do(http.MethodPost, "/ui/shares/"+fixture.shareID+"/revoke", url.Values{
		"csrf_token": {csrf}, "share_id": {fixture.shareID},
		"server_id": {fixture.linuxID}, "server_name": {"linux-web"},
		"server_shell": {"posix"}, "mount_point": {"/mnt/sshmgr/datasets"},
		"retry_cleanup": {"1"}, "force": {"1"},
	})
	if force.StatusCode != http.StatusSeeOther {
		t.Fatalf("启动强制清理状态码 = %d：\n%s", force.StatusCode, body(t, force))
	}
	forcePage := waitUntilRevokeDone(t, fixture.handler, force.Header.Get("Location"))
	if !strings.Contains(forcePage, "撤销授权完成") || !strings.Contains(forcePage, "强制执行") {
		t.Fatalf("强制清理结果页：\n%s", forcePage)
	}
	if fixture.linux.State().Mounted {
		t.Fatalf("强制清理后挂载应被移除：%+v", fixture.linux.State())
	}
	rows, err := fixture.store.QueryAudit(store.AuditFilter{
		OwnerOnly: true, Actions: []string{"share-revoke", "share-revoke-remote", "share-unmount"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Source != store.AuditSourceWebUI || row.SourceIP != "192.0.2.1" {
			t.Fatalf("撤销授权占用与强制清理审计来源不正确：%+v", row)
		}
	}
}

func waitUntilRevokeDone(t *testing.T, h sessionHandler, taskPath string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp := h.do(http.MethodGet, taskPath, nil)
		page := body(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("撤销授权任务页状态码 = %d：\n%s", resp.StatusCode, page)
		}
		if !strings.Contains(page, "撤销授权进行中") {
			return page
		}
		if time.Now().After(deadline) {
			t.Fatalf("撤销授权任务超时未完成：\n%s", page)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (f *unmountFixture) revokePath(serverID string) string {
	return "/ui/shares/" + f.shareID + "/revoke?server=" + serverID
}
