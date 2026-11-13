package webuishares

import (
	"strings"
	"testing"

	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

// TestClientRoleRejectsShareWriteSourcesAndExecutors 验证客户端角色误配共
// 享存储写入来源或远端任务执行对象时，构造直接报错，写路由不会悄悄存在。
func TestClientRoleRejectsShareWriteSourcesAndExecutors(t *testing.T) {
	st := newIntegrationStore(t)
	cases := map[string]func(*webui.Config){
		"共享存储写入来源": func(config *webui.Config) {
			config.SharesWriter = WriterFromStore(openStoreForTest(st))
		},
		"现场探测任务": func(config *webui.Config) {
			config.SharesRemoteTasks = RemoteTasksFromStore(openStoreForTest(st))
		},
		"单台卸载任务": func(config *webui.Config) {
			config.ShareUnmountRunner = UnmountTasksFromStore(openStoreForTest(st))
		},
		"撤销授权任务": func(config *webui.Config) {
			config.ShareRevokeRunner = RevokeTasksFromStore(openStoreForTest(st))
		},
		"重选地址任务": func(config *webui.Config) {
			config.ShareReselectRunner = ReselectTasksFromStore(openStoreForTest(st))
		},
		"残留检查任务": func(config *webui.Config) {
			config.ShareResidueRunner = ResidueTasksFromStore(openStoreForTest(st))
		},
	}
	for name, setup := range cases {
		config := webui.Config{
			Role: webui.RoleClient, Port: 18086,
			Servers: clientServerSource{},
			Shares:  FromClientSnapshot(func(string) (*store.Snapshot, error) { return nil, nil }),
		}
		setup(&config)
		_, _, err := webui.New(config)
		if err == nil || !strings.Contains(err.Error(), "客户端") {
			t.Fatalf("%s：客户端误配执行对象应报错，实际：%v", name, err)
		}
	}
}
