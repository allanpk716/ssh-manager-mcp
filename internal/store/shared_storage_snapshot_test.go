package store

import (
	"encoding/json"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

func TestSharedStorageSnapshotRoundTrip(t *testing.T) {
	source := seedSharedStorageVault(t)
	added, err := source.AddSharedStorage(SharedStorageInput{
		Name: "datasets", Device: "NAS", User: "mount-user",
		PasswordSecret: "NAS_MOUNT_PASSWORD", Drive: "Z",
		Directories: []models.SharedStorageDirectory{
			{Purpose: "临时数据", Path: "tmp"},
			{Purpose: "长期数据", Path: "datasets"},
		},
		Note: "模型数据集",
	}, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	newNote := "模型数据集与临时数据"
	changeNote := "补充用途说明"
	edited, err := source.EditSharedStorage("datasets", SharedStorageChanges{
		Note:       &newNote,
		ChangeNote: &changeNote,
	}, CLISource())
	if err != nil {
		t.Fatal(err)
	}

	snapshot, err := source.ExportSnapshot()
	if err != nil {
		t.Fatalf("全量导出失败：%v", err)
	}
	if len(snapshot.SharedStorages) != 1 {
		t.Fatalf("全量快照应携带共享存储：%+v", snapshot.SharedStorages)
	}
	shared := snapshot.SharedStorages[0]
	if shared.ID != added.ID || shared.Name != "datasets" || shared.DeviceID != added.DeviceID ||
		shared.MountUser != "mount-user" || shared.PasswordSecretID != added.PasswordSecretID ||
		shared.Drive != "Z" || shared.Note != "模型数据集与临时数据" ||
		shared.Version != edited.Version || shared.ChangeNote != "补充用途说明" ||
		len(shared.Directories) != 2 || shared.Directories[0].Purpose != "临时数据" {
		t.Fatalf("共享存储快照字段不完整：%+v", shared)
	}

	target := newTestStore(t)
	if err := target.ImportSnapshot(snapshot); err != nil {
		t.Fatalf("导入含共享存储的快照失败：%v", err)
	}
	restored, err := target.GetSharedStorage("datasets")
	if err != nil {
		t.Fatal(err)
	}
	if restored.ID != added.ID || restored.Version != edited.Version || restored.ChangeNote != "补充用途说明" ||
		restored.DeviceName != "NAS" || restored.PasswordSecretName != "NAS_MOUNT_PASSWORD" ||
		len(restored.Directories) != 2 || len(restored.MountableServers) != 2 ||
		len(restored.CandidateAddresses) != 1 {
		t.Fatalf("导入后的共享存储不完整：%+v", restored)
	}
}

func TestExportSnapshotForProfileScopesSharedStorages(t *testing.T) {
	s := seedSharedStorageVault(t)
	linuxWeb, err := s.GetServerByName("linux-web")
	if err != nil || linuxWeb == nil {
		t.Fatalf("取 linux-web 失败：%v %+v", err, linuxWeb)
	}
	windowsWeb, err := s.GetServerByName("windows-web")
	if err != nil || windowsWeb == nil {
		t.Fatalf("取 windows-web 失败：%v %+v", err, windowsWeb)
	}
	if err := s.AddSecret("OTHER_MOUNT_PASSWORD", "另一台服务器的挂载密码", []byte("other-password"), CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := s.AllowSecrets([]string{"OTHER_MOUNT_PASSWORD"}, []string{"windows-web"}, CLISource()); err != nil {
		t.Fatal(err)
	}
	datasets, err := s.AddSharedStorage(SharedStorageInput{
		Name: "datasets", Device: "NAS", User: "mount-user",
		PasswordSecret: "NAS_MOUNT_PASSWORD", Drive: "Z",
	}, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddSharedStorage(SharedStorageInput{
		Name: "scratch", Device: "NAS", User: "other-user",
		PasswordSecret: "OTHER_MOUNT_PASSWORD", Drive: "Y",
	}, CLISource()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddDevice("文件服务器", "受管的共享存储提供方", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddServerWithCredentialsToDevice(&models.Server{
		Name: "provider-entry", Host: "192.0.2.30", Port: 22, User: "deploy",
	}, nil, nil, "文件服务器"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddNetworkMembership("文件服务器", "办公局域网", "192.0.2.30"); err != nil {
		t.Fatal(err)
	}
	managedShare, err := s.AddSharedStorage(SharedStorageInput{
		Name: "models", Device: "文件服务器", User: "model-user",
		PasswordSecret: "NAS_MOUNT_PASSWORD", Drive: "X",
	}, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	profileID, err := s.AddProfile("共享存储客户端")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.GrantServers(profileID, []string{linuxWeb.ID}); err != nil {
		t.Fatal(err)
	}

	declared, err := s.ExportSnapshotForProfile(profileID, true, "")
	if err != nil {
		t.Fatal(err)
	}
	sharedIDs := make(map[string]bool, len(declared.SharedStorages))
	for _, shared := range declared.SharedStorages {
		sharedIDs[shared.ID] = true
	}
	if len(declared.SharedStorages) != 2 || !sharedIDs[datasets.ID] || !sharedIDs[managedShare.ID] {
		t.Fatalf("客户端快照应只带可挂载范围与授权服务器有交集的共享存储：%+v", declared.SharedStorages)
	}
	if !snapshotHasDevice(declared, "文件服务器") {
		t.Fatalf("客户端快照应携带已下发共享存储的提供方设备：%+v", declared.Devices)
	}
	foundProviderAddress := false
	for _, membership := range declared.NetworkMemberships {
		if membership.DeviceID == managedShare.DeviceID && membership.Address == "192.0.2.30" {
			foundProviderAddress = true
		}
	}
	if !foundProviderAddress {
		t.Fatalf("客户端快照应携带共享存储提供方设备的网络成员：%+v", declared.NetworkMemberships)
	}
	if len(declared.SecretAllows) != 1 || declared.SecretAllows[0].ServerID != linuxWeb.ID {
		t.Fatalf("共享存储依赖的机密项关联应只保留交集内服务器：%+v", declared.SecretAllows)
	}

	undeclared, err := s.ExportSnapshotForProfile(profileID, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(undeclared.SharedStorages) != 2 {
		t.Fatalf("未声明客户端版本也应收到共享存储段：%+v", undeclared.SharedStorages)
	}
	if len(undeclared.Secrets) != 0 || len(undeclared.SecretAllows) != 0 {
		t.Fatalf("未声明客户端版本仍不应收到机密项：%+v %+v", undeclared.Secrets, undeclared.SecretAllows)
	}
	encoded, err := json.Marshal(undeclared)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "mount-password") || strings.Contains(string(encoded), "other-password") {
		t.Fatalf("未声明客户端版本的共享存储段不应携带挂载密码值：%s", encoded)
	}

	full, err := s.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(full.SharedStorages) != 3 {
		t.Fatalf("全量备份不应裁剪共享存储：%+v", full.SharedStorages)
	}
}

func snapshotHasDevice(snapshot *Snapshot, name string) bool {
	for _, device := range snapshot.Devices {
		if device.Name == name {
			return true
		}
	}
	return false
}
