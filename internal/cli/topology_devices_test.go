package cli

// `sshmgr devices` 命令组的行为测试。所有用例都从根命令进入，先钉住设备
// 登记与客户端实例一对一关联，再进入网络成员与筛选。

import (
	"strings"
	"testing"

	"ssh-manager-mcp/internal/store"
)

// seedTopologyClient 登记一个客户端实例，返回它的设备码名字。
func seedTopologyClient(t *testing.T, dbPath string, masterKey []byte, name string) {
	t.Helper()
	st, err := store.Open(dbPath, masterKey)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	profileID := ""
	profiles, err := st.ListProfiles()
	if err != nil {
		t.Fatalf("list profiles: %v", err)
	}
	for _, profile := range profiles {
		if profile.Name == "拓扑测试轮廓" {
			profileID = profile.ID
			break
		}
	}
	if profileID == "" {
		profileID, err = st.AddProfile("拓扑测试轮廓")
		if err != nil {
			t.Fatalf("add profile: %v", err)
		}
	}
	if _, _, err := st.AddCacheToken(name, profileID); err != nil {
		t.Fatalf("add client %s: %v", name, err)
	}
}

// TestDevicesRegistrationClientAndShow 覆盖设备登记、大小写不同的名字拒绝、
// 部分修改、客户端实例一对一关联、解除关联与详情输出。
func TestDevicesRegistrationClientAndShow(t *testing.T) {
	dbPath, masterKey := withCliStoreEnv(t)
	seedTopologyClient(t, dbPath, masterKey, "laptop")
	seedTopologyClient(t, dbPath, masterKey, "desktop")

	runCli(t, "devices", "add", "工作站", "--note", "家里的主力机器", "--client", "laptop")
	runCli(t, "devices", "add", "NAS")

	errText := runCliErr(t, "devices", "add", "另一台设备", "--client", "laptop")
	if !strings.Contains(errText, "laptop") || !strings.Contains(errText, "已关联设备 工作站") {
		t.Fatalf("adding a second device for one client must be refused with both names, got:\n%s", errText)
	}

	errText = runCliErr(t, "devices", "add", "nas")
	if !strings.Contains(errText, "设备名字已存在") {
		t.Fatalf("case-insensitive duplicate device name must be refused, got:\n%s", errText)
	}

	out := runCli(t, "devices", "show", "工作站")
	for _, want := range []string{"设备 工作站", "家里的主力机器", "客户端实例：laptop", "网络成员：无"} {
		if !strings.Contains(out, want) {
			t.Fatalf("device show must contain %q:\n%s", want, out)
		}
	}

	runCli(t, "devices", "edit", "工作站", "--name", "家A工作站", "--note", "新备注", "--no-client")
	out = runCli(t, "devices", "show", "家A工作站")
	if !strings.Contains(out, "新备注") || !strings.Contains(out, "客户端实例：无") || strings.Contains(out, "laptop") {
		t.Fatalf("device edit must apply fields and clear the client:\n%s", out)
	}

	runCli(t, "devices", "edit", "家A工作站", "--client", "desktop")
	errText = runCliErr(t, "devices", "edit", "NAS", "--client", "desktop")
	if !strings.Contains(errText, "desktop") || !strings.Contains(errText, "已关联设备") {
		t.Fatalf("a client must not link to two devices, got:\n%s", errText)
	}
	errText = runCliErr(t, "devices", "edit", "NAS", "--client", "missing")
	if !strings.Contains(errText, "客户端实例不存在") || !strings.Contains(errText, "missing") {
		t.Fatalf("unknown client must be refused by name, got:\n%s", errText)
	}
}

