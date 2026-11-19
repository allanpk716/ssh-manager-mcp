package cli

import (
	"strings"
	"testing"
)

// TestServersAddSupportsMultipleAddressesAndLocation 是 Web UI 分步新增的命
// 令行等价面：同一台机器可一次登记多个网络地址，并直接选择地点树。
func TestServersAddSupportsMultipleAddressesAndLocation(t *testing.T) {
	withCliStoreEnv(t)
	runCli(t, "sites", "add", "公司")
	runCli(t, "sites", "add", "一厂区", "--parent", "公司")
	runCli(t, "networks", "add", "办公网", "--type", "physical", "--site", "公司 / 一厂区")
	runCli(t, "networks", "add", "虚拟网", "--type", "virtual")

	out := runCli(t,
		"servers", "add", "--name", "multi", "--user", "deploy", "--port", "2222",
		"--site", "公司 / 一厂区",
		"--address", "办公网=10.0.0.21",
		"--address", "虚拟网=100.64.0.21",
	)
	if !strings.Contains(out, "added server multi") {
		t.Fatalf("新增应成功，输出：\n%s", out)
	}
	deviceOut := runCli(t, "devices", "show", "multi")
	for _, want := range []string{
		"地点：公司 / 一厂区",
		"办公网（物理局域网，地点 公司 / 一厂区）：10.0.0.21",
		"虚拟网（虚拟局域网）：100.64.0.21",
	} {
		if !strings.Contains(deviceOut, want) {
			t.Fatalf("机器详情缺少 %q：\n%s", want, deviceOut)
		}
	}
}
