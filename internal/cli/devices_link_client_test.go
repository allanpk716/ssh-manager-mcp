package cli

import (
	"strings"
	"testing"
)

func TestDevicesLinkClient(t *testing.T) {
	dbPath, masterKey := withCliStoreEnv(t)
	seedTopologyClient(t, dbPath, masterKey, "laptop-client")
	runCli(t, "devices", "add", "工作站")

	out := runCli(t, "devices", "link-client", "工作站", "laptop-client")
	if !strings.Contains(out, "已把客户端实例 laptop-client 关联到机器 工作站") {
		t.Fatalf("link-client 输出不正确：\n%s", out)
	}
	show := runCli(t, "devices", "show", "工作站")
	if !strings.Contains(show, "laptop-client") {
		t.Fatalf("关联后设备详情应显示客户端实例：\n%s", show)
	}
}
