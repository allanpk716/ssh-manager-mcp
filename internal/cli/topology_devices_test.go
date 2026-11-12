package cli

// `sshmgr devices` 命令组的行为测试。所有用例都从根命令进入，先钉住设备
// 登记与客户端实例一对一关联，再进入网络成员与筛选。

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/servermanage"
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

// seedTopologyServer 直接种一个还没有所属设备的服务器条目，用来测试
// devices move-server 的第一次关联。
func seedTopologyServer(t *testing.T, dbPath string, masterKey []byte, name string, port int) {
	t.Helper()
	st, err := store.Open(dbPath, masterKey)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	_, err = st.AddServerWithCredentials(&models.Server{
		Name: name, Host: "192.0.2.10", Port: port, User: "deploy",
	}, nil, nil)
	if err != nil {
		t.Fatalf("add server %s: %v", name, err)
	}
}

// TestDevicesMoveServerCoversAssociateAndMove 覆盖服务器条目第一次关联设
// 备、从一台设备移到另一台设备，以及设备详情中的条目清单；原设备变空也
// 保留。
func TestDevicesMoveServerCoversAssociateAndMove(t *testing.T) {
	dbPath, masterKey := withCliStoreEnv(t)
	seedTopologyServer(t, dbPath, masterKey, "deploy", 22)
	runCli(t, "devices", "add", "机器A")
	runCli(t, "devices", "add", "机器B")

	runCli(t, "devices", "move-server", "deploy", "--to", "机器A")
	out := runCli(t, "devices", "show", "机器A")
	if !strings.Contains(out, "服务器条目：deploy（用户 deploy，端口 22，未锚定）") {
		t.Fatalf("device show must list the associated server entry:\n%s", out)
	}

	runCli(t, "devices", "move-server", "deploy", "--to", "机器B")
	out = runCli(t, "devices", "show", "机器A")
	if !strings.Contains(out, "服务器条目：无") || strings.Contains(out, "deploy") {
		t.Fatalf("source device must stay but no longer list the moved entry:\n%s", out)
	}
	out = runCli(t, "devices", "show", "机器B")
	if !strings.Contains(out, "服务器条目：deploy（用户 deploy，端口 22，未锚定）") {
		t.Fatalf("target device show must list the moved server entry:\n%s", out)
	}
}

