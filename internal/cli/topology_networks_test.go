package cli

// `sshmgr networks` 命令组的行为测试。用例从根命令进入，覆盖网络类型与
// 站点归属约束、部分更新、筛选、删除确认，以及站点列表的网络分组。

import (
	"bytes"
	"strings"
	"testing"
)

// seedTopologySites 登记两个站点，供网络归属与筛选用例使用。
func seedTopologySites(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		runCli(t, "sites", "add", name)
	}
}

// TestNetworksRegistrationAndFilters 覆盖新增约束、重复名字、相同网段
// 不拦截、部分更新、类型不可改与列表筛选。
func TestNetworksRegistrationAndFilters(t *testing.T) {
	withCliStoreEnv(t)
	seedTopologySites(t, "家A", "公司A")

	errText := runCliErr(t, "networks", "add", "家A局域网", "--type", "physical", "--cidr", "192.168.1.0/24")
	if !strings.Contains(errText, "物理局域网必须归属一个站点") {
		t.Fatalf("physical network without a site must explain the reason, got:\n%s", errText)
	}
	errText = runCliErr(t, "networks", "add", "虚拟网", "--type", "virtual", "--site", "家A")
	if !strings.Contains(errText, "虚拟局域网不属于站点") {
		t.Fatalf("virtual network with a site must explain the reason, got:\n%s", errText)
	}
	errText = runCliErr(t, "networks", "add", "家A局域网", "--type", "unknown")
	if !strings.Contains(errText, "unknown") || !strings.Contains(errText, "physical") || !strings.Contains(errText, "virtual") {
		t.Fatalf("unknown network type must list valid values, got:\n%s", errText)
	}

	runCli(t, "networks", "add", "家A局域网", "--type", "physical", "--site", "家A",
		"--note", "家里的千兆网", "--cidr", "192.168.1.0/24",
		"--bandwidth-mbps", "1000", "--metered")
	runCli(t, "networks", "add", "虚拟网", "--type", "virtual", "--note", "跨站点虚拟网")
	runCli(t, "networks", "add", "公司局域网", "--type", "physical", "--site", "公司A",
		"--cidr", "192.168.1.0/24", "--bandwidth-mbps", "500")

	errText = runCliErr(t, "networks", "add", "家A局域网", "--type", "physical", "--site", "家A")
	if !strings.Contains(errText, "网络名字已存在") {
		t.Fatalf("duplicate network name must be refused, got:\n%s", errText)
	}

	out := runCli(t, "networks", "list")
	for _, want := range []string{"家A局域网", "物理局域网", "家A", "192.168.1.0/24", "1000 Mbit/s", "按流量计费", "虚拟网", "虚拟局域网", "公司局域网"} {
		if !strings.Contains(out, want) {
			t.Fatalf("network list must contain %q:\n%s", want, out)
		}
	}

	out = runCli(t, "networks", "list", "--site", "家A")
	if !strings.Contains(out, "家A局域网") || strings.Contains(out, "公司局域网") || strings.Contains(out, "虚拟网") {
		t.Fatalf("site filter must keep only networks in that site:\n%s", out)
	}
	out = runCli(t, "networks", "list", "--type", "virtual")
	if !strings.Contains(out, "虚拟网") || strings.Contains(out, "家A局域网") || strings.Contains(out, "公司局域网") {
		t.Fatalf("type filter must keep only virtual networks:\n%s", out)
	}
	out = runCli(t, "networks", "list", "--site", "公司A", "--type", "physical")
	if !strings.Contains(out, "公司局域网") || strings.Contains(out, "家A局域网") {
		t.Fatalf("combined filters must keep only the matching network:\n%s", out)
	}

	errText = runCliErr(t, "networks", "edit", "家A局域网", "--type", "virtual")
	if !strings.Contains(errText, "网络类型登记后不能修改") || !strings.Contains(errText, "删除后重建") {
		t.Fatalf("editing network type must explain the immutable rule, got:\n%s", errText)
	}
	runCli(t, "networks", "edit", "家A局域网", "--note", "家A新备注", "--metered=false")
	out = runCli(t, "networks", "list", "--site", "家A")
	if !strings.Contains(out, "家A新备注") || strings.Contains(out, "按流量计费") || !strings.Contains(out, "192.168.1.0/24") || !strings.Contains(out, "1000 Mbit/s") {
		t.Fatalf("partial edit must only change explicitly passed fields:\n%s", out)
	}
	runCli(t, "networks", "edit", "家A局域网", "--name", "家A主网", "--site", "公司A",
		"--cidr", "10.0.0.0/8", "--bandwidth-mbps", "2000")
	out = runCli(t, "networks", "list", "--site", "公司A", "--type", "physical")
	if !strings.Contains(out, "家A主网") || !strings.Contains(out, "10.0.0.0/8") || !strings.Contains(out, "2000 Mbit/s") {
		t.Fatalf("full edit must apply new values:\n%s", out)
	}

	out = runCli(t, "sites", "list")
	if !strings.Contains(out, "家A主网") || !strings.Contains(out, "公司局域网") || !strings.Contains(out, "站点 家A") {
		t.Fatalf("sites list must group physical networks under sites:\n%s", out)
	}
	if strings.Contains(out, "虚拟网") {
		t.Fatalf("sites list must not attach virtual networks to a site:\n%s", out)
	}
}

