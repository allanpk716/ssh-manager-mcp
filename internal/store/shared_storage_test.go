package store

import (
	"errors"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

func seedSharedStorageVault(t *testing.T) *Store {
	t.Helper()
	s := newTestStore(t)
	if _, err := s.AddDevice("NAS", "存放共享数据", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSecret("NAS_MOUNT_PASSWORD", "共享存储挂载密码", []byte("mount-password"), CLISource()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddServer(&models.Server{
		Name: "linux-web", Host: "192.0.2.20", Port: 22, User: "deploy", AuthMethod: models.AuthPassword,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddServer(&models.Server{
		Name: "windows-web", Host: "192.0.2.21", Port: 22, User: "deploy", AuthMethod: models.AuthPassword,
		ShellType: models.ShellPowerShell,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AllowSecrets([]string{"NAS_MOUNT_PASSWORD"}, []string{"linux-web", "windows-web"}, CLISource()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddSite("办公室", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddNetwork(models.Network{Name: "办公局域网", Type: models.NetworkPhysical, SiteName: "办公室"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddNetworkMembership("NAS", "办公局域网", "192.0.2.10"); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSharedStorageAddGetAndList(t *testing.T) {
	s := seedSharedStorageVault(t)
	added, err := s.AddSharedStorage(SharedStorageInput{
		Name: "datasets", Device: "nas", User: "mount-user",
		PasswordSecret: "NAS_MOUNT_PASSWORD", Drive: "z",
		Directories: []models.SharedStorageDirectory{
			{Purpose: "临时数据", Path: "tmp"},
			{Purpose: "长期数据", Path: "datasets"},
		},
		Note: "模型数据集",
	}, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if added.Version != 1 || added.Drive != "Z" || added.DeviceName != "NAS" ||
		added.PasswordSecretName != "NAS_MOUNT_PASSWORD" {
		t.Fatalf("新增后的共享存储登记不正确：%+v", added)
	}
	if len(added.CandidateAddresses) != 1 || added.CandidateAddresses[0] != "192.0.2.10" {
		t.Fatalf("候选访问地址不正确：%v", added.CandidateAddresses)
	}
	if len(added.MountableServers) != 2 ||
		added.MountableServers[0].MountPoint != "/mnt/sshmgr/datasets" ||
		added.MountableServers[1].MountPoint != "Z:" {
		t.Fatalf("可挂载服务器与挂载点不正确：%+v", added.MountableServers)
	}

	got, err := s.GetSharedStorage("DATASETS")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "datasets" || len(got.Directories) != 2 || got.Directories[1].Purpose != "长期数据" {
		t.Fatalf("按名字查询应不区分大小写并保留目录约定顺序：%+v", got)
	}

	all, err := s.ListSharedStorages(SharedStorageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].ID != added.ID {
		t.Fatalf("共享存储列表不正确：%+v", all)
	}
	byServer, err := s.ListSharedStorages(SharedStorageFilter{Server: "linux-web"})
	if err != nil {
		t.Fatal(err)
	}
	if len(byServer) != 1 || len(byServer[0].MountableServers) != 2 {
		t.Fatalf("按可挂载服务器筛选不正确：%+v", byServer)
	}

	_, err = s.AddSharedStorage(SharedStorageInput{
		Name: "scratch", Device: "NAS", User: "mount-user",
		PasswordSecret: "NAS_MOUNT_PASSWORD", Drive: "Y",
	}, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	byDevice, err := s.ListSharedStorages(SharedStorageFilter{Device: "nas"})
	if err != nil {
		t.Fatal(err)
	}
	if len(byDevice) != 2 {
		t.Fatalf("按提供方设备筛选应返回同一台设备的多个共享：%+v", byDevice)
	}
	none, err := s.ListSharedStorages(SharedStorageFilter{Device: "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("不存在的提供方设备不应筛出共享存储：%+v", none)
	}
}

func TestSharedStorageAddRejectsInvalidInput(t *testing.T) {
	s := seedSharedStorageVault(t)
	_, err := s.AddSharedStorage(SharedStorageInput{
		Name: "datasets", Device: "missing", User: "mount-user",
		PasswordSecret: "NAS_MOUNT_PASSWORD", Drive: "Z",
	}, CLISource())
	if err == nil || !strings.Contains(err.Error(), "请先执行 sshmgr devices add 登记设备") {
		t.Fatalf("提供方设备不存在时应给出登记设备指引，实际：%v", err)
	}
	_, err = s.AddSharedStorage(SharedStorageInput{
		Name: "datasets", Device: "NAS", User: "mount-user",
		PasswordSecret: "missing", Drive: "Z",
	}, CLISource())
	if err == nil || !strings.Contains(err.Error(), "请先执行 sshmgr secrets add 登记机密项") {
		t.Fatalf("挂载密码机密项不存在时应给出登记机密项指引，实际：%v", err)
	}

	input := SharedStorageInput{
		Name: "datasets", Device: "NAS", User: "mount-user",
		PasswordSecret: "NAS_MOUNT_PASSWORD", Drive: "z",
		Directories: []models.SharedStorageDirectory{{Purpose: "临时数据", Path: "tmp"}},
	}
	first, err := s.AddSharedStorage(input, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	input.Name = "DATASETS"
	input.Drive = "y"
	if _, err := s.AddSharedStorage(input, CLISource()); err == nil ||
		!errors.Is(err, ErrSharedStorageNameExists) {
		t.Fatalf("共享名应不区分大小写去重，实际：%v", err)
	}
	input.Name = "scratch"
	input.Drive = "z"
	if _, err := s.AddSharedStorage(input, CLISource()); err == nil ||
		!errors.Is(err, ErrSharedStorageDriveExists) {
		t.Fatalf("盘符应全库唯一，实际：%v", err)
	}
	for _, drive := range []string{"c", "a", "zz", ""} {
		input.Drive = drive
		if _, err := s.AddSharedStorage(input, CLISource()); !errors.Is(err, ErrSharedStorageDriveInvalid) {
			t.Fatalf("非法盘符 %q 应被拒绝，实际：%v", drive, err)
		}
	}
	input.Drive = "Y"
	input.Directories = []models.SharedStorageDirectory{{Purpose: "临时数据", Path: ""}}
	if _, err := s.AddSharedStorage(input, CLISource()); err == nil ||
		!strings.Contains(err.Error(), "用途与子路径都不能为空") {
		t.Fatalf("目录约定缺子路径应被拒绝，实际：%v", err)
	}
	input.Directories = []models.SharedStorageDirectory{{Purpose: "临时数据", Path: "tmp"}, {Purpose: "临时数据", Path: "other"}}
	if _, err := s.AddSharedStorage(input, CLISource()); err == nil ||
		!strings.Contains(err.Error(), "用途不能重复") {
		t.Fatalf("重复用途应被拒绝，实际：%v", err)
	}
	input.Directories = nil
	second, err := s.AddSharedStorage(input, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if second.DeviceID != first.DeviceID || second.PasswordSecretID != first.PasswordSecretID {
		t.Fatalf("同一台设备与同一个挂载密码机密项应能提供多个共享：%+v %+v", first, second)
	}
}

func TestSharedStorageEditDeleteVersionAuditAndRevision(t *testing.T) {
	s := seedSharedStorageVault(t)
	if _, err := s.AddDevice("第二NAS", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSecret("SECOND_MOUNT_PASSWORD", "", []byte("second-password"), CLISource()); err != nil {
		t.Fatal(err)
	}
	_, err := s.AddSharedStorage(SharedStorageInput{
		Name: "datasets", Device: "NAS", User: "mount-user",
		PasswordSecret: "NAS_MOUNT_PASSWORD", Drive: "Z",
		Directories: []models.SharedStorageDirectory{{Purpose: "旧用途", Path: "old"}},
		Note:        "旧备注",
	}, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	revisionBefore, err := s.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	newName := "models"
	newDevice := "第二NAS"
	newUser := "service-account"
	newSecret := "SECOND_MOUNT_PASSWORD"
	newDrive := "Y"
	newDirectories := []models.SharedStorageDirectory{{Purpose: "新用途", Path: "new"}}
	newNote := "新备注"
	changeNote := "换到新设备并更新目录约定"
	edited, err := s.EditSharedStorage("DATASETS", SharedStorageChanges{
		Name: &newName, Device: &newDevice, User: &newUser,
		PasswordSecret: &newSecret, Drive: &newDrive,
		Directories: &newDirectories, Note: &newNote, ChangeNote: &changeNote,
	}, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if edited.Name != "models" || edited.DeviceName != "第二NAS" ||
		edited.MountUser != "service-account" || edited.PasswordSecretName != "SECOND_MOUNT_PASSWORD" ||
		edited.Drive != "Y" || len(edited.Directories) != 1 || edited.Note != "新备注" {
		t.Fatalf("部分更新后的共享存储登记不正确：%+v", edited)
	}
	if edited.Version != 2 || edited.ChangeNote != changeNote {
		t.Fatalf("主动修改应让共享存储版本号加一并写入变更说明：%+v", edited)
	}
	if after, err := s.SnapshotRevision(); err != nil || after != revisionBefore+1 {
		t.Fatalf("修改共享存储后的快照修订号 = %d，应为 %d：%v", after, revisionBefore+1, err)
	}

	onlyNote := "只改变更说明也算修改"
	edited, err = s.EditSharedStorage("models", SharedStorageChanges{ChangeNote: &onlyNote}, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if edited.Version != 3 || edited.ChangeNote != onlyNote {
		t.Fatalf("只改变更说明也应让版本号加一：%+v", edited)
	}
	emptyNote := ""
	edited, err = s.EditSharedStorage("models", SharedStorageChanges{Note: &emptyNote}, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if edited.Version != 4 || edited.ChangeNote != "" || edited.Note != "" {
		t.Fatalf("不带变更说明的修改应清空旧说明：%+v", edited)
	}
	if after, err := s.SnapshotRevision(); err != nil || after != revisionBefore+3 {
		t.Fatalf("三次修改后的快照修订号 = %d，应为 %d：%v", after, revisionBefore+3, err)
	}

	deleted, err := s.DeleteSharedStorage("MODELS", CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if deleted.ID != edited.ID || deleted.Version != 4 {
		t.Fatalf("删除应返回删除前的登记：%+v", deleted)
	}
	if remaining, err := s.ListSharedStorages(SharedStorageFilter{}); err != nil || len(remaining) != 0 {
		t.Fatalf("删除后的共享存储列表应为空：%v %+v", err, remaining)
	}
	if _, err := s.GetSharedStorage("models"); !errors.Is(err, ErrSharedStorageNotFound) {
		t.Fatalf("删除后再查询应报不存在，实际：%v", err)
	}
	rows, err := s.QueryAudit(AuditFilter{OwnerOnly: true, Actions: []string{"share-add", "share-edit", "share-rm"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 {
		t.Fatalf("共享存储增改删应写五条审计行，实际 %d 条：%+v", len(rows), rows)
	}
	for _, row := range rows {
		if strings.Contains(row.Command, "mount-password") || strings.Contains(row.Command, "second-password") {
			t.Fatalf("共享存储审计摘要不能包含挂载密码：%s", row.Command)
		}
	}
	editRows, err := s.QueryAudit(AuditFilter{OwnerOnly: true, Actions: []string{"share-edit"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(editRows) != 3 {
		t.Fatalf("修改审计应记录字段变化与命令行来源：%+v", editRows)
	}
	if after, err := s.SnapshotRevision(); err != nil || after != revisionBefore+4 {
		t.Fatalf("删除共享存储后的快照修订号 = %d，应为 %d：%v", after, revisionBefore+4, err)
	}
}

func TestSharedStorageSecretValueChangeBumpsReferencingShares(t *testing.T) {
	s := seedSharedStorageVault(t)
	_, err := s.AddSharedStorage(SharedStorageInput{
		Name: "datasets", Device: "NAS", User: "mount-user",
		PasswordSecret: "NAS_MOUNT_PASSWORD", Drive: "Z",
	}, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.AddSharedStorage(SharedStorageInput{
		Name: "scratch", Device: "NAS", User: "mount-user",
		PasswordSecret: "NAS_MOUNT_PASSWORD", Drive: "Y",
	}, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	changeNote := "owner 已经更新挂载密码"
	for _, name := range []string{"datasets", "scratch"} {
		if _, err := s.EditSharedStorage(name, SharedStorageChanges{
			ChangeNote: &changeNote,
		}, CLISource()); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.SetSecretValue("NAS_MOUNT_PASSWORD", []byte("second-password"), CLISource()); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"datasets", "scratch"} {
		share, err := s.GetSharedStorage(name)
		if err != nil {
			t.Fatal(err)
		}
		if share.Version != 3 || share.ChangeNote != "" {
			t.Fatalf("机密项换值后共享 %s 的版本号与变更说明不正确：%+v", name, share)
		}
	}
}

func TestSharedStorageProviderNetworkMembershipChangesBumpShares(t *testing.T) {
	s := seedSharedStorageVault(t)
	for _, input := range []SharedStorageInput{
		{Name: "datasets", Device: "NAS", User: "mount-user", PasswordSecret: "NAS_MOUNT_PASSWORD", Drive: "Z"},
		{Name: "scratch", Device: "NAS", User: "mount-user", PasswordSecret: "NAS_MOUNT_PASSWORD", Drive: "Y"},
	} {
		if _, err := s.AddSharedStorage(input, CLISource()); err != nil {
			t.Fatal(err)
		}
	}
	changeNote := "这个说明应随网络成员变化清空"
	for _, name := range []string{"datasets", "scratch"} {
		if _, err := s.EditSharedStorage(name, SharedStorageChanges{
			ChangeNote: &changeNote,
		}, CLISource()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AddDevice("第二NAS", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSecret("SECOND_MOUNT_PASSWORD", "", []byte("second-password"), CLISource()); err != nil {
		t.Fatal(err)
	}
	var err error
	_, err = s.AddSharedStorage(SharedStorageInput{
		Name: "other", Device: "第二NAS", User: "mount-user",
		PasswordSecret: "SECOND_MOUNT_PASSWORD", Drive: "X",
	}, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddNetwork(models.Network{
		Name: "备份局域网", Type: models.NetworkPhysical, SiteName: "办公室",
	}); err != nil {
		t.Fatal(err)
	}

	assertNASVersion := func(want int64) {
		t.Helper()
		for _, name := range []string{"datasets", "scratch"} {
			share, err := s.GetSharedStorage(name)
			if err != nil {
				t.Fatal(err)
			}
			if share.Version != want {
				t.Fatalf("网络成员变化后共享 %s 的版本号 = %d，应为 %d", name, share.Version, want)
			}
			if share.ChangeNote != "" {
				t.Fatalf("网络成员变化后共享 %s 的变更说明应清空：%+v", name, share)
			}
		}
	}
	if _, err := s.AddNetworkMembership("NAS", "备份局域网", "192.0.2.11"); err != nil {
		t.Fatal(err)
	}
	assertNASVersion(3)
	if _, err := s.EditNetworkMembership("NAS", "备份局域网", "192.0.2.12"); err != nil {
		t.Fatal(err)
	}
	assertNASVersion(4)
	if _, err := s.RemoveNetworkMembership("NAS", "备份局域网"); err != nil {
		t.Fatal(err)
	}
	assertNASVersion(5)

	other, err := s.GetSharedStorage("other")
	if err != nil {
		t.Fatal(err)
	}
	if other.Version != 1 {
		t.Fatalf("其他设备的共享存储版本号不应变化：%+v", other)
	}
}

func TestSharedStorageSecretAllowChangesBumpShares(t *testing.T) {
	s := seedSharedStorageVault(t)
	for _, input := range []SharedStorageInput{
		{Name: "datasets", Device: "NAS", User: "mount-user", PasswordSecret: "NAS_MOUNT_PASSWORD", Drive: "Z"},
		{Name: "scratch", Device: "NAS", User: "mount-user", PasswordSecret: "NAS_MOUNT_PASSWORD", Drive: "Y"},
	} {
		if _, err := s.AddSharedStorage(input, CLISource()); err != nil {
			t.Fatal(err)
		}
	}
	changeNote := "owner 已经调整过共享登记"
	for _, name := range []string{"datasets", "scratch"} {
		if _, err := s.EditSharedStorage(name, SharedStorageChanges{
			ChangeNote: &changeNote,
		}, CLISource()); err != nil {
			t.Fatal(err)
		}
	}
	thirdServer, err := s.AddServer(&models.Server{
		Name: "linux-worker", Host: "192.0.2.22", Port: 22,
		User: "deploy", AuthMethod: models.AuthPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertVersion := func(want int64, wantChangeNote string) {
		t.Helper()
		for _, name := range []string{"datasets", "scratch"} {
			share, err := s.GetSharedStorage(name)
			if err != nil {
				t.Fatal(err)
			}
			if share.Version != want || share.ChangeNote != wantChangeNote {
				t.Fatalf("可写入服务器变化后共享 %s 不正确：%+v", name, share)
			}
		}
	}

	if err := s.AllowSecrets(
		[]string{"NAS_MOUNT_PASSWORD"}, []string{"linux-worker"}, CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	assertVersion(3, "")
	if err := s.DisallowSecrets(
		[]string{"NAS_MOUNT_PASSWORD"}, []string{"linux-worker"}, CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	assertVersion(4, "")

	linuxWeb, err := s.GetServerByName("linux-web")
	if err != nil {
		t.Fatal(err)
	}
	preview, err := s.ServerDeletePreview(linuxWeb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteServerCascadingConfirmed(linuxWeb.ID, preview.Digest()); err != nil {
		t.Fatal(err)
	}
	assertVersion(5, "")
	if _, err := s.GetServer(thirdServer); err != nil {
		t.Fatal(err)
	}
}

func TestSharedStorageSecretMetadataChangesDoNotBumpShares(t *testing.T) {
	s := seedSharedStorageVault(t)
	if _, err := s.AddSharedStorage(SharedStorageInput{
		Name: "datasets", Device: "NAS", User: "mount-user",
		PasswordSecret: "NAS_MOUNT_PASSWORD", Drive: "Z",
	}, CLISource()); err != nil {
		t.Fatal(err)
	}
	changeNote := "这个说明应保留"
	if _, err := s.EditSharedStorage("datasets", SharedStorageChanges{
		ChangeNote: &changeNote,
	}, CLISource()); err != nil {
		t.Fatal(err)
	}

	if err := s.EditSecretNote("NAS_MOUNT_PASSWORD", "新备注", CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSecretEnvironmentName(
		"NAS_MOUNT_PASSWORD", "NAS_MOUNT_PASSWORD_NAME", CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`
		UPDATE secrets SET name='RENAMED_MOUNT_PASSWORD', name_lower='renamed_mount_password'
		WHERE name='NAS_MOUNT_PASSWORD'
	`); err != nil {
		t.Fatal(err)
	}

	share, err := s.GetSharedStorage("datasets")
	if err != nil {
		t.Fatal(err)
	}
	if share.Version != 2 || share.ChangeNote != changeNote || share.PasswordSecretName != "RENAMED_MOUNT_PASSWORD" {
		t.Fatalf("只改机密项备注、环境变量名或名字时共享版本号不应变化：%+v", share)
	}
}

func TestSharedStorageLinkedVersionRollsBackWithFailedWrite(t *testing.T) {
	s := seedSharedStorageVault(t)
	if _, err := s.AddSharedStorage(SharedStorageInput{
		Name: "datasets", Device: "NAS", User: "mount-user",
		PasswordSecret: "NAS_MOUNT_PASSWORD", Drive: "Z",
	}, CLISource()); err != nil {
		t.Fatal(err)
	}
	changeNote := "失败回滚后应保留这个说明"
	if _, err := s.EditSharedStorage("datasets", SharedStorageChanges{
		ChangeNote: &changeNote,
	}, CLISource()); err != nil {
		t.Fatal(err)
	}
	revisionBefore, err := s.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`
		CREATE TRIGGER shared_storage_test_reject_secret_set_audit
		BEFORE INSERT ON audit_log WHEN NEW.action='secret-set'
		BEGIN SELECT RAISE(ABORT, 'injected secret audit failure'); END
	`); err != nil {
		t.Fatal(err)
	}

	err = s.SetSecretValue("NAS_MOUNT_PASSWORD", []byte("second-password"), CLISource())
	if err == nil || !strings.Contains(err.Error(), "injected secret audit failure") {
		t.Fatalf("测试应注入机密项写入失败，实际：%v", err)
	}
	share, err := s.GetSharedStorage("datasets")
	if err != nil {
		t.Fatal(err)
	}
	if share.Version != 2 || share.ChangeNote != changeNote {
		t.Fatalf("写入失败回滚后共享版本号与变更说明不正确：%+v", share)
	}
	revisionAfter, err := s.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if revisionAfter != revisionBefore {
		t.Fatalf("写入失败回滚后快照修订号 = %d，应保持 %d", revisionAfter, revisionBefore)
	}
}
