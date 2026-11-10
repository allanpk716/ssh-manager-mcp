package softwareinventory

import (
	"errors"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// newTestManager 打开一个全新的临时 vault，并返回挂在它上面的软件清单
// 领域模块。存储句柄只用于准备设备、服务器条目与 profile，以及读回审计。
func newTestManager(t *testing.T) (*Manager, *store.Store) {
	t.Helper()
	key, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "vault.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return New(st), st
}

// mustDevice 新增一台设备；needServer 为 true 时再挂一个服务器条目。
func mustDevice(t *testing.T, st *store.Store, name string, needServer bool) string {
	t.Helper()
	device, err := st.AddDevice(name, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if needServer {
		credentialID, err := st.SetCredential(&models.Credential{
			Type: models.CredPassword, Secret: []byte("test-password"),
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = st.AddServerWithCredentialsToDevice(&models.Server{
			Name: name + "-ssh", Host: "192.0.2.10", Port: 22, User: "owner",
			AuthMethod: models.AuthPassword, CredentialID: credentialID,
		}, nil, nil, name)
		if err != nil {
			t.Fatal(err)
		}
	}
	return device.ID
}

// TestDeclarationVisibilityAndQueries 覆盖声明的字段保存、按设备查看、跨设备
// 搜索，以及可见设备范围：owner 能看全部设备，profile 只能看至少有一个
// 服务器条目被授权的受管设备，看不到不受管设备。
func TestDeclarationVisibilityAndQueries(t *testing.T) {
	manager, st := newTestManager(t)
	mustDevice(t, st, "受管甲", true)
	mustDevice(t, st, "受管乙", true)
	mustDevice(t, st, "网络存储", false)

	opensshID, err := manager.AddDeclaration(Declaration{
		Device:         "受管甲",
		Category:       CategoryServiceProgram,
		Name:           "OpenSSH",
		Version:        "1:9.6p1-3ubuntu1",
		PackageURL:     "pkg:deb/ubuntu/openssh-server@1:9.6p1-3ubuntu1",
		CPE:            "cpe:2.3:a:openbsd:openssh:9.6p1:-:*:*:*:*:*:*",
		ListeningPorts: []ListeningPort{{Address: "0.0.0.0", Port: 22, Protocol: "tcp", Exposure: ExposurePublicNetwork}},
		BelongsTo:      "系统服务",
		Note:           "管理员登录入口",
	}, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if opensshID == "" {
		t.Fatal("新增声明必须返回可用于修改和删除的标识符")
	}
	for _, declaration := range []Declaration{
		{Device: "受管乙", Category: CategoryServiceProgram, Name: "nginx", Version: "1.25.3"},
		{Device: "网络存储", Category: CategoryOperatingSystem, Name: "Synology DSM", Version: "7.2.2-72806"},
		{Device: "受管甲", Category: CategorySystemPackage, Name: "libc6", Version: "2.39-0ubuntu8"},
	} {
		if _, err := manager.AddDeclaration(declaration, CLISource()); err != nil {
			t.Fatal(err)
		}
	}

	ownerInventory, err := manager.ViewDevice("受管甲", AllDevicesVisible(), DeviceViewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ownerInventory.Groups) != 2 {
		t.Fatalf("受管甲分组数量 = %d，应为 2：%+v", len(ownerInventory.Groups), ownerInventory.Groups)
	}
	serviceGroup := ownerInventory.Groups[0]
	if serviceGroup.Category != CategoryServiceProgram || len(serviceGroup.Items) != 1 {
		t.Fatalf("服务程序分组 = %+v", serviceGroup)
	}
	item := serviceGroup.Items[0]
	if item.Name != "OpenSSH" || item.Version != "1:9.6p1-3ubuntu1" ||
		item.PackageURL != "pkg:deb/ubuntu/openssh-server@1:9.6p1-3ubuntu1" ||
		item.CPE != "cpe:2.3:a:openbsd:openssh:9.6p1:-:*:*:*:*:*:*" ||
		item.BelongsTo != "系统服务" || item.Note != "管理员登录入口" ||
		item.Source != SourceDeclared {
		t.Fatalf("声明字段未被原样保存：%+v", item)
	}
	if len(item.ListeningPorts) != 1 || item.ListeningPorts[0].Address != "0.0.0.0" ||
		item.ListeningPorts[0].Port != 22 || item.ListeningPorts[0].Protocol != "tcp" ||
		item.ListeningPorts[0].Exposure != ExposurePublicNetwork {
		t.Fatalf("监听端口未被原样保存：%+v", item.ListeningPorts)
	}
	packageGroup := ownerInventory.Groups[1]
	if packageGroup.Category != CategorySystemPackage || len(packageGroup.Items) != 0 ||
		packageGroup.SystemPackageCount != 1 || !packageGroup.SystemPackagesOmitted {
		t.Fatalf("系统包默认只显示数量：%+v", packageGroup)
	}
	fullInventory, err := manager.ViewDevice("受管甲", AllDevicesVisible(), DeviceViewOptions{
		IncludeSystemPackages: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := fullInventory.Groups[1].Items; len(got) != 1 || got[0].Name != "libc6" ||
		fullInventory.Groups[1].SystemPackagesOmitted {
		t.Fatalf("显式列出系统包失败：%+v", fullInventory.Groups[1])
	}

	server, err := st.GetServerByName("受管甲-ssh")
	if err != nil || server == nil {
		t.Fatalf("取受管甲服务器条目失败：%v %+v", err, server)
	}
	profileVisibility := DevicesVisibleThroughServers(server.ID)
	visible, err := manager.ViewDevice("受管甲", profileVisibility, DeviceViewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(visible.Groups) == 0 || visible.Groups[0].Items[0].Name != "OpenSSH" {
		t.Fatalf("profile 应能看到授权服务器所在设备的清单：%+v", visible)
	}
	if _, err := manager.ViewDevice("受管乙", profileVisibility, DeviceViewOptions{}); err == nil ||
		!errors.Is(err, ErrDeviceNotVisible) {
		t.Fatalf("profile 查看范围外设备应返回不可见错误，实际：%v", err)
	}
	if _, err := manager.ViewDevice("网络存储", profileVisibility, DeviceViewOptions{}); err == nil ||
		!errors.Is(err, ErrDeviceNotVisible) {
		t.Fatalf("profile 不应看到不受管设备，实际：%v", err)
	}

	ownerHits, err := manager.SearchSoftware("ssh", AllDevicesVisible())
	if err != nil {
		t.Fatal(err)
	}
	if len(ownerHits) != 1 || ownerHits[0].DeviceName != "受管甲" ||
		ownerHits[0].Item.Name != "OpenSSH" {
		t.Fatalf("owner 跨设备搜索结果 = %+v", ownerHits)
	}
	profileHits, err := manager.SearchSoftware("SSH", profileVisibility)
	if err != nil {
		t.Fatal(err)
	}
	if len(profileHits) != 1 || profileHits[0].DeviceName != "受管甲" {
		t.Fatalf("profile 跨设备搜索结果 = %+v", profileHits)
	}
}

// TestDeclarationEditDeleteAndAudit 覆盖声明修改、删除，以及三个写操作的
// 审计来源。
func TestDeclarationEditDeleteAndAudit(t *testing.T) {
	manager, st := newTestManager(t)
	mustDevice(t, st, "工作站", false)
	id, err := manager.AddDeclaration(Declaration{
		Device: "工作站", Category: CategoryContainerImage,
		Name: "postgres", Version: "16.2",
		ListeningPorts: []ListeningPort{{Address: "127.0.0.1", Port: 5432, Protocol: "tcp", Exposure: ExposureLocalMachine}},
	}, CLISource())
	if err != nil {
		t.Fatal(err)
	}

	version := "16.4"
	ports := []ListeningPort{{Address: "192.0.2.20", Port: 5432, Protocol: "tcp", Exposure: ExposureVirtualLocalAreaNetwork}}
	if err := manager.EditDeclaration(id, DeclarationEdit{
		Version:        &version,
		PackageURL:     stringPointer("pkg:docker/library/postgres@16.4"),
		ListeningPorts: &ports,
	}, CLISource()); err != nil {
		t.Fatal(err)
	}
	inventory, err := manager.ViewDevice("工作站", AllDevicesVisible(), DeviceViewOptions{IncludeSystemPackages: true})
	if err != nil {
		t.Fatal(err)
	}
	item := inventory.Groups[0].Items[0]
	if item.Version != "16.4" || item.PackageURL != "pkg:docker/library/postgres@16.4" ||
		len(item.ListeningPorts) != 1 || item.ListeningPorts[0].Exposure != ExposureVirtualLocalAreaNetwork {
		t.Fatalf("修改后的声明 = %+v", item)
	}

	rows, err := st.QueryAudit(store.AuditFilter{Actions: []string{
		"software-declaration-add", "software-declaration-edit", "software-declaration-rm",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("修改后应已有新增与修改两条审计，实际：%+v", rows)
	}
	for _, row := range rows {
		if row.Source != store.AuditSourceCLI || row.SourceIP != "" || row.Status != "ok" {
			t.Fatalf("声明审计来源或状态不正确：%+v", row)
		}
	}

	if err := manager.DeleteDeclaration(id, CLISource()); err != nil {
		t.Fatal(err)
	}
	rows, err = st.QueryAudit(store.AuditFilter{Actions: []string{
		"software-declaration-add", "software-declaration-edit", "software-declaration-rm",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0].Action != "software-declaration-rm" || rows[0].Source != store.AuditSourceCLI {
		t.Fatalf("删除后的审计行 = %+v", rows)
	}
	afterDelete, err := manager.ViewDevice("工作站", AllDevicesVisible(), DeviceViewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(afterDelete.Groups) != 0 {
		t.Fatalf("删除声明后不应还有条目，实际：%+v", afterDelete)
	}
}

// TestDeclarationEditMovesPortsToDevice 覆盖声明移动设备时的端口归属：
// 监听端口行必须跟随声明一起移动，否则旧设备删除时会把新设备声明的端口
// 级联删掉。
func TestDeclarationEditMovesPortsToDevice(t *testing.T) {
	manager, st := newTestManager(t)
	mustDevice(t, st, "旧设备", false)
	mustDevice(t, st, "新设备", false)
	id, err := manager.AddDeclaration(Declaration{
		Device: "旧设备", Category: CategoryServiceProgram, Name: "nginx", Version: "1.25.3",
		ListeningPorts: []ListeningPort{{Address: "0.0.0.0", Port: 443, Protocol: "tcp", Exposure: ExposurePublicNetwork}},
	}, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	newDevice := "新设备"
	if err := manager.EditDeclaration(id, DeclarationEdit{Device: &newDevice}, CLISource()); err != nil {
		t.Fatal(err)
	}
	preview, err := st.DeviceDeletePreview("新设备")
	if err != nil {
		t.Fatal(err)
	}
	if preview.SoftwareDeclarationCount != 1 || preview.SoftwareListeningPortCount != 1 {
		t.Fatalf("声明移动后新设备的软件清单数量 = 声明 %d、端口 %d，应为 1、1",
			preview.SoftwareDeclarationCount, preview.SoftwareListeningPortCount)
	}
	oldPreview, err := st.DeviceDeletePreview("旧设备")
	if err != nil {
		t.Fatal(err)
	}
	if oldPreview.SoftwareDeclarationCount != 0 || oldPreview.SoftwareListeningPortCount != 0 {
		t.Fatalf("声明移动后旧设备不应保留清单数量：%+v", oldPreview)
	}
}

func stringPointer(value string) *string { return &value }

// TestSoftwareInventoryBackupProfileSnapshotAndRevision 覆盖三条存储边界：
// 全量导出能恢复声明与端口；profile 导出不含任何软件清单数据；清单变化
// 不推进客户端快照使用的修订号。
func TestSoftwareInventoryBackupProfileSnapshotAndRevision(t *testing.T) {
	manager, st := newTestManager(t)
	mustDevice(t, st, "受管甲", true)
	mustDevice(t, st, "网络存储", false)
	_, err := manager.AddDeclaration(Declaration{
		Device: "受管甲", Category: CategoryServiceProgram,
		Name: "OpenSSH", Version: "1:9.6p1-3ubuntu1",
		ListeningPorts: []ListeningPort{{Address: "0.0.0.0", Port: 22, Protocol: "tcp", Exposure: ExposurePublicNetwork}},
	}, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	unmanagedID, err := manager.AddDeclaration(Declaration{
		Device: "网络存储", Category: CategoryOperatingSystem,
		Name: "Synology DSM", Version: "7.2.2-72806",
	}, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	server, err := st.GetServerByName("受管甲-ssh")
	if err != nil || server == nil {
		t.Fatalf("取服务器条目失败：%v %+v", err, server)
	}
	profileID, err := st.AddProfile("运维")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{server.ID}); err != nil {
		t.Fatal(err)
	}

	revisionBefore, err := st.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	profileSnapshot, err := st.ExportSnapshotForProfile(profileID, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(profileSnapshot.SoftwareDeclarations) != 0 || len(profileSnapshot.SoftwareDeclarationPorts) != 0 {
		t.Fatalf("profile 导出不得携带软件清单：%+v %+v",
			profileSnapshot.SoftwareDeclarations, profileSnapshot.SoftwareDeclarationPorts)
	}
	version := "1:9.8p1-3ubuntu1"
	if err := manager.EditDeclaration(unmanagedID, DeclarationEdit{Version: &version}, CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := manager.DeleteDeclaration(unmanagedID, CLISource()); err != nil {
		t.Fatal(err)
	}
	revisionAfter, err := st.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if revisionBefore != revisionAfter {
		t.Fatalf("软件清单变化推进了快照修订号：%d -> %d", revisionBefore, revisionAfter)
	}

	fullSnapshot, err := st.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	targetKey, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	target, err := store.Open(filepath.Join(t.TempDir(), "restored.db"), targetKey)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if err := target.ImportSnapshot(fullSnapshot); err != nil {
		t.Fatal(err)
	}
	restored, err := New(target).ViewDevice("受管甲", AllDevicesVisible(), DeviceViewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.Groups) != 1 || len(restored.Groups[0].Items) != 1 {
		t.Fatalf("恢复后的软件清单 = %+v", restored)
	}
	item := restored.Groups[0].Items[0]
	if item.Name != "OpenSSH" || item.Version != "1:9.6p1-3ubuntu1" ||
		len(item.ListeningPorts) != 1 || item.ListeningPorts[0].Exposure != ExposurePublicNetwork {
		t.Fatalf("全量导出没有逐字段恢复声明与端口：%+v", item)
	}
}

// TestDeviceDeleteIncludesSoftwareInventory 覆盖设备删除预览与确认后的级联：
// 预览列出软件条目、声明与监听端口数量；删除后重新登记同名设备不会看到
// 旧声明。
func TestDeviceDeleteIncludesSoftwareInventory(t *testing.T) {
	manager, st := newTestManager(t)
	mustDevice(t, st, "网络存储", false)
	_, err := manager.AddDeclaration(Declaration{
		Device: "网络存储", Category: CategoryServiceProgram,
		Name: "nginx", Version: "1.25.3",
		ListeningPorts: []ListeningPort{
			{Address: "0.0.0.0", Port: 80, Protocol: "tcp", Exposure: ExposureAllAddresses},
			{Address: "0.0.0.0", Port: 443, Protocol: "tcp", Exposure: ExposureAllAddresses},
		},
	}, CLISource())
	if err != nil {
		t.Fatal(err)
	}

	preview, err := st.DeviceDeletePreview("网络存储")
	if err != nil {
		t.Fatal(err)
	}
	if preview.SoftwareItemCount != 1 || preview.SoftwareDeclarationCount != 1 ||
		preview.SoftwareListeningPortCount != 2 {
		t.Fatalf("设备删除预览的软件清单数量 = 条目 %d、声明 %d、端口 %d",
			preview.SoftwareItemCount, preview.SoftwareDeclarationCount,
			preview.SoftwareListeningPortCount)
	}
	if _, err := st.DeleteDeviceCascading("网络存储", preview); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("网络存储", "", ""); err != nil {
		t.Fatal(err)
	}
	after, err := manager.ViewDevice("网络存储", AllDevicesVisible(), DeviceViewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Groups) != 0 {
		t.Fatalf("设备删除后软件声明必须级联删除，实际：%+v", after)
	}
}