// TestTopologyDeleteGuards 覆盖非空站点删除被拒并列出网络、空站点可删，
// 以及网络删除的交互终端守卫与跳过参数。
func TestTopologyDeleteGuards(t *testing.T) {
	withCliStoreEnv(t)
	seedTopologySites(t, "家A", "公司A")
	runCli(t, "networks", "add", "家A局域网", "--type", "physical", "--site", "家A")

	errText := runCliErr(t, "sites", "rm", "家A")
	if !strings.Contains(errText, "站点 家A 下还有物理局域网") || !strings.Contains(errText, "家A局域网") {
		t.Fatalf("non-empty site deletion must list blocking networks, got:\n%s", errText)
	}
	runCli(t, "sites", "rm", "公司A")

	oldIsInteractive := topologyStdinIsInteractive
	topologyStdinIsInteractive = func() bool { return false }
	t.Cleanup(func() { topologyStdinIsInteractive = oldIsInteractive })
	errText = runCliErr(t, "networks", "rm", "家A局域网")
	if !strings.Contains(errText, "非交互终端") || !strings.Contains(errText, "--yes") {
		t.Fatalf("network deletion without an interactive terminal must require --yes, got:\n%s", errText)
	}
	out := runCli(t, "networks", "list")
	if !strings.Contains(out, "家A局域网") {
		t.Fatalf("refused network deletion must keep the network:\n%s", out)
	}
	out = runCli(t, "networks", "rm", "家A局域网", "--yes")
	if !strings.Contains(out, "已删除网络 家A局域网") {
		t.Fatalf("--yes must delete the network:\n%s", out)
	}
	out = runCli(t, "sites", "rm", "家A")
	if !strings.Contains(out, "已删除站点 家A") {
		t.Fatalf("site with its network removed must be deletable:\n%s", out)
	}
}

// TestNetworkRemoveInteractiveConfirm 覆盖交互终端上的确认流程：输入 n 取
// 消且不删除，输入 y 才执行。
func TestNetworkRemoveInteractiveConfirm(t *testing.T) {
	withCliStoreEnv(t)
	seedTopologySites(t, "家A")
	runCli(t, "networks", "add", "家A局域网", "--type", "physical", "--site", "家A")

	oldIsInteractive := topologyStdinIsInteractive
	topologyStdinIsInteractive = func() bool { return true }
	t.Cleanup(func() { topologyStdinIsInteractive = oldIsInteractive })

	runTopologyRootWithStdin(t, "n\n", "networks", "rm", "家A局域网")
	if out := runCli(t, "networks", "list"); !strings.Contains(out, "家A局域网") {
		t.Fatalf("answering n must cancel deletion:\n%s", out)
	}
	out := runTopologyRootWithStdin(t, "y\n", "networks", "rm", "家A局域网")
	if !strings.Contains(out, "已删除网络 家A局域网") {
		t.Fatalf("answering y must delete the network:\n%s", out)
	}
}