// TestOtherDeviceWebUIAndCLICreateSameTopologyData 用同一种数据形状分别走
// Web UI 背后的服务器管理模块与既有 devices 命令，证明不受管设备路径最终
// 写的是同一份设备与网络成员数据。
func TestOtherDeviceWebUIAndCLICreateSameTopologyData(t *testing.T) {
	dbPath, masterKey := withCliStoreEnv(t)
	st, err := store.Open(dbPath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSite("家A", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "家A局域网", Type: models.NetworkPhysical, SiteName: "家A"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "虚拟网", Type: models.NetworkVirtual}); err != nil {
		t.Fatal(err)
	}
	if _, err := servermanage.New(st).AddOtherDevice(servermanage.OtherDeviceAddRequest{
		DeviceName: "Web 网络存储", Note: "共享照片与备份", SiteName: "家A",
		Memberships: []servermanage.NetworkMembershipInput{
			{Network: "家A局域网", Address: "192.168.1.70"},
			{Network: "虚拟网", Address: "100.64.0.70"},
		},
	}, servermanage.WebUISource("192.0.2.7")); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	runCli(t, "devices", "add", "命令行网络存储", "--note", "共享照片与备份")
	runCli(t, "devices", "member", "add", "命令行网络存储", "--network", "家A局域网", "--address", "192.168.1.71")
	runCli(t, "devices", "member", "add", "命令行网络存储", "--network", "虚拟网", "--address", "100.64.0.71")

	out := runCli(t, "devices", "show", "Web 网络存储")
	for _, want := range []string{"备注：共享照片与备份", "家A局域网", "192.168.1.70", "虚拟网", "100.64.0.70"} {
		if !strings.Contains(out, want) {
			t.Fatalf("devices show 应显示 Web UI 保存的同一份数据，缺少 %q：\n%s", want, out)
		}
	}

	st, err = store.Open(dbPath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, name := range []string{"Web 网络存储", "命令行网络存储"} {
		device, err := st.GetDevice(name)
		if err != nil || device == nil || device.Note != "共享照片与备份" || device.ClientName != "" {
			t.Fatalf("两条入口应保存同样的设备字段：%v %+v", err, device)
		}
		memberships, err := st.ListNetworkMembershipsByRegistration(name)
		if err != nil || len(memberships) != 2 ||
			memberships[0].NetworkName != "家A局域网" || memberships[1].NetworkName != "虚拟网" {
			t.Fatalf("两条入口应保存同样的网络成员：%v %+v", err, memberships)
		}
	}
}

// TestServersAddCreatesOrUsesDevice 覆盖 servers add 的两种设备归属：不带
// --device 时生成同名设备，设备名冲突则加数字后缀；带 --device 时关联已有
// 设备。两种生成出来的设备都没有网络成员，因此出现在未归类列表。
func TestServersAddCreatesOrUsesDevice(t *testing.T) {
	withCliStoreEnv(t)
	runCli(t, "devices", "add", "deploy")
	runCli(t, "servers", "add", "--name", "deploy", "--host", "192.0.2.10", "--user", "deploy")
	runCli(t, "devices", "add", "NAS")
	runCli(t, "servers", "add", "--name", "api", "--host", "192.0.2.20", "--user", "root", "--device", "NAS")

	out := runCli(t, "devices", "show", "deploy-2")
	if !strings.Contains(out, "服务器条目：deploy（用户 deploy，端口 22，未锚定）") {
		t.Fatalf("server add without --device must create a same-named device with a numeric suffix:\n%s", out)
	}
	out = runCli(t, "devices", "show", "NAS")
	if !strings.Contains(out, "服务器条目：api（用户 root，端口 22，未锚定）") {
		t.Fatalf("server add with --device must associate the existing device:\n%s", out)
	}
	out = runCli(t, "devices", "list", "--unassigned")
	if !strings.Contains(out, "设备 deploy") || !strings.Contains(out, "设备 deploy-2") || !strings.Contains(out, "设备 NAS") {
		t.Fatalf("new devices have no network memberships and must appear as unassigned:\n%s", out)
	}
}

// TestServersAddUsesDevicePhysicalAddressAsFallback 覆盖命令行与表单一致的
// 兜底主机规则：指定已有设备时可以不给主机地址；有物理局域网成员则取第一
// 个物理地址，没有物理地址则保存为空。
func TestServersAddUsesDevicePhysicalAddressAsFallback(t *testing.T) {
	dbPath, masterKey := withCliStoreEnv(t)
	st, err := store.Open(dbPath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSite("家A", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "家A局域网", Type: models.NetworkPhysical, SiteName: "家A"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "虚拟网", Type: models.NetworkVirtual}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("工作站", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership("工作站", "家A局域网", "192.168.1.20"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership("工作站", "虚拟网", "100.64.0.20"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	runCli(t, "servers", "add", "--name", "web", "--user", "deploy", "--device", "工作站")
	st, err = store.Open(dbPath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv, err := st.GetServerByName("web")
	if err != nil || srv == nil {
		t.Fatalf("servers add 应保存服务器条目：%v %+v", err, srv)
	}
	if srv.Host != "192.168.1.20" {
		t.Fatalf("兜底主机应取第一个物理局域网地址：%+v", srv)
	}

	if _, err := st.RemoveNetworkMembership("工作站", "家A局域网"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	runCli(t, "servers", "add", "--name", "db", "--user", "deploy", "--device", "工作站")
	st, err = store.Open(dbPath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv, err = st.GetServerByName("db")
	if err != nil || srv == nil || srv.Host != "" {
		t.Fatalf("没有物理局域网地址时兜底主机应为空：%v %+v", err, srv)
	}
}

// TestServerDeviceAnchorCopySamePort 覆盖同一设备、同一端口的其他服务器条目的锚复制：新条目
// 尚无锚时复制既有锚，命令输出、锚来源信息与审计都写明来源条目。
func TestServerDeviceAnchorCopySamePort(t *testing.T) {
	dbPath, masterKey := withCliStoreEnv(t)
	_, fingerprint := genHostKey(t)
	runCli(t, "devices", "add", "机器")
	runCli(t, "servers", "add", "--name", "root", "--host", "192.0.2.10", "--user", "root", "--device", "机器")
	runCli(t, "servers", "pin-hostkey", "root", "--fingerprint", fingerprint)

	out := runCli(t, "servers", "add", "--name", "deploy", "--host", "192.0.2.10",
		"--user", "deploy", "--device", "机器")
	if !strings.Contains(out, "已从服务器条目 root 复制主机密钥锚") {
		t.Fatalf("server add must report the copied anchor and its source entry:\n%s", out)
	}
	out = runCli(t, "servers", "pin-hostkey", "deploy")
	if !strings.Contains(out, fingerprint) || !strings.Contains(out, "source=manual") ||
		!strings.Contains(out, "format=fingerprint") || !strings.Contains(out, "device=-") ||
		strings.Contains(out, "复制自条目") {
		t.Fatalf("copied anchor must keep the sibling pin metadata instead of free-text provenance:\n%s", out)
	}
	rows := topologyAuditRows(t, dbPath, masterKey, "pin-copy")
	if len(rows) != 1 {
		t.Fatalf("anchor copy must leave exactly one pin-copy audit row, got %d", len(rows))
	}
	if rows[0].ServerID == "" {
		t.Fatalf("anchor copy audit must name the target entry: %+v", rows[0])
	}
	summary := topologyAuditSummary(t, rows[0])
	wantSummary := map[string]any{
		"entry":       "deploy",
		"host":        "192.0.2.10:22",
		"fingerprint": fingerprint,
		"来源服务器条目":     "root",
	}
	if !reflect.DeepEqual(summary, wantSummary) {
		t.Fatalf("anchor copy audit summary = %#v, want %#v", summary, wantSummary)
	}
	if forwardRows := topologyAuditRows(t, dbPath, masterKey, "pin-forward"); len(forwardRows) != 0 {
		t.Fatalf("anchor copy must not use pin-forward audit action: %+v", forwardRows)
	}
}

// TestDevicesMoveServerCopiesAnchor 覆盖移动条目进入设备时的同端口锚复制，
// 并钉住 device.move-server 审计动作。
func TestDevicesMoveServerCopiesAnchor(t *testing.T) {
	dbPath, masterKey := withCliStoreEnv(t)
	_, fingerprint := genHostKey(t)
	runCli(t, "devices", "add", "机器")
	runCli(t, "servers", "add", "--name", "root", "--host", "192.0.2.10",
		"--user", "root", "--device", "机器")
	runCli(t, "servers", "pin-hostkey", "root", "--fingerprint", fingerprint)
	runCli(t, "servers", "add", "--name", "admin", "--host", "192.0.2.10", "--user", "admin")

	out := runCli(t, "devices", "move-server", "admin", "--to", "机器")
	if !strings.Contains(out, "已从服务器条目 root 复制主机密钥锚") {
		t.Fatalf("moving an unpinned entry must copy the sibling anchor:\n%s", out)
	}
	if out := runCli(t, "servers", "pin-hostkey", "admin"); !strings.Contains(out, fingerprint) {
		t.Fatalf("moved entry must carry the copied anchor:\n%s", out)
	}
	if rows := topologyAuditRows(t, dbPath, masterKey, "device.move-server"); len(rows) != 1 {
		t.Fatalf("moving a server entry must leave one device.move-server audit row, got %d", len(rows))
	}
}

// TestServerDeviceAnchorCopyConflict 覆盖同一设备、同一端口的多个服务器条目锚互不一致：
// 不复制、不落锚，并在命令结果里逐项提示。
func TestServerDeviceAnchorCopyConflict(t *testing.T) {
	withCliStoreEnv(t)
	_, fingerprintA := genHostKey(t)
	_, fingerprintB := genHostKey(t)
	runCli(t, "devices", "add", "机器")
	runCli(t, "servers", "add", "--name", "root", "--host", "192.0.2.10", "--user", "root", "--device", "机器")
	runCli(t, "servers", "add", "--name", "admin", "--host", "192.0.2.10", "--user", "admin", "--device", "机器")
	runCli(t, "servers", "pin-hostkey", "root", "--fingerprint", fingerprintA)
	runCli(t, "servers", "pin-hostkey", "admin", "--fingerprint", fingerprintB)

	out := runCli(t, "servers", "add", "--name", "deploy", "--host", "192.0.2.10",
		"--user", "deploy", "--device", "机器")
	if !strings.Contains(out, "同一设备、同一端口的其他服务器条目主机密钥锚不一致，未复制；请手工确认后锚定") {
		t.Fatalf("conflicting sibling anchors must be reported and not copied:\n%s", out)
	}
	out = runCli(t, "servers", "pin-hostkey", "deploy")
	if !strings.Contains(out, "no pin for deploy") {
		t.Fatalf("conflicting sibling anchors must not create an anchor:\n%s", out)
	}
}

// TestServerDeviceAnchorCopyGuards 覆盖锚复制的两个保护：端口不同不复制；
// 目标条目已有锚时绝不覆盖。
func TestServerDeviceAnchorCopyGuards(t *testing.T) {
	withCliStoreEnv(t)
	_, fingerprintA := genHostKey(t)
	_, fingerprintB := genHostKey(t)
	runCli(t, "devices", "add", "机器")
	runCli(t, "servers", "add", "--name", "root", "--host", "192.0.2.10",
		"--user", "root", "--device", "机器")
	runCli(t, "servers", "pin-hostkey", "root", "--fingerprint", fingerprintA)

	runCli(t, "servers", "add", "--name", "container", "--host", "192.0.2.10",
		"--port", "2222", "--user", "deploy", "--device", "机器")
	out := runCli(t, "servers", "pin-hostkey", "container")
	if !strings.Contains(out, "no pin for container") {
		t.Fatalf("a different port must not inherit the sibling anchor:\n%s", out)
	}

	runCli(t, "servers", "add", "--name", "deploy", "--host", "192.0.2.10", "--user", "deploy")
	runCli(t, "servers", "pin-hostkey", "deploy", "--fingerprint", fingerprintB)
	runCli(t, "devices", "move-server", "deploy", "--to", "机器")
	out = runCli(t, "servers", "pin-hostkey", "deploy")
	if !strings.Contains(out, fingerprintB) || strings.Contains(out, fingerprintA) {
		t.Fatalf("an existing anchor must never be replaced during a move:\n%s", out)
	}
}

// TestServersRemoveKeepsDevice 覆盖删除服务器条目只解除设备关联，设备本
// 身保留，详情里不再列出被删除的条目。
func TestServersRemoveKeepsDevice(t *testing.T) {
	withCliStoreEnv(t)
	runCli(t, "devices", "add", "机器")
	runCli(t, "servers", "add", "--name", "deploy", "--host", "192.0.2.10",
		"--user", "deploy", "--device", "机器")

	runCli(t, "servers", "rm", "--yes", "deploy")
	out := runCli(t, "devices", "show", "机器")
	if !strings.Contains(out, "设备 机器") || !strings.Contains(out, "服务器条目：无") {
		t.Fatalf("removing a server entry must keep its now-empty device:\n%s", out)
	}
}

// seedDeviceDeleteFixture 通过“导出、追加布局坐标、导入”构造一个完整设备
// 删除现场。布局坐标目前没有命令行写入入口。
func seedDeviceDeleteFixture(t *testing.T, dbPath string, masterKey []byte) {
	t.Helper()
	sourcePath := filepath.Join(t.TempDir(), "fixture.db")
	source, err := store.Open(sourcePath, masterKey)
	if err != nil {
		t.Fatalf("open fixture source store: %v", err)
	}
	defer source.Close()
	profileID, err := source.AddProfile("device-delete-profile")
	if err != nil {
		t.Fatal(err)
	}
	device, err := source.AddDevice("工作站", "", "")
	if err != nil {
		t.Fatal(err)
	}
	root, err := source.AddServerWithCredentialsToDevice(&models.Server{
		Name: "root", Host: "192.0.2.10", Port: 22, User: "root",
	}, &models.Credential{Type: models.CredPassword, Secret: []byte("root-password")}, nil, "工作站")
	if err != nil {
		t.Fatal(err)
	}
	deploy, err := source.AddServerWithCredentialsToDevice(&models.Server{
		Name: "deploy", Host: "192.0.2.10", Port: 22, User: "deploy",
	}, nil, nil, "工作站")
	if err != nil {
		t.Fatal(err)
	}
	if err := source.GrantServers(profileID, []string{root.ServerID, deploy.ServerID}); err != nil {
		t.Fatal(err)
	}
	if err := source.AddSecret("deploy_secret", "", []byte("secret-value"), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := source.AllowSecrets([]string{"deploy_secret"}, []string{"root", "deploy"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	rootServer, err := source.GetServerByName("root")
	if err != nil || rootServer == nil {
		t.Fatalf("read root server: server=%v err=%v", rootServer, err)
	}
	if _, err := source.UpsertManualEntryPin(
		rootServer, "", []byte("SHA256:fixture-host-key-fingerprint"),
		store.PinFormatFingerprint, false,
		func(old *store.PinMeta) store.AuditRow {
			return store.AuditRow{Action: "pin-manual"}
		},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := source.AddSite("家A", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := source.AddNetwork(models.Network{
		Name: "家A局域网", Type: models.NetworkPhysical, SiteName: "家A",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := source.AddNetworkMembership("工作站", "家A局域网", "192.168.1.20"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := source.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	snapshot.DeviceLayoutCoordinates = []store.SnapshotDeviceLayoutCoordinate{{
		ViewKey: "主视图", DeviceID: device.ID, X: 12.5, Y: -3, CreatedAt: 1000, UpdatedAt: 1001,
	}}
	target, err := store.Open(dbPath, masterKey)
	if err != nil {
		t.Fatalf("open fixture target store: %v", err)
	}
	if err := target.ImportSnapshot(snapshot); err != nil {
		target.Close()
		t.Fatalf("import fixture snapshot: %v", err)
	}
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}

	seedTopologyClient(t, dbPath, masterKey, "laptop")
	runCli(t, "devices", "edit", "工作站", "--client", "laptop")
}

// TestDevicesRemovePreviewsAndCascadesServerEntries 覆盖完整删除预览、
// 非交互守卫，以及确认后服务器条目、锚、授权、网络成员与布局坐标的级联。
func TestDevicesRemovePreviewsAndCascadesServerEntries(t *testing.T) {
	dbPath, masterKey := withCliStoreEnv(t)
	seedDeviceDeleteFixture(t, dbPath, masterKey)

	oldIsInteractive := topologyStdinIsInteractive
	topologyStdinIsInteractive = func() bool { return false }
	t.Cleanup(func() { topologyStdinIsInteractive = oldIsInteractive })
	errText := runCliErr(t, "devices", "rm", "工作站")
	if !strings.Contains(errText, "非交互终端") || !strings.Contains(errText, "--yes") {
		t.Fatalf("non-interactive device removal must require --yes:\n%s", errText)
	}
	if out := runCli(t, "devices", "show", "工作站"); !strings.Contains(out, "服务器条目：root") {
		t.Fatalf("refused device removal must keep the fixture:\n%s", out)
	}

	out := runCli(t, "devices", "rm", "工作站", "--yes")
	groups := []string{
		"将删除设备 工作站",
		"服务器条目：\n  deploy（用户 deploy，端口 22，未锚定）\n  root（用户 root，端口 22，已锚定）",
		"会从以下机密项的可写入服务器列表移出：\n  deploy_secret：deploy、root",
		"网络成员：\n  家A局域网：192.168.1.20",
		"布局坐标：\n  主视图：(12.5, -3)",
		"会被解除关联的客户端实例：laptop",
	}
	position := -1
	for _, want := range groups {
		next := strings.Index(out, want)
		if next < 0 {
			t.Fatalf("device removal preview must contain %q:\n%s", want, out)
		}
		if next < position {
			t.Fatalf("device removal preview groups are out of order:\n%s", out)
		}
		position = next
	}
	if !strings.Contains(out, "已删除设备 工作站") {
		t.Fatalf("device removal must report success:\n%s", out)
	}

	verification, err := store.Open(dbPath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer verification.Close()
	snapshot, err := verification.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Servers) != 0 || len(snapshot.EntryHostKeys) != 0 ||
		len(snapshot.SecretAllows) != 0 || len(snapshot.DeviceLayoutCoordinates) != 0 {
		t.Fatalf("device removal must cascade server entries, anchors, secret allows, and layouts: %+v",
			snapshot)
	}
	profiles, err := verification.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, profile := range profiles {
		if profile.Name != "device-delete-profile" {
			continue
		}
		servers, err := verification.ServersForProfile(profile.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(servers) != 0 {
			t.Fatalf("device removal must remove profile grants, got %+v", servers)
		}
	}
	clients, err := verification.ListCacheTokens()
	if err != nil {
		t.Fatal(err)
	}
	if len(clients) != 1 || clients[0].Name != "laptop" {
		t.Fatalf("device removal must keep the client instance: %+v", clients)
	}
	if out := runCli(t, "networks", "list"); !strings.Contains(out, "家A局域网") {
		t.Fatalf("device removal must keep networks:\n%s", out)
	}

	deviceRows := topologyAuditRows(t, dbPath, masterKey, "device.rm")
	if len(deviceRows) != 1 {
		t.Fatalf("device removal must leave exactly one device.rm audit row, got %d", len(deviceRows))
	}
	gotSummary := topologyAuditSummary(t, deviceRows[0])
	wantSummary := map[string]any{
		"name":               "工作站",
		"client":             "laptop",
		"servers":            []any{"deploy", "root"},
		"anchors":            []any{"root"},
		"members":            []any{map[string]any{"device": "工作站", "network": "家A局域网", "address": "192.168.1.20"}},
		"layout-coordinates": float64(1),
		"shared-storages":    []any{},
	}
	if !reflect.DeepEqual(gotSummary, wantSummary) {
		t.Fatalf("device.rm summary = %#v, want %#v", gotSummary, wantSummary)
	}
	for action, want := range map[string]int{
		"server.rm": 2, "pin-delete": 1, "secret-disallow": 2,
	} {
		if rows := topologyAuditRows(t, dbPath, masterKey, action); len(rows) != want {
			t.Fatalf("action %s must leave %d audit rows, got %d", action, want, len(rows))
		}
	}
}

func TestDevicesRemovePreviewsAndCascadesSharedStorages(t *testing.T) {
	dbPath, masterKey := seedSharesCommandVault(t)
	setup, err := store.Open(dbPath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setup.AddDevice("Web 主机", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.MoveServerToDevice("linux-web", "Web 主机"); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.MoveServerToDevice("windows-web", "Web 主机"); err != nil {
		t.Fatal(err)
	}
	if err := setup.Close(); err != nil {
		t.Fatal(err)
	}
	runCli(t, "shares", "add", "datasets", "--device", "NAS", "--user", "mount-user",
		"--password-secret", "NAS_MOUNT_PASSWORD", "--drive", "Z")

	before, err := store.Open(dbPath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	revisionBefore, err := before.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if err := before.Close(); err != nil {
		t.Fatal(err)
	}

	out := runCli(t, "devices", "rm", "NAS", "--yes")
	groups := []string{
		"共享存储：\n  datasets",
		"Linux 与 macOS 服务器（将删除配置并卸载）：",
		"linux-web（所属设备 Web 主机，系统类型 Linux 或 macOS）",
		"Windows 服务器（将随快照同步停止注入）：",
		"windows-web（所属设备 Web 主机，系统类型 Windows）",
	}
	position := -1
	for _, want := range groups {
		next := strings.Index(out, want)
		if next < 0 {
			t.Fatalf("设备删除预览应包含 %q：\n%s", want, out)
		}
		if next < position {
			t.Fatalf("设备删除预览的共享存储内容顺序不正确：\n%s", out)
		}
		position = next
	}
	if !strings.Contains(out, "已删除设备 NAS") {
		t.Fatalf("设备删除应报告成功：\n%s", out)
	}

	verification, err := store.Open(dbPath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer verification.Close()
	shares, err := verification.ListSharedStorages(store.SharedStorageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(shares) != 0 {
		t.Fatalf("设备删除应一并删除它提供的共享存储：%+v", shares)
	}
	revisionAfter, err := verification.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if revisionAfter <= revisionBefore {
		t.Fatalf("删除共享存储后快照修订号应前进：before=%d after=%d", revisionBefore, revisionAfter)
	}

	rows := topologyAuditRows(t, dbPath, masterKey, "device.rm")
	if len(rows) != 1 {
		t.Fatalf("设备删除应写一条 device.rm 审计行，实际 %d 条", len(rows))
	}
	summary := topologyAuditSummary(t, rows[0])
	if !reflect.DeepEqual(summary["shared-storages"], []any{"datasets"}) {
		t.Fatalf("device.rm 审计摘要应记录一并删除的共享存储：%#v", summary)
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

func TestDevicesListShowsUnassignedSection(t *testing.T) {
	withCliStoreEnv(t)
	seedTopologySites(t, "家A")
	runCli(t, "networks", "add", "家A局域网", "--type", "physical", "--site", "家A")
	runCli(t, "devices", "add", "工作站")
	runCli(t, "devices", "add", "未归类设备")
	runCli(t, "devices", "member", "add", "工作站", "--network", "家A局域网", "--address", "192.168.1.20")

	out := runCli(t, "devices", "list")
	categorizedAt := strings.Index(out, "已归类设备：")
	unassignedAt := strings.Index(out, "未归类设备：")
	if categorizedAt < 0 || unassignedAt < 0 || categorizedAt > unassignedAt {
		t.Fatalf("unfiltered list must show categorized devices before a separate unassigned section:\n%s", out)
	}
	if !strings.Contains(out[categorizedAt:unassignedAt], "工作站") ||
		strings.Contains(out[categorizedAt:unassignedAt], "未归类设备") {
		t.Fatalf("categorized section must contain only devices with network memberships:\n%s", out)
	}
	if !strings.Contains(out[unassignedAt:], "未归类设备") ||
		strings.Contains(out[unassignedAt:], "工作站") {
		t.Fatalf("unassigned section must contain only devices without network memberships:\n%s", out)
	}
}

func TestTopologyNamesTrimSurroundingWhitespace(t *testing.T) {
	withCliStoreEnv(t)

	out := runCli(t, "sites", "add", " 家A ")
	if !strings.Contains(out, "已登记站点 家A\n") {
		t.Fatalf("site registration must trim the stored name:\n%s", out)
	}
	out = runCli(t, "networks", "add", " 家A局域网 ", "--type", "physical", "--site", " 家A ")
	if !strings.Contains(out, "已登记网络 家A局域网") {
		t.Fatalf("network registration must trim its name and look up the site after trimming:\n%s", out)
	}
	out = runCli(t, "devices", "add", " 工作站 ")
	if !strings.Contains(out, "已登记设备 工作站\n") {
		t.Fatalf("device registration must trim the stored name:\n%s", out)
	}

	out = runCli(t, "sites", "edit", " 家A ", "--name", " 家B ")
	if !strings.Contains(out, "新名字 家B") {
		t.Fatalf("site rename must trim lookup and new names:\n%s", out)
	}
	out = runCli(t, "networks", "edit", " 家A局域网 ", "--name", " 家B局域网 ", "--site", " 家B ")
	if !strings.Contains(out, "新名字 家B局域网") {
		t.Fatalf("network rename must trim lookup and new names:\n%s", out)
	}
	out = runCli(t, "devices", "edit", " 工作站 ", "--name", " 新工作站 ")
	if !strings.Contains(out, "新名字 新工作站") {
		t.Fatalf("device rename must trim lookup and new names:\n%s", out)
	}

	out = runCli(t, "networks", "list", "--site", " 家B ")
	if !strings.Contains(out, "网络 家B局域网") || strings.Contains(out, "网络  ") {
		t.Fatalf("network lookup and output must use trimmed names:\n%s", out)
	}
	out = runCli(t, "devices", "show", " 新工作站 ")
	if !strings.Contains(out, "设备 新工作站\n") {
		t.Fatalf("device lookup and output must use trimmed names:\n%s", out)
	}
}

// TestDevicesRemoveCleansCurrentRelations 覆盖设备删除确认、删除网络成员、
// 解除客户端实例关联。
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
