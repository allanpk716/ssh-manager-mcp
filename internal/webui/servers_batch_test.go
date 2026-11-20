package webui

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
)

func newServerBatchTestHandler(t *testing.T, role Role) (http.Handler, *http.Cookie) {
	t.Helper()
	store := newStubServerStore()
	store.rows = []ServerListEntry{
		{ID: "srv-fine", Name: "fine", User: "root", Host: "192.0.2.10", Port: 22},
		{ID: "srv-bare", Name: "bare", User: "root", Host: "192.0.2.11", Port: 22},
	}
	handler, startup, err := New(Config{
		Role: role, Servers: store, ServerWriter: &stubServerWriter{},
		Topology: serverListTopologySource{}, Access: &stubAccessStore{},
		VersionHint: func() string { return "" }, Port: 18080,
	})
	if err != nil {
		t.Fatalf("构造 Web UI Handler 失败：%v", err)
	}
	return handler, exchangeStartupTokenForCookie(t, handler, startup)
}

// TestServerListBatchSelectionEntrances 校验可写角色的多选入口：进入与退
// 出按钮、全选当前筛选结果、取消、设置地点与测试连接的禁用占位，以及收
// 进「更多 ⋯」弹层的三项既有批量授权。
func TestServerListBatchSelectionEntrances(t *testing.T) {
	handler, cookie := newServerBatchTestHandler(t, RoleStandalone)
	page := serversGet(t, handler, cookie, "/ui/servers/")

	for _, want := range []string{
		`data-server-multi-select`, `aria-pressed="false"`, "多选",
		`data-server-select-all`, "全选当前筛选结果",
		`data-server-cancel-selection`, "取消",
		`data-server-set-location-action`, "设置地点",
		`data-server-test-connection-action`, "测试连接",
		`data-server-more-button`, "更多 ⋯",
		`data-server-more-drawer`, "更多批量操作",
		`data-batch-button="profiles"`, `data-batch-button="secrets"`, `data-batch-button="shares"`,
		"加入 profile", "授权机密项", "授权共享存储",
		`data-server-batch-drawer`,
		`value="srv-fine"`, `value="srv-bare"`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("可写角色的服务器列表缺少 %q：\n%s", want, page)
		}
	}

	// 设置地点仍属于后续票：入口只能是禁用的普通按钮。测试连接已由
	// #336 提供，必须是可用按钮；页面不提供可触发写入的表单字段，写路由
	// 仍统一走防跨站请求伪造中间件。
	for _, attribute := range []string{"data-server-set-location-action"} {
		button := regexp.MustCompile(`<button[^>]*` + attribute + `[^>]*>`).FindString(page)
		if button == "" {
			t.Fatalf("缺少 %s 按钮：\n%s", attribute, page)
		}
		if !strings.Contains(button, `type="button"`) || !strings.Contains(button, "disabled") {
			t.Fatalf("%s 必须是禁用的普通按钮，实际 %q", attribute, button)
		}
	}
	testButton := regexp.MustCompile(`<button[^>]*data-server-test-connection-action[^>]*>`).FindString(page)
	if testButton == "" {
		t.Fatalf("缺少测试连接按钮：\n%s", page)
	}
	if !strings.Contains(testButton, `type="button"`) || strings.Contains(testButton, "disabled") ||
		strings.Contains(testButton, "尚未开放") {
		t.Fatalf("测试连接必须是已启用的普通按钮，实际 %q", testButton)
	}
	barStart := strings.Index(page, "data-server-selection-bar")
	if barStart < 0 {
		t.Fatalf("缺少底部多选操作条：\n%s", page)
	}
	barEnd := strings.Index(page[barStart:], "</section>")
	barHTML := page[barStart : barStart+barEnd]
	for _, forbidden := range []string{"<form", `method="post"`, "name=\"set_location\"", "name=\"test_connection\""} {
		if strings.Contains(barHTML, forbidden) {
			t.Fatalf("多选操作条不应包含可提交内容 %q：\n%s", forbidden, barHTML)
		}
	}

	// 全选只作用于当前筛选结果：筛选后的页面只渲染匹配卡片的复选框。
	filtered := serversGet(t, handler, cookie, "/ui/servers/?query=fine")
	if strings.Contains(filtered, `value="srv-bare"`) || !strings.Contains(filtered, `value="srv-fine"`) {
		t.Fatalf("筛选后应只保留匹配卡片供全选：\n%s", filtered)
	}
	if !strings.Contains(filtered, `data-server-select-all`) {
		t.Fatalf("筛选后的页面仍应有全选当前筛选结果按钮：\n%s", filtered)
	}
}

// TestServerListClientRoleHasNoBatchSelectionEntrances 校验客户端只读角色
// 不出现多选入口、批量写路由与占位按钮。
func TestServerListClientRoleHasNoBatchSelectionEntrances(t *testing.T) {
	handler, cookie := newServerBatchTestHandler(t, RoleClient)
	page := serversGet(t, handler, cookie, "/ui/servers/")
	for _, forbidden := range []string{
		"data-server-multi-select", "data-server-select-all", "data-server-cancel-selection",
		"data-server-set-location-action", "data-server-test-connection-action",
		"data-server-more-button", "data-server-more-drawer", "data-server-batch-drawer",
		"server-select-control", "设置地点", "更多 ⋯",
	} {
		if strings.Contains(page, forbidden) {
			t.Fatalf("客户端只读列表不应出现多选入口 %q：\n%s", forbidden, page)
		}
	}
}
