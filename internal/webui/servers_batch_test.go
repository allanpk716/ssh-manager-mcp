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
// 出按钮、全选当前筛选结果、取消、测试连接的禁用占位，以及收进「更
// 多 ⋯」弹层的三项既有批量授权。
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

	// 批量设置地点（#335）与批量测试连接（#336）都已接入：两个入口都
	// 必须是可用按钮。设置地点的可提交内容只出现在独立确认页，测试连接
	// 走内部接口；底部操作条仍不直接携带表单，写路由统一走防跨站请求伪
	// 造中间件。
	for _, attribute := range []string{"data-server-set-location-action", "data-server-test-connection-action"} {
		button := regexp.MustCompile(`<button[^>]*` + attribute + `[^>]*>`).FindString(page)
		if button == "" {
			t.Fatalf("缺少 %s 按钮：\n%s", attribute, page)
		}
		if !strings.Contains(button, `type="button"`) || strings.Contains(button, "disabled") ||
			strings.Contains(button, "尚未开放") {
			t.Fatalf("%s 必须是已启用的普通按钮，实际 %q", attribute, button)
		}
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