// TestDevicesNetworkMembers 覆盖网络成员的新增、地址唯一性、同一设备重复
// 加入同一网络被拒、地址修改、删除与详情输出。
func TestDevicesNetworkMembers(t *testing.T) {
	withCliStoreEnv(t)
	seedTopologySites(t, "家A")
	runCli(t, "networks", "add", "家A局域网", "--type", "physical", "--site", "家A")
	runCli(t, "networks", "add", "虚拟网", "--type", "virtual")
	runCli(t, "devices", "add", "工作站")
	runCli(t, "devices", "add", "NAS")

	runCli(t, "devices", "member", "add", "工作站", "--network", "家A局域网", "--address", " 192.168.1.20 ")
	runCli(t, "devices", "member", "add", "工作站", "--network", "虚拟网", "--address", "Workstation.Example")

	errText := runCliErr(t, "devices", "member", "add", "NAS", "--network", "家A局域网", "--address", "192.168.1.20")
	if !strings.Contains(errText, "已被设备 工作站 占用") || !strings.Contains(errText, "192.168.1.20") {
		t.Fatalf("duplicate address in one network must name the occupying device, got:\n%s", errText)
	}
	errText = runCliErr(t, "devices", "member", "add", "NAS", "--network", "虚拟网", "--address", "workstation.example")
	if !strings.Contains(errText, "已被设备 工作站 占用") {
		t.Fatalf("case-insensitive domain comparison must refuse the duplicate, got:\n%s", errText)
	}
	errText = runCliErr(t, "devices", "member", "add", "工作站", "--network", "家A局域网", "--address", "192.168.1.30")
	if !strings.Contains(errText, "已有网络成员") || !strings.Contains(errText, "devices member edit") {
		t.Fatalf("duplicate device membership must be refused, got:\n%s", errText)
	}
	runCli(t, "devices", "member", "add", "NAS", "--network", "虚拟网", "--address", "192.168.1.20")

	out := runCli(t, "devices", "show", "工作站")
	for _, want := range []string{
		"家A局域网（物理局域网，站点 家A）：192.168.1.20",
		"虚拟网（虚拟局域网）：Workstation.Example",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("device show must list membership %q:\n%s", want, out)
		}
	}

	runCli(t, "devices", "member", "edit", "工作站", "--network", "家A局域网", "--address", " 192.168.1.21 ")
	out = runCli(t, "devices", "show", "工作站")
	if !strings.Contains(out, "192.168.1.21") || strings.Contains(out, "192.168.1.20\n") {
		t.Fatalf("membership edit must replace the address after trimming:\n%s", out)
	}
	errText = runCliErr(t, "devices", "member", "edit", "NAS", "--network", "虚拟网", "--address", "workstation.example")
	if !strings.Contains(errText, "已被设备 工作站 占用") {
		t.Fatalf("membership edit must enforce address uniqueness, got:\n%s", errText)
	}

	runCli(t, "devices", "member", "rm", "工作站", "--network", "虚拟网")
	out = runCli(t, "devices", "show", "工作站")
	if strings.Contains(out, "Workstation.Example") || !strings.Contains(out, "家A局域网") {
		t.Fatalf("membership removal must remove only the named membership:\n%s", out)
	}
}

// TestDevicesListFilters 覆盖未归类、站点与网络筛选；站点只由设备所在的
// 物理局域网推出，虚拟局域网不算站点归属。
func TestDevicesListFilters(t *testing.T) {
	withCliStoreEnv(t)
	seedTopologySites(t, "家A", "公司A")
	runCli(t, "networks", "add", "家A局域网", "--type", "physical", "--site", "家A")
	runCli(t, "networks", "add", "公司局域网", "--type", "physical", "--site", "公司A")
	runCli(t, "networks", "add", "虚拟网", "--type", "virtual")
	for _, device := range []string{"工作站", "NAS", "公司机器", "未归类设备"} {
		runCli(t, "devices", "add", device)
	}
	runCli(t, "devices", "member", "add", "工作站", "--network", "家A局域网", "--address", "192.168.1.20")
	runCli(t, "devices", "member", "add", "工作站", "--network", "虚拟网", "--address", "100.64.0.5")
	runCli(t, "devices", "member", "add", "NAS", "--network", "家A局域网", "--address", "192.168.1.30")
	runCli(t, "devices", "member", "add", "公司机器", "--network", "公司局域网", "--address", "10.0.0.10")

	out := runCli(t, "devices", "list", "--unassigned")
	if !strings.Contains(out, "未归类设备") || strings.Contains(out, "工作站") ||
		strings.Contains(out, "NAS") || strings.Contains(out, "公司机器") {
		t.Fatalf("unassigned filter must return only devices without memberships:\n%s", out)
	}
	out = runCli(t, "devices", "list", "--site", "家A")
	if !strings.Contains(out, "工作站") || !strings.Contains(out, "NAS") ||
		strings.Contains(out, "公司机器") || strings.Contains(out, "未归类设备") {
		t.Fatalf("site filter must return devices in that site's physical networks:\n%s", out)
	}
	out = runCli(t, "devices", "list", "--network", "虚拟网")
	if !strings.Contains(out, "工作站") || strings.Contains(out, "NAS") ||
		strings.Contains(out, "公司机器") || strings.Contains(out, "未归类设备") {
		t.Fatalf("network filter must return only devices in that network:\n%s", out)
	}
	out = runCli(t, "devices", "list", "--network", "家A局域网")
	if !strings.Contains(out, "工作站") || !strings.Contains(out, "NAS") || strings.Contains(out, "公司机器") {
		t.Fatalf("physical network filter must return its member devices:\n%s", out)
	}
}

