package webuishares

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/sharemounttest"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

// startScopeRemovalPreview 先执行一次现场探测，再生成把已挂载的 Linux 服
// 务器移出可挂载范围的预览页，返回预览正文、确认摘要与目标授权格。
func startScopeRemovalPreview(t *testing.T, fixture *unmountFixture) (string, string, []string) {
	t.Helper()
	started := fixture.handler.do(http.MethodPost, "/ui/shares/"+fixture.shareID+"/check", url.Values{
		"csrf_token": {csrfToken(t, fixture.handler)},
	})
	if started.StatusCode != http.StatusSeeOther {
		t.Fatalf("启动现场探测状态码 = %d：%s", started.StatusCode, body(t, started))
	}
	waitRemoteTaskDone(t, fixture.handler, started.Header.Get("Location"))
	views, err := fixture.store.ListSecretsInScope(store.ScopeAllServers())
	if err != nil || len(views) != 1 {
		t.Fatalf("读取挂载密码机密项失败：%v %+v", err, views)
	}
	secretID := views[0].ID
	preview := fixture.handler.do(http.MethodPost, "/ui/secrets/authorization/preview", url.Values{
		"csrf_token": {csrfToken(t, fixture.handler)},
		"return_to":  {"/ui/shares/" + fixture.shareID},
		"secret_id":  {secretID},
		"server_id":  {fixture.linuxID, fixture.windowsID},
		"cell__" + secretID + "__" + fixture.linuxID:   {"0"},
		"cell__" + secretID + "__" + fixture.windowsID: {"1"},
	})
	page := body(t, preview)
	if preview.StatusCode != http.StatusOK {
		t.Fatalf("可挂载范围预览状态码 = %d：%s", preview.StatusCode, page)
	}
	target := []string{
		secretID + " " + fixture.linuxID + " 0",
		secretID + " " + fixture.windowsID + " 1",
	}
	return page, pageHiddenValue(t, page, "summary"), target
}