// TestNetworksRemovePreviewsAndCascadesMembers 覆盖删除网络前列出网络成员，
// 确认后网络与这些成员一起删除。
func TestNetworksRemovePreviewsAndCascadesMembers(t *testing.T) {
	withCliStoreEnv(t)
	seedTopologySites(t, "家A")
	runCli(t, "networks", "add", "家A局域网", "--type", "physical", "--site", "家A")
	runCli(t, "devices", "add", "工作站")
	runCli(t, "devices", "add", "NAS")
	runCli(t, "devices", "member", "add", "工作站", "--network", "家A局域网", "--address", "192.168.1.20")
	runCli(t, "devices", "member", "add", "NAS", "--network", "家A局域网", "--address", "192.168.1.30")

	oldIsInteractive := topologyStdinIsInteractive
	topologyStdinIsInteractive = func() bool { return true }
	t.Cleanup(func() { topologyStdinIsInteractive = oldIsInteractive })
	out := runTopologyRootWithStdin(t, "n\n", "networks", "rm", "家A局域网")
	for _, want := range []string{
		"将删除网络 家A局域网", "工作站 — 192.168.1.20", "NAS — 192.168.1.30", "已取消，未删除网络",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("network removal preview must contain %q:\n%s", want, out)
		}
	}
	if out := runCli(t, "devices", "show", "工作站"); !strings.Contains(out, "192.168.1.20") {
		t.Fatalf("cancelled network removal must keep memberships:\n%s", out)
	}

	out = runTopologyRootWithStdin(t, "y\n", "networks", "rm", "家A局域网")
	if !strings.Contains(out, "已删除网络 家A局域网") {
		t.Fatalf("answering y must delete the network:\n%s", out)
	}
	if out := runCli(t, "networks", "list", "--site", "家A"); strings.Contains(out, "家A局域网") {
		t.Fatalf("network must be deleted, got:\n%s", out)
	}
	for device, address := range map[string]string{"工作站": "192.168.1.20", "NAS": "192.168.1.30"} {
		out := runCli(t, "devices", "show", device)
		if strings.Contains(out, address) || !strings.Contains(out, "网络成员：无") {
			t.Fatalf("deleting network must remove %s's membership:\n%s", device, out)
		}
	}
}

// TestNetworkRemoveHintsServersWithOnlyFallbackAddress 覆盖删除网络后的逐
// 条提示：所属设备因此没有网络成员的服务器条目只剩兜底 Host；还有其他
// 网络成员的条目不提示。
func TestNetworkRemoveHintsServersWithOnlyFallbackAddress(t *testing.T) {
	withCliStoreEnv(t)
	seedTopologySites(t, "家A")
	runCli(t, "networks", "add", "家A局域网", "--type", "physical", "--site", "家A")
	runCli(t, "networks", "add", "虚拟网", "--type", "virtual")
	runCli(t, "servers", "add", "--name", "web", "--host", "192.0.2.10", "--user", "root")
	runCli(t, "servers", "add", "--name", "db", "--host", "192.0.2.20", "--user", "root")
	runCli(t, "devices", "member", "add", "web", "--network", "家A局域网", "--address", "192.168.1.10")
	runCli(t, "devices", "member", "add", "db", "--network", "家A局域网", "--address", "192.168.1.20")
	runCli(t, "devices", "member", "add", "db", "--network", "虚拟网", "--address", "100.64.0.20")

	out := runCli(t, "networks", "rm", "家A局域网", "--yes")
	for _, want := range []string{"将删除网络 家A局域网", "网络成员：", "web — 192.168.1.10", "db — 192.168.1.20"} {
		if !strings.Contains(out, want) {
			t.Fatalf("network removal with --yes must still print the member preview, missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "服务器条目 web：候选地址只剩兜底 Host") {
		t.Fatalf("network removal must warn each entry whose device lost its last membership:\n%s", out)
	}
	if strings.Contains(out, "服务器条目 db：") {
		t.Fatalf("entries whose device keeps another membership must not be warned:\n%s", out)
	}
}

// runTopologyRootWithStdin 从根命令执行命令并喂入标准输入，返回标准输出。
func runTopologyRootWithStdin(t *testing.T, stdin string, args ...string) string {
	t.Helper()
	root := NewRootCmd()
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	if err := root.Execute(); err != nil {
		t.Fatalf("cli %v: %v\n%s", args, err, out.String())
	}
	return out.String()
}