// TestDevicesRemoveCleansCurrentRelations 覆盖设备删除确认、删除网络成员、
// 解除客户端实例关联；本票设备还不能关联服务器条目。
func TestDevicesRemoveCleansCurrentRelations(t *testing.T) {
	dbPath, masterKey := withCliStoreEnv(t)
	seedTopologyClient(t, dbPath, masterKey, "laptop")
	seedTopologySites(t, "家A")
	runCli(t, "networks", "add", "家A局域网", "--type", "physical", "--site", "家A")
	runCli(t, "networks", "add", "虚拟网", "--type", "virtual")
	runCli(t, "devices", "add", "工作站", "--client", "laptop")
	runCli(t, "devices", "add", "NAS")
	runCli(t, "devices", "member", "add", "工作站", "--network", "家A局域网", "--address", "192.168.1.20")
	runCli(t, "devices", "member", "add", "工作站", "--network", "虚拟网", "--address", "100.64.0.5")
	runCli(t, "devices", "member", "add", "NAS", "--network", "家A局域网", "--address", "192.168.1.30")

	oldIsInteractive := topologyStdinIsInteractive
	topologyStdinIsInteractive = func() bool { return false }
	t.Cleanup(func() { topologyStdinIsInteractive = oldIsInteractive })
	errText := runCliErr(t, "devices", "rm", "工作站")
	if !strings.Contains(errText, "请加 --yes") {
		t.Fatalf("non-interactive device removal must require --yes, got:\n%s", errText)
	}
	if out := runCli(t, "devices", "show", "工作站"); !strings.Contains(out, "设备 工作站") {
		t.Fatalf("refused removal must keep the device:\n%s", out)
	}

	topologyStdinIsInteractive = func() bool { return true }
	out := runTopologyRootWithStdin(t, "n\n", "devices", "rm", "工作站")
	if !strings.Contains(out, "已取消，未删除设备") || !strings.Contains(out, "设备 工作站") {
		t.Fatalf("answering n must cancel device removal:\n%s", out)
	}
	out = runTopologyRootWithStdin(t, "y\n", "devices", "rm", "工作站")
	for _, want := range []string{
		"将删除设备 工作站", "家A局域网：192.168.1.20", "虚拟网：100.64.0.5",
		"会被解除关联的客户端实例：laptop", "已删除设备 工作站",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("device removal preview/result must contain %q:\n%s", want, out)
		}
	}
	errText = runCliErr(t, "devices", "show", "工作站")
	if !strings.Contains(errText, "设备不存在") {
		t.Fatalf("deleted device must no longer be showable, got:\n%s", errText)
	}
	if out := runCli(t, "devices", "show", "NAS"); !strings.Contains(out, "192.168.1.30") {
		t.Fatalf("other device memberships must survive:\n%s", out)
	}
	if out := runCli(t, "networks", "list"); !strings.Contains(out, "家A局域网") || !strings.Contains(out, "虚拟网") {
		t.Fatalf("networks must survive device removal:\n%s", out)
	}
}
