package cli

import (
	"strings"
	"testing"
)

// TestServersAddRequiresHostWithoutDeviceMemberships 钉住命令行兜底主机约
// 束：没有设备、或指定设备没有任何网络成员时，都必须显式提供 --host。
func TestServersAddRequiresHostWithoutDeviceMemberships(t *testing.T) {
	withCliStoreEnv(t)

	withoutDevice := runCliErr(t, "servers", "add", "--name", "web", "--user", "deploy")
	if !strings.Contains(withoutDevice, "必须提供 --host，或提供 --device 并确保该设备已有网络成员") {
		t.Fatalf("既没有兜底主机也没有设备时应给出清晰错误：%s", withoutDevice)
	}

	runCli(t, "devices", "add", "暂不归类设备")
	withoutMembership := runCliErr(t,
		"servers", "add", "--name", "web", "--user", "deploy", "--device", "暂不归类设备")
	if !strings.Contains(withoutMembership, "指定已有设备但没有网络成员时，必须提供兜底主机") {
		t.Fatalf("指定设备没有网络成员时应要求兜底主机：%s", withoutMembership)
	}
}
