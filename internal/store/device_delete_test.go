package store

// 设备删除的事务回滚防回归：任何一个级联步骤失败，服务器条目、锚、
// 网络成员、布局坐标与审计都不能留下半删除状态。

import (
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

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
	if err := st.AddSecret("rollback_secret", "", []byte("rollback-secret-value")); err != nil {
		t.Fatal(err)
	}
	if err := st.AllowSecrets([]string{"rollback_secret"}, []string{"deploy"}); err != nil {
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

	_, err = st.DeleteDeviceCascading("工作站")
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
