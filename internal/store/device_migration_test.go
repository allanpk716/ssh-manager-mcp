package store

import (
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// prepareUnmigratedStore 建出升级前形态：结构已经建立，但服务器条目还没有
// 设备归属，也没有「设备生成已执行」标记。标记只在测试准备旧库时删除；
// 全部结果断言都走打开、列表、移动与审计这些公开行为。
func prepareUnmigratedStore(t *testing.T) (*Store, string, []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "old-vault.db")
	masterKey, err := GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM settings WHERE key=?`, serverDeviceMigrationSetting); err != nil {
		t.Fatal(err)
	}
	return s, path, masterKey
}

// TestDeviceMigrationMergesByHostAndAnchorOnce 覆盖升级合并规则、唯一一条
// 迁移审计，以及重复打开不打乱 owner 后来做出的归类。
func TestDeviceMigrationMergesByHostAndAnchorOnce(t *testing.T) {
	s, path, masterKey := prepareUnmigratedStore(t)
	rootID := addEntryForTest(t, s, "root", "Machine.example", 22)
	deployID := addEntryForTest(t, s, "deploy", "machine.example", 2222)
	otherAnchorID := addEntryForTest(t, s, "other-anchor", "machine.example", 22)
	noAnchorAID := addEntryForTest(t, s, "no-anchor-a", "192.0.2.30", 22)
	noAnchorBID := addEntryForTest(t, s, "no-anchor-b", "192.0.2.30", 2200)
	sharedKey, _ := pinHostKey(t)
	otherKey, _ := pinHostKey(t)
	for id, key := range map[string][]byte{
		rootID: sharedKey, deployID: sharedKey, otherAnchorID: otherKey,
	} {
		if err := s.SaveEntryPin(serverOf(t, s, id), key); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := Open(path, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	root := serverOf(t, upgraded, rootID)
	deploy := serverOf(t, upgraded, deployID)
	otherAnchor := serverOf(t, upgraded, otherAnchorID)
	noAnchorA := serverOf(t, upgraded, noAnchorAID)
	noAnchorB := serverOf(t, upgraded, noAnchorBID)
	if root.DeviceID == "" || root.DeviceID != deploy.DeviceID {
		t.Fatalf("同兜底 Host 且同主机密钥指纹的条目应合并：%+v %+v", root, deploy)
	}
	if otherAnchor.DeviceID == "" || otherAnchor.DeviceID == root.DeviceID {
		t.Fatalf("同兜底 Host 但指纹不同的条目不应合并：%+v", otherAnchor)
	}
	if noAnchorA.DeviceID == "" || noAnchorB.DeviceID == "" || noAnchorA.DeviceID == noAnchorB.DeviceID {
		t.Fatalf("没有锚的条目应各自成设备：%+v %+v", noAnchorA, noAnchorB)
	}
	devices, err := upgraded.ListDevices(DeviceFilter{Unassigned: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 4 {
		t.Fatalf("未归类设备数量 = %d，应为 4：%+v", len(devices), devices)
	}
	for _, device := range devices {
		if len(device.Memberships) != 0 {
			t.Fatalf("迁移生成的设备不应有网络成员：%+v", device)
		}
	}
	deviceNames := make(map[string]string)
	for _, server := range []*models.Server{root, deploy, otherAnchor, noAnchorA, noAnchorB} {
		for _, device := range devices {
			if device.ID == server.DeviceID {
				deviceNames[server.Name] = device.Name
			}
		}
	}
	if deviceNames["root"] != "Machine.example" || deviceNames["deploy"] != "Machine.example" ||
		deviceNames["other-anchor"] != "other-anchor" || deviceNames["no-anchor-a"] != "no-anchor-a" ||
		deviceNames["no-anchor-b"] != "no-anchor-b" {
		t.Fatalf("迁移设备命名不正确：%v", deviceNames)
	}
	migrations, err := upgraded.QueryAudit(AuditFilter{Actions: []string{"device.migrate"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) != 1 {
		t.Fatalf("迁移审计行数 = %d，应为 1：%+v", len(migrations), migrations)
	}
	for _, name := range []string{
		"Machine.example", "root", "deploy", "other-anchor", "no-anchor-a", "no-anchor-b",
	} {
		if !containsAll(migrations[0].Command, name) {
			t.Fatalf("迁移审计没有列出设备或条目 %s：%q", name, migrations[0].Command)
		}
	}

	ownerDevice, err := upgraded.AddDevice("owner-choice", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upgraded.MoveServerToDevice("root", "owner-choice"); err != nil {
		t.Fatal(err)
	}
	if err := upgraded.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(path, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	rootAfterReopen := serverOf(t, again, rootID)
	if rootAfterReopen.DeviceID != ownerDevice.ID {
		t.Fatalf("重复打开后 owner 的归类被打乱：%+v", rootAfterReopen)
	}
	migrationsAgain, err := again.QueryAudit(AuditFilter{Actions: []string{"device.migrate"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(migrationsAgain) != 1 {
		t.Fatalf("重复打开后的迁移审计行数 = %d，应为 1", len(migrationsAgain))
	}
}

// TestDeviceMigrationSuffixesDuplicateNames 钉住迁移命名与 servers add 自动
// 生成设备相同的重名处理：小写冲突的第二个名字追加数字后缀。
func TestDeviceMigrationSuffixesDuplicateNames(t *testing.T) {
	s, path, masterKey := prepareUnmigratedStore(t)
	alphaID := addEntryForTest(t, s, "Alpha", "192.0.2.41", 22)
	alphaLowerID := addEntryForTest(t, s, "alpha", "192.0.2.42", 22)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := Open(path, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	alpha := serverOf(t, upgraded, alphaID)
	alphaLower := serverOf(t, upgraded, alphaLowerID)
	if alpha.DeviceID == alphaLower.DeviceID {
		t.Fatalf("无锚条目不应因名字大小写相同而合并：%+v %+v", alpha, alphaLower)
	}
	devices, err := upgraded.ListDevices(DeviceFilter{})
	if err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool, len(devices))
	for _, device := range devices {
		names[device.Name] = true
	}
	if !names["Alpha"] || !names["alpha-2"] {
		t.Fatalf("设备重名应生成 Alpha 与 alpha-2，实际：%v", names)
	}
}

// TestImportOldSnapshotMigratesDevices 导入不含拓扑段的旧导出时，走与升级
// 相同的设备生成和合并规则。
func TestImportOldSnapshotMigratesDevices(t *testing.T) {
	sourceDir := t.TempDir()
	masterKey, err := GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	source, err := Open(filepath.Join(sourceDir, "source.db"), masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	rootID := addEntryForTest(t, source, "root", "192.0.2.50", 22)
	deployID := addEntryForTest(t, source, "deploy", "192.0.2.50", 2200)
	sharedKey, _ := pinHostKey(t)
	for _, id := range []string{rootID, deployID} {
		if err := source.SaveEntryPin(serverOf(t, source, id), sharedKey); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := source.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Devices != nil {
		t.Fatalf("测试输入应是设备段为空的旧快照：%+v", snapshot.Devices)
	}

	target, err := Open(filepath.Join(t.TempDir(), "target.db"), masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if err := target.ImportSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	root := serverOf(t, target, rootID)
	deploy := serverOf(t, target, deployID)
	if root.DeviceID == "" || root.DeviceID != deploy.DeviceID {
		t.Fatalf("导入旧快照后同指纹条目应合并：%+v %+v", root, deploy)
	}
	migrations, err := target.QueryAudit(AuditFilter{Actions: []string{"device.migrate"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) != 1 || !containsAll(migrations[0].Command, "192.0.2.50", "root", "deploy") {
		t.Fatalf("导入旧快照的迁移审计不正确：%+v", migrations)
	}
}
