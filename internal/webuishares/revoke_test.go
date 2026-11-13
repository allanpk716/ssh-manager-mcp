package webuishares

import (
	"net/http"
	"strings"
	"testing"
)

func TestShareRevokeConfirmationExplainsRegistrationAndRemoteCleanup(t *testing.T) {
	fixture := newUnmountFixture(t)

	detail := body(t, fixture.handler.do(http.MethodGet, "/ui/shares/"+fixture.shareID, nil))
	for _, want := range []string{
		"撤销授权", `href="`+fixture.revokePath(fixture.linuxID)+`"`,
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

func (f *unmountFixture) revokePath(serverID string) string {
	return "/ui/shares/" + f.shareID + "/revoke?server=" + serverID
}

