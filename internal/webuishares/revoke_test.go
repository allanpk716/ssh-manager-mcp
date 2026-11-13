package webuishares

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sharemounttest"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
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

func TestAuthorityShareRevokePasswordChangeRequiresReverifyAndHidesValue(t *testing.T) {
	fixture := newUnmountFixture(t)
	if _, err := fixture.store.AddServer(&models.Server{
		Name: "linux-worker", Host: "192.0.2.11", Port: 22, User: "deploy",
		AuthMethod: models.AuthPassword, ShellType: models.ShellPOSIX,
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.AllowSecrets(
		[]string{"DATASETS_MOUNT_PASSWORD"}, []string{"linux-worker"}, store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	views, err := fixture.store.ListSecretsInScope(store.ScopeAllServers())
	if err != nil || len(views) != 1 {
		t.Fatalf("读取挂载密码机密项失败：%v %+v", err, views)
	}
	oldPassword := []byte("DATASETS_MOUNT_PASSWORD-value-不回显")
	newPassword := []byte("new-password-\x00-'\"-密码")
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.Local)
	h := newShareAuthorityHandler(t, fixture.store, &now, func(config *webui.Config) {
		config.ShareRevokeRunner = RevokeTasksFromStore(openStoreForTest(fixture.store))
	})
	now = now.Add(6 * time.Minute)
	csrf := authorityPageCSRF(t, h, fixture.revokePath(fixture.linuxID))

	rejected := h.doMultipart(t, "/ui/shares/"+fixture.shareID+"/revoke", map[string]string{
		"csrf_token": csrf, "server_id": fixture.linuxID,
		"unmount": "1", "change_password": "1",
	}, "new_password_file", newPassword)
	rejectedBody := body(t, rejected)
	resume := fixture.revokePath(fixture.linuxID) + "&unmount=1&change_password=1"
	if rejected.StatusCode != http.StatusSeeOther ||
		rejected.Header.Get("Location") != "/ui/reverify?next="+url.QueryEscape(resume) {
		t.Fatalf("未重新验证的换密码撤销应跳转重新验证，状态码=%d 跳转=%q：\n%s",
			rejected.StatusCode, rejected.Header.Get("Location"), rejectedBody)
	}
	share, err := fixture.store.GetSharedStorage("datasets")
	if err != nil || share.Version != 2 || len(share.MountableServers) != 3 {
		t.Fatalf("未重新验证前不应撤销登记：%+v %v", share, err)
	}

	reverify := h.do(http.MethodPost, "/ui/reverify", url.Values{
		"csrf_token": {authorityPageCSRF(t, h, "/ui/reverify?next="+url.QueryEscape(resume))},
		"password":   {"correct-password"}, "next": {resume},
	})
	if reverify.StatusCode != http.StatusSeeOther || reverify.Header.Get("Location") != resume {
		t.Fatalf("重新验证后应回到撤销确认页，状态码=%d 跳转=%q：\n%s",
			reverify.StatusCode, reverify.Header.Get("Location"), body(t, reverify))
	}
	resumedPage := body(t, h.do(http.MethodGet, resume, nil))
	if !strings.Contains(resumedPage, `name="unmount" value="1" checked`) ||
		!strings.Contains(resumedPage, `name="change_password" value="1" checked`) {
		t.Fatalf("重新验证后的撤销确认页应保留选项：\n%s", resumedPage)
	}

	confirmed := h.doMultipart(t, "/ui/shares/"+fixture.shareID+"/revoke", map[string]string{
		"csrf_token": csrf, "server_id": fixture.linuxID,
		"unmount": "1", "change_password": "1",
	}, "new_password_file", newPassword)
	if confirmed.StatusCode != http.StatusSeeOther {
		t.Fatalf("重新验证后的换密码撤销状态码=%d：\n%s",
			confirmed.StatusCode, body(t, confirmed))
	}
	taskPage := waitUntilAuthorityRevokeDone(t, h, confirmed.Header.Get("Location"))
	for _, want := range []string{
		"撤销授权完成", "已更换共享密码",
		"仍可挂载的 Linux 与 macOS 服务器：linux-worker",
		"这些服务器需要 agent 按新版本号重新挂载",
		"桌面手动连接也需要更新密码",
	} {
		if !strings.Contains(taskPage, want) {
			t.Fatalf("换密码撤销结果页缺少 %q：\n%s", want, taskPage)
		}
	}

	secret, err := fixture.store.SecretValueForSharedStorageMount(views[0].ID)
	if err != nil || !bytes.Equal(secret.Value, newPassword) {
		t.Fatalf("新共享密码没有按原始字节保存：%v", err)
	}
	allResponses := resumedPage + taskPage
	for _, secretValue := range [][]byte{oldPassword, newPassword} {
		if strings.Contains(allResponses, string(secretValue)) {
			t.Fatalf("撤销授权页面泄露共享密码：%q", secretValue)
		}
	}
	rows, err := fixture.store.QueryAudit(store.AuditFilter{OwnerOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if strings.Contains(row.Command, string(oldPassword)) ||
			strings.Contains(row.Command, string(newPassword)) {
			t.Fatalf("撤销授权审计泄露共享密码：%s", row.Command)
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

func waitUntilAuthorityRevokeDone(t *testing.T, h authorityHandler, taskPath string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp := h.do(http.MethodGet, taskPath, nil)
		page := body(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("权威端撤销授权任务页状态码 = %d：\n%s", resp.StatusCode, page)
		}
		if !strings.Contains(page, "撤销授权进行中") {
			return page
		}
		if time.Now().After(deadline) {
			t.Fatalf("权威端撤销授权任务超时未完成：\n%s", page)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (h authorityHandler) doMultipart(
	t *testing.T, target string, fields map[string]string, fileField string, fileValue []byte,
) *http.Response {
	t.Helper()
	var requestBody bytes.Buffer
	writer := multipart.NewWriter(&requestBody)
	for name, value := range fields {
		if err := writer.WriteField(name, value); err != nil {
			t.Fatal(err)
		}
	}
	part, err := writer.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": []string{
			`form-data; name="` + fileField + `"; filename="new-password.txt"`,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(fileValue); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, target, &requestBody)
	req.Host = "127.0.0.1:18087"
	req.RemoteAddr = "198.51.100.9:45678"
	req.AddCookie(h.cookie)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Origin", "https://127.0.0.1:18087")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec.Result()
}

func (f *unmountFixture) revokePath(serverID string) string {
	return "/ui/shares/" + f.shareID + "/revoke?server=" + serverID
}
