package store

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRevokeSharedStorageMountAndPasswordChangesValueOnce(t *testing.T) {
	s := seedSharedStorageVault(t)
	added, err := s.AddSharedStorage(SharedStorageInput{
		Name: "datasets", Device: "NAS", User: "mount-user",
		PasswordSecret: "NAS_MOUNT_PASSWORD", Drive: "Z",
	}, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	revisionBefore, err := s.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}

	revoked, err := s.RevokeSharedStorageMountAndPassword(
		added.ID, linuxSharedStorageTestServerID(t, s), false, false,
		[]byte("second-mount-password"), CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if revoked.Version != added.Version+1 {
		t.Fatalf("撤销加换密码后的共享存储版本号 = %d，期待 %d", revoked.Version, added.Version+1)
	}
	value, err := s.SecretValueForSharedStorageMount(added.PasswordSecretID)
	if err != nil {
		t.Fatal(err)
	}
	if string(value.Value) != "second-mount-password" {
		t.Fatal("撤销加换密码后应能读到新的挂载密码")
	}
	if len(revoked.MountableServers) != 1 || revoked.MountableServers[0].Name != "windows-web" {
		t.Fatalf("撤销后的可挂载服务器应只剩 windows-web：%+v", revoked.MountableServers)
	}
	revisionAfter, err := s.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if revisionAfter <= revisionBefore {
		t.Fatalf("快照修订号未前进：before=%d after=%d", revisionBefore, revisionAfter)
	}

	rows, err := s.QueryAudit(AuditFilter{OwnerOnly: true, Actions: []string{"share-revoke"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("共享撤销 owner 审计行数 = %d", len(rows))
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(rows[0].Command), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["password_changed"] != true || summary["version"] != float64(revoked.Version) {
		t.Fatalf("撤销加换密码审计摘要应记录换值与最终版本号：%#v", summary)
	}
	if strings.Contains(rows[0].Command, "second-mount-password") {
		t.Fatalf("共享撤销审计泄露新挂载密码：%s", rows[0].Command)
	}
}

func TestRevokeSharedStorageMountAndPasswordRollsBackTogether(t *testing.T) {
	s := seedSharedStorageVault(t)
	added, err := s.AddSharedStorage(SharedStorageInput{
		Name: "datasets", Device: "NAS", User: "mount-user",
		PasswordSecret: "NAS_MOUNT_PASSWORD", Drive: "Z",
	}, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	revisionBefore, err := s.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`
		CREATE TRIGGER shared_storage_test_reject_revoke_disallow_audit
		BEFORE INSERT ON audit_log WHEN NEW.action='secret-disallow'
		BEGIN SELECT RAISE(ABORT, 'injected revoke audit failure'); END
	`); err != nil {
		t.Fatal(err)
	}
	serverID := linuxSharedStorageTestServerID(t, s)

	_, err = s.RevokeSharedStorageMountAndPassword(
		added.ID, serverID, false, false, []byte("second-mount-password"), CLISource())
	if err == nil || !strings.Contains(err.Error(), "injected revoke audit failure") {
		t.Fatalf("测试应注入审计写入失败，实际：%v", err)
	}
	if strings.Contains(err.Error(), "second-mount-password") {
		t.Fatalf("错误文本泄露新挂载密码：%s", err)
	}
	share, err := s.GetSharedStorage("datasets")
	if err != nil {
		t.Fatal(err)
	}
	value, err := s.SecretValueForSharedStorageMount(added.PasswordSecretID)
	if err != nil {
		t.Fatal(err)
	}
	if share.Version != added.Version || len(share.MountableServers) != 2 ||
		string(value.Value) != "mount-password" {
		t.Fatalf("事务失败后应同时回滚换值与移出范围：%+v %q", share, value.Value)
	}
	revisionAfter, err := s.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if revisionAfter != revisionBefore {
		t.Fatalf("事务失败后快照修订号应保持 %d，实际 %d", revisionBefore, revisionAfter)
	}
}

func linuxSharedStorageTestServerID(t *testing.T, s *Store) string {
	t.Helper()
	server, err := s.GetServerByName("linux-web")
	if err != nil || server == nil {
		t.Fatalf("取 linux-web 失败：%v %+v", err, server)
	}
	return server.ID
}