// TestShareScopeConfirmStartsUncheckedServerUnmount 覆盖「取消勾选已挂载服
// 务器并勾选同时卸载」：范围变更生效，卸载任务启动并完成，页面跳到能看
// 到卸载任务状态的汇总页。
func TestShareScopeConfirmStartsUncheckedServerUnmount(t *testing.T) {
	fixture := newUnmountFixture(t)
	preview, summary, target := startScopeRemovalPreview(t, fixture)
	for _, want := range []string{
		"以下服务器将移出可挂载范围", "linux-web", "最近现场探测：已挂载",
		`name="unmount_server" value="` + fixture.linuxID + `"`,
	} {
		if !strings.Contains(preview, want) {
			t.Fatalf("可挂载范围确认页缺少同时卸载选项 %q：\n%s", want, preview)
		}
	}
	if strings.Contains(preview, "同时卸载并撤销授权") || strings.Contains(preview, fixture.revokePath(fixture.linuxID)) {
		t.Fatalf("确认页不得再提供逐台撤销授权跳转链接：\n%s", preview)
	}

	confirmed := fixture.handler.do(http.MethodPost, "/ui/secrets/authorization/confirm", url.Values{
		"csrf_token": {csrfToken(t, fixture.handler)}, "summary": {summary},
		"return_to": {"/ui/shares/" + fixture.shareID}, "target": target,
		"unmount_server": {fixture.linuxID},
	})
	if confirmed.StatusCode != http.StatusSeeOther ||
		!strings.HasPrefix(confirmed.Header.Get("Location"), "/ui/shares/"+fixture.shareID+"/unmount-batch?task=") {
		t.Fatalf("勾选同时卸载后应跳转到卸载任务汇总页，状态码=%d 跳转=%q：%s",
			confirmed.StatusCode, confirmed.Header.Get("Location"), body(t, confirmed))
	}
	batchPage := waitUntilUnmountBatchDone(t, fixture.handler, confirmed.Header.Get("Location"))
	for _, want := range []string{"同时卸载任务", "linux-web", "卸载完成"} {
		if !strings.Contains(batchPage, want) {
			t.Fatalf("卸载任务汇总页缺少 %q：\n%s", want, batchPage)
		}
	}

	views, err := fixture.store.ListSecretsInScope(store.ScopeAllServers())
	if err != nil || len(views) != 1 || len(views[0].AllowedServers) != 1 ||
		views[0].AllowedServers[0] != "windows-web" {
		t.Fatalf("确认后的可挂载范围不正确：%v %+v", err, views)
	}
	state := fixture.linux.State()
	if state.Mounted || len(state.FstabLines) != 0 || state.Credential != "" {
		t.Fatalf("确认后的远端状态应已卸载并清理配置：%+v", state)
	}
	for _, action := range []string{"secret-allow-change", "share-unmount"} {
		rows, err := fixture.store.QueryAudit(store.AuditFilter{
			OwnerOnly: true, Source: store.AuditSourceWebUI, Actions: []string{action},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].SourceIP != "192.0.2.1" || rows[0].Status != "ok" {
			t.Fatalf("确认后的 %s 审计不正确：%+v", action, rows)
		}
	}
}

// TestShareScopeConfirmWithoutUnmountOnlyChangesScope 覆盖「不勾选同时卸
// 载」：只变更可挂载范围，不启动卸载，远端挂载保持原状。
func TestShareScopeConfirmWithoutUnmountOnlyChangesScope(t *testing.T) {
	fixture := newUnmountFixture(t)
	_, summary, target := startScopeRemovalPreview(t, fixture)

	confirmed := fixture.handler.do(http.MethodPost, "/ui/secrets/authorization/confirm", url.Values{
		"csrf_token": {csrfToken(t, fixture.handler)}, "summary": {summary},
		"return_to": {"/ui/shares/" + fixture.shareID}, "target": target,
	})
	confirmedBody := body(t, confirmed)
	if confirmed.StatusCode != http.StatusOK || !strings.Contains(confirmedBody, "已保存授权变更") {
		t.Fatalf("不勾选同时卸载时确认应只保存授权变更，状态码=%d：\n%s",
			confirmed.StatusCode, confirmedBody)
	}
	if strings.Contains(confirmedBody, "unmount-batch") {
		t.Fatalf("不勾选同时卸载时不得跳转卸载任务汇总页：\n%s", confirmedBody)
	}
	views, err := fixture.store.ListSecretsInScope(store.ScopeAllServers())
	if err != nil || len(views) != 1 || len(views[0].AllowedServers) != 1 ||
		views[0].AllowedServers[0] != "windows-web" {
		t.Fatalf("确认后的可挂载范围不正确：%v %+v", err, views)
	}
	state := fixture.linux.State()
	if !state.Mounted || len(state.FstabLines) != 1 || state.Credential == "" {
		t.Fatalf("不勾选同时卸载时远端挂载应保持原状：%+v", state)
	}
	rows, err := fixture.store.QueryAudit(store.AuditFilter{
		OwnerOnly: true, Actions: []string{"share-unmount"},
	})
	if err != nil || len(rows) != 0 {
		t.Fatalf("不勾选同时卸载时不得启动卸载任务：%v %+v", err, rows)
	}
}

// TestShareScopeUnmountBusyOffersForceAfterScopeChange 覆盖联动卸载被占用
// 的场景：汇总页能进入单台任务页，占用进程照旧展示，强制卸载可用。
func TestShareScopeUnmountBusyOffersForceAfterScopeChange(t *testing.T) {
	fixture := newUnmountFixture(t)
	fixture.linux.BusyProcesses = []sharemounttest.BusyProcess{
		{ProcessID: "1234", User: "worker", Command: "python"},
	}
	_, summary, target := startScopeRemovalPreview(t, fixture)
	confirmed := fixture.handler.do(http.MethodPost, "/ui/secrets/authorization/confirm", url.Values{
		"csrf_token": {csrfToken(t, fixture.handler)}, "summary": {summary},
		"return_to": {"/ui/shares/" + fixture.shareID}, "target": target,
		"unmount_server": {fixture.linuxID},
	})
	batchPath := confirmed.Header.Get("Location")
	batchPage := waitUntilUnmountBatchDone(t, fixture.handler, batchPath)
	for _, want := range []string{"占用", "1234", "worker", "python"} {
		if !strings.Contains(batchPage, want) {
			t.Fatalf("联动卸载被占用时汇总页应展示占用信息 %q：\n%s", want, batchPage)
		}
	}

	taskID := queryParam(t, batchPath, "task")
	taskPage := body(t, fixture.handler.do(http.MethodGet,
		"/ui/shares/"+fixture.shareID+"/unmount?server="+fixture.linuxID+"&task="+taskID, nil))
	for _, want := range []string{"占用挂载点的进程", "1234", "worker", "python", "强制卸载", "稍后再试"} {
		if !strings.Contains(taskPage, want) {
			t.Fatalf("范围外联动卸载任务页缺少 %q：\n%s", want, taskPage)
		}
	}

	fixture.linux.ForceSucceeds = true
	force := fixture.handler.do(http.MethodPost, "/ui/shares/"+fixture.shareID+"/unmount", url.Values{
		"csrf_token": {csrfToken(t, fixture.handler)},
		"server_id":  {fixture.linuxID},
		"task":       {taskID},
		"force":      {"1"},
	})
	if force.StatusCode != http.StatusSeeOther {
		t.Fatalf("范围外强制卸载状态码 = %d：%s", force.StatusCode, body(t, force))
	}
	forcePage := waitUntilUnmountDone(t, fixture.handler, force.Header.Get("Location"))
	if !strings.Contains(forcePage, "卸载完成") || !strings.Contains(forcePage, "强制执行") {
		t.Fatalf("范围外强制卸载结果页：\n%s", forcePage)
	}
	if fixture.linux.State().Mounted {
		t.Fatalf("强制卸载后挂载应被移除：%+v", fixture.linux.State())
	}
}

// TestAuthorityShareScopeConfirmReverifyPreservesUnmountChoice 覆盖权威端
// 五分钟重新验证：跳转回来后「同时卸载」勾选保留，确认后仍会启动卸载。
func TestAuthorityShareScopeConfirmReverifyPreservesUnmountChoice(t *testing.T) {
	fixture := newUnmountStoreFixture(t)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.Local)
	h := newShareAuthorityHandler(t, fixture.store, &now, func(config *webui.Config) {
		remoteTasks := RemoteTasksFromStore(openStoreForTest(fixture.store))
		config.SharesRemoteTasks = remoteTasks
		config.ShareLatestChecks = remoteTasks
		config.ShareUnmountRunner = UnmountTasksFromStore(openStoreForTest(fixture.store))
	})
	now = now.Add(6 * time.Minute)

	started := h.do(http.MethodPost, "/ui/shares/"+fixture.shareID+"/check", url.Values{
		"csrf_token": {authorityPageCSRF(t, h, "/ui/shares/"+fixture.shareID)},
	})
	if started.StatusCode != http.StatusSeeOther {
		t.Fatalf("权威端启动现场探测状态码 = %d：%s", started.StatusCode, body(t, started))
	}
	waitAuthorityRemoteTaskDone(t, h, started.Header.Get("Location"))
	views, err := fixture.store.ListSecretsInScope(store.ScopeAllServers())
	if err != nil || len(views) != 1 {
		t.Fatalf("读取挂载密码机密项失败：%v %+v", err, views)
	}
	secretID := views[0].ID
	target := []string{
		secretID + " " + fixture.linuxID + " 0",
		secretID + " " + fixture.windowsID + " 1",
	}
	preview := h.do(http.MethodPost, "/ui/secrets/authorization/preview", url.Values{
		"csrf_token": {authorityPageCSRF(t, h, "/ui/shares/"+fixture.shareID)},
		"return_to":  {"/ui/shares/" + fixture.shareID},
		"secret_id":  {secretID},
		"server_id":  {fixture.linuxID, fixture.windowsID},
		"cell__" + secretID + "__" + fixture.linuxID:   {"0"},
		"cell__" + secretID + "__" + fixture.windowsID: {"1"},
	})
	previewBody := body(t, preview)
	if preview.StatusCode != http.StatusOK {
		t.Fatalf("权威端可挂载范围预览状态码 = %d：%s", preview.StatusCode, previewBody)
	}
	summary := authorityHiddenValue(t, previewBody, "summary")

	rejected := h.do(http.MethodPost, "/ui/secrets/authorization/confirm", url.Values{
		"csrf_token": {authorityPageCSRF(t, h, "/ui/shares/"+fixture.shareID)},
		"summary":    {summary},
		"return_to":  {"/ui/shares/" + fixture.shareID}, "target": target,
		"unmount_server": {fixture.linuxID},
	})
	if rejected.StatusCode != http.StatusSeeOther ||
		!strings.HasPrefix(rejected.Header.Get("Location"), "/ui/reverify?next=") {
		t.Fatalf("权威端确认应先重新验证，状态码=%d 跳转=%q：%s",
			rejected.StatusCode, rejected.Header.Get("Location"), body(t, rejected))
	}
	resume := queryParam(t, rejected.Header.Get("Location"), "next")
	if !strings.Contains(resume, "unmount="+fixture.linuxID) {
		t.Fatalf("重新验证回跳地址应保留同时卸载勾选：%s", resume)
	}
	if !strings.Contains(resume, "target="+url.QueryEscape(target[0])) {
		t.Fatalf("重新验证回跳地址应保留目标授权格：%s", resume)
	}
	reverify := h.do(http.MethodPost, "/ui/reverify", url.Values{
		"csrf_token": {authorityPageCSRF(t, h, "/ui/reverify?next="+url.QueryEscape(resume))},
		"password":   {"correct-password"}, "next": {resume},
	})
	if reverify.StatusCode != http.StatusSeeOther || reverify.Header.Get("Location") != resume {
		t.Fatalf("重新验证后应回到保留勾选的预览，状态码=%d 跳转=%q：%s",
			reverify.StatusCode, reverify.Header.Get("Location"), body(t, reverify))
	}
	resumed := body(t, h.do(http.MethodGet, resume, nil))
	if !strings.Contains(resumed, `name="unmount_server" value="`+fixture.linuxID+`" checked`) {
		t.Fatalf("重新验证后的预览应保留同时卸载勾选：\n%s", resumed)
	}

	confirmed := h.do(http.MethodPost, "/ui/secrets/authorization/confirm", url.Values{
		"csrf_token": {authorityPageCSRF(t, h, "/ui/shares/"+fixture.shareID)},
		"summary":    {summary},
		"return_to":  {"/ui/shares/" + fixture.shareID}, "target": target,
		"unmount_server": {fixture.linuxID},
	})
	if confirmed.StatusCode != http.StatusSeeOther ||
		!strings.HasPrefix(confirmed.Header.Get("Location"), "/ui/shares/"+fixture.shareID+"/unmount-batch?task=") {
		t.Fatalf("重新验证后确认应启动卸载任务，状态码=%d 跳转=%q：%s",
			confirmed.StatusCode, confirmed.Header.Get("Location"), body(t, confirmed))
	}
}

// waitUntilUnmountBatchDone 轮询卸载任务汇总页直到没有进行中的任务。
func waitUntilUnmountBatchDone(t *testing.T, h sessionHandler, batchPath string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp := h.do(http.MethodGet, batchPath, nil)
		page := body(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("卸载任务汇总页状态码 = %d：\n%s", resp.StatusCode, page)
		}
		if !strings.Contains(page, "卸载进行中") {
			return page
		}
		if time.Now().After(deadline) {
			t.Fatalf("卸载任务汇总页超时未完成：\n%s", page)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitAuthorityRemoteTaskDone 轮询权威端的现场探测任务页直到完成。
func waitAuthorityRemoteTaskDone(t *testing.T, h authorityHandler, taskPath string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		page := body(t, h.do(http.MethodGet, taskPath, nil))
		if strings.Contains(page, "现场探测完成") || strings.Contains(page, "现场探测失败") {
			return page
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("权威端现场探测任务未及时完成：%s", taskPath)
	return ""
}

// queryParam 提取跳转地址里的一个查询参数。
func queryParam(t *testing.T, location, name string) string {
	t.Helper()
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Query().Get(name)
}
