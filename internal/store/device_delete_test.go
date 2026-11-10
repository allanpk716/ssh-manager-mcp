package store

// 设备删除的事务回滚防回归：任何一个级联步骤失败，服务器条目、锚、
// 网络成员、布局坐标与审计都不能留下半删除状态。

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

func TestDeviceDeleteRejectsPreviewChangedAfterConfirmation(t *testing.T) {
	dir := t.TempDir()
	masterKey, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("generate master key: %v", err)
	}
	st, err := Open(filepath.Join(dir, "vault.db"), masterKey)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if _, err := st.AddDevice("工作站", "", ""); err != nil {
		t.Fatal(err)
	}
	first, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: "root", Host: "192.0.2.10", Port: 22, User: "root",
	}, nil, nil, "工作站")
	if err != nil {
		t.Fatal(err)
	}
	confirmed, err := st.DeviceDeletePreview("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: "deploy", Host: "192.0.2.10", Port: 22, User: "deploy",
	}, nil, nil, "工作站"); err != nil {
		t.Fatal(err)
	}

	_, err = st.DeleteDeviceCascading("工作站", confirmed)
	if err == nil || !strings.Contains(err.Error(), "设备删除预览已变化") ||
		!strings.Contains(err.Error(), "请重新执行命令") {
		t.Fatalf("stale preview must abort with rerun guidance, got: %v", err)
	}
	if _, err := st.GetDevice("工作站"); err != nil {
		t.Fatalf("stale preview must keep the device: %v", err)
	}
	for _, name := range []string{"root", "deploy"} {
		if _, err := st.GetServerByName(name); err != nil {
			t.Fatalf("stale preview must keep server %s: %v", name, err)
		}
	}
	if first == nil {
		t.Fatal("test fixture lost the first added server")
	}
}

func TestDeviceDeleteRollsBackWhenLayoutDeleteFails(t *testing.T) {
	dir := t.TempDir()
	masterKey, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("generate master key: %v", err)
	}
	st, err := Open(filepath.Join(dir, "source.db"), masterKey)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	device, err := st.AddDevice("工作站", "", "")
	if err != nil {
		t.Fatal(err)
	}
	added, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: "deploy", Host: "192.0.2.10", Port: 22, User: "deploy",
	}, &models.Credential{Type: models.CredPassword, Secret: []byte("password")}, nil, "工作站")
	if err != nil {
		t.Fatal(err)
	}
	server, err := st.GetServer(added.ServerID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertManualEntryPin(
		server, "", []byte("SHA256:rollback-host-key-fingerprint"),
		PinFormatFingerprint, false, func(old *PinMeta) AuditRow { return AuditRow{Action: "pin-manual"} },
	); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret("rollback_secret", "", []byte("rollback-secret-value"), CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := st.AllowSecrets([]string{"rollback_secret"}, []string{"deploy"}, CLISource()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSite("家A", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{
		Name: "家A局域网", Type: models.NetworkPhysical, SiteName: "家A",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership("工作站", "家A局域网", "192.168.1.20"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(
		`INSERT INTO device_layout_coordinates(view_key,device_id,x,y,created_at,updated_at)
		 VALUES(?,?,?,?,?,?)`, "主视图", device.ID, 1, 2, 1000, 1000,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(
		`CREATE TRIGGER device_delete_layout_fail
		 BEFORE DELETE ON device_layout_coordinates
		 BEGIN SELECT RAISE(ABORT, 'injected layout delete failure'); END`,
	); err != nil {
		t.Fatal(err)
	}

	confirmed, err := st.DeviceDeletePreview("工作站")
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.DeleteDeviceCascading("工作站", confirmed)
	if err == nil || !strings.Contains(err.Error(), "injected layout delete failure") {
		t.Fatalf("device deletion must surface the injected failure, got: %v", err)
	}
	if _, err := st.GetDevice("工作站"); err != nil {
		t.Fatalf("failed deletion must keep the device: %v", err)
	}
	if server, err = st.GetServer(added.ServerID); err != nil || server == nil {
		t.Fatalf("failed deletion must keep the server entry: server=%v err=%v", server, err)
	}
	if pin, err := st.GetEntryPinMeta(added.ServerID); err != nil || pin == nil {
		t.Fatalf("failed deletion must keep the anchor: pin=%v err=%v", pin, err)
	}
	snapshot, err := st.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.NetworkMemberships) != 1 || len(snapshot.DeviceLayoutCoordinates) != 1 ||
		len(snapshot.SecretAllows) != 1 {
		t.Fatalf("failed deletion must keep memberships, layout coordinates, and secret allows: %+v", snapshot)
	}
	for _, action := range []string{"device.rm", "server.rm", "pin-delete", "secret-disallow"} {
		rows, err := st.QueryAudit(AuditFilter{Actions: []string{action}})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 0 {
			t.Fatalf("failed deletion must not leave %s audit rows: %+v", action, rows)
		}
	}
}

func TestDeviceDeleteWritesSharedStorageAuditAndRollsBackTogether(t *testing.T) {
	s := seedSharedStorageVault(t)
	defer s.Close()
	if _, err := s.AddSharedStorage(SharedStorageInput{
		Name: "datasets", Device: "NAS", User: "mount-user",
		PasswordSecret: "NAS_MOUNT_PASSWORD", Drive: "Z",
	}, CLISource()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`
		CREATE TRIGGER shared_storage_test_reject_cascade_audit
		BEFORE INSERT ON audit_log WHEN NEW.action='share-rm'
		BEGIN SELECT RAISE(ABORT, 'injected share audit failure'); END
	`); err != nil {
		t.Fatal(err)
	}
	preview, err := s.DeviceDeletePreview("NAS")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteDeviceCascading("NAS", preview); err == nil ||
		!strings.Contains(err.Error(), "injected share audit failure") {
		t.Fatalf("共享存储审计写入失败应让设备删除整体回滚，实际：%v", err)
	}
	if _, err := s.GetDevice("NAS"); err != nil {
		t.Fatalf("整体回滚后设备应保留：%v", err)
	}
	if _, err := s.GetSharedStorage("datasets"); err != nil {
		t.Fatalf("整体回滚后共享存储应保留：%v", err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER shared_storage_test_reject_cascade_audit`); err != nil {
		t.Fatal(err)
	}
	preview, err = s.DeviceDeletePreview("NAS")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteDeviceCascading("NAS", preview); err != nil {
		t.Fatal(err)
	}
	rows, err := s.QueryAudit(AuditFilter{OwnerOnly: true, Actions: []string{"share-rm"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("设备级联删除共享存储应写一条 share-rm 审计行，实际 %d 条：%+v", len(rows), rows)
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(rows[0].Command), &summary); err != nil {
		t.Fatalf("解析共享存储删除审计失败：%v\n%s", err, rows[0].Command)
	}
	if summary["name"] != "datasets" || summary["reason"] != "随设备删除而删除" {
		t.Fatalf("共享存储级联删除审计摘要不正确：%+v", summary)
	}
}
