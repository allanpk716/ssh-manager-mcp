package store

import (
	"bytes"
	"encoding/json"
	"testing"
)

// TestExportSnapshotForProfile_ScopesSecrets 覆盖机密项随客户端快照下发的
// 裁剪矩阵：只保留与该 profile 授权服务器有交集的机密项，关联行也只保留
// 交集内服务器；未声明客户端版本时两段为空，全量导出不受影响。
func TestExportSnapshotForProfile_ScopesSecrets(t *testing.T) {
	s := newTestStore(t)
	profileA := seedScopedVault(t, s)
	serverA, err := s.GetServerByName("srvA")
	if err != nil || serverA == nil {
		t.Fatalf("取 profileA 授权的 srvA 失败：%v %+v", err, serverA)
	}
	serverB, err := s.GetServerByName("srvB")
	if err != nil || serverB == nil {
		t.Fatalf("取 profileB 授权的 srvB 失败：%v %+v", err, serverB)
	}

	if err := s.AddSecret("SHARED_TOKEN", "两个 profile 都能写", []byte("current-secret-value")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSecretValue("SHARED_TOKEN", []byte("newer-secret-value")); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSecret("OTHER_PROFILE", "只有另一个 profile 能写", []byte("other-secret-value")); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSecret("EMPTY_LIST", "没有可写入服务器", []byte("unused-secret-value")); err != nil {
		t.Fatal(err)
	}
	if err := s.AllowSecrets([]string{"SHARED_TOKEN"}, []string{"srvA", "srvB"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AllowSecrets([]string{"OTHER_PROFILE"}, []string{"srvB"}); err != nil {
		t.Fatal(err)
	}

	declared, err := s.ExportSnapshotForProfile(profileA, true, "")
	if err != nil {
		t.Fatalf("导出声明版本的客户端快照失败：%v", err)
	}
	if len(declared.Secrets) != 1 || declared.Secrets[0].Name != "SHARED_TOKEN" {
		t.Fatalf("声明版本的客户端快照应只带与 profileA 有交集的机密项，实际：%+v", declared.Secrets)
	}
	secret := declared.Secrets[0]
	if secret.Note != "两个 profile 都能写" ||
		string(secret.Value) != "newer-secret-value" ||
		string(secret.PreviousValue) != "current-secret-value" ||
		secret.ValueVersion != 2 ||
		secret.CreatedAt == 0 || secret.ValueUpdatedAt == 0 {
		t.Fatalf("客户端快照中的机密项字段不完整：%+v", secret)
	}
	if len(declared.SecretAllows) != 1 ||
		declared.SecretAllows[0].SecretID != secret.ID ||
		declared.SecretAllows[0].ServerID != serverA.ID {
		t.Fatalf("客户端快照的机密项关联应只保留 profileA 授权的服务器，实际：%+v", declared.SecretAllows)
	}
	encoded, err := json.Marshal(declared)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("srvB")) {
		t.Fatalf("客户端快照泄露了范围外服务器名 srvB：%s", encoded)
	}

	undeclared, err := s.ExportSnapshotForProfile(profileA, false, "")
	if err != nil {
		t.Fatalf("导出未声明版本的客户端快照失败：%v", err)
	}
	if len(undeclared.Secrets) != 0 || len(undeclared.SecretAllows) != 0 {
		t.Fatalf("未声明版本的旧客户端不应收到机密项：%+v %+v", undeclared.Secrets, undeclared.SecretAllows)
	}

	full, err := s.ExportSnapshot()
	if err != nil {
		t.Fatalf("全量导出失败：%v", err)
	}
	if len(full.Secrets) != 3 || len(full.SecretAllows) != 3 {
		t.Fatalf("全量导出应保留全部机密项与关联，实际：%d/%d", len(full.Secrets), len(full.SecretAllows))
	}
}

// TestListSecretsInScope 验证机密项可见列表的共用服务器范围
// 判定：「全部服务器」返回所有关联，「指定服务器范围」只返回交集非空的
// 机密项并裁剪关联；删除服务器条目后关联随之消失。
func TestListSecretsInScope(t *testing.T) {
	s := newTestStore(t)
	seedScopedVault(t, s)
	serverA, err := s.GetServerByName("srvA")
	if err != nil || serverA == nil {
		t.Fatalf("取 srvA 失败：%v %+v", err, serverA)
	}
	serverB, err := s.GetServerByName("srvB")
	if err != nil || serverB == nil {
		t.Fatalf("取 srvB 失败：%v %+v", err, serverB)
	}
	if err := s.AddSecret("SHARED_TOKEN", "共享", []byte("shared-secret-value")); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSecret("A_ONLY_TOKEN", "仅 srvA", []byte("a-only-secret-value")); err != nil {
		t.Fatal(err)
	}
	if err := s.AllowSecrets([]string{"SHARED_TOKEN"}, []string{"srvA", "srvB"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AllowSecrets([]string{"A_ONLY_TOKEN"}, []string{"srvA"}); err != nil {
		t.Fatal(err)
	}

	allViews, err := s.ListSecretsInScope(ScopeAllServers())
	if err != nil {
		t.Fatal(err)
	}
	if names := secretViewNames(allViews); len(names) != 2 ||
		names[0] != "A_ONLY_TOKEN" || names[1] != "SHARED_TOKEN" {
		t.Fatalf("全部服务器范围应列出全部机密项，实际：%v", allViews)
	}
	if servers := allViews[0].AllowedServers; len(servers) != 1 || servers[0] != "srvA" {
		t.Fatalf("A_ONLY_TOKEN 应只能写 srvA，实际：%v", servers)
	}
	if servers := allViews[1].AllowedServers; len(servers) != 2 || servers[0] != "srvA" || servers[1] != "srvB" {
		t.Fatalf("SHARED_TOKEN 应能写 srvA 与 srvB，实际：%v", servers)
	}

	profileAViews, err := s.ListSecretsInScope(ScopeToServers(serverA.ID))
	if err != nil {
		t.Fatal(err)
	}
	if len(profileAViews) != 2 {
		t.Fatalf("只授权 srvA 时两个机密项都应可见，实际：%+v", profileAViews)
	}
	for _, view := range profileAViews {
		if len(view.AllowedServers) != 1 || view.AllowedServers[0] != "srvA" {
			t.Fatalf("指定服务器范围外的不应出现在可写入列表：%+v", view)
		}
	}

	profileBViews, err := s.ListSecretsInScope(ScopeToServers(serverB.ID))
	if err != nil {
		t.Fatal(err)
	}
	if len(profileBViews) != 1 || profileBViews[0].Name != "SHARED_TOKEN" ||
		len(profileBViews[0].AllowedServers) != 1 || profileBViews[0].AllowedServers[0] != "srvB" {
		t.Fatalf("只授权 srvB 时应仅剩 SHARED_TOKEN/srvB，实际：%+v", profileBViews)
	}

	if err := s.DeleteServerCascading(serverA.ID); err != nil {
		t.Fatal(err)
	}
	afterDelete, err := s.ListSecretsInScope(ScopeAllServers())
	if err != nil {
		t.Fatal(err)
	}
	if len(afterDelete) != 2 {
		t.Fatalf("删除服务器条目不应删除机密项本身，实际：%+v", afterDelete)
	}
	if view := afterDelete[0]; view.Name != "A_ONLY_TOKEN" || len(view.AllowedServers) != 0 {
		t.Fatalf("删除 srvA 后它的关联应消失，实际：%+v", view)
	}
	if view := afterDelete[1]; view.Name != "SHARED_TOKEN" ||
		len(view.AllowedServers) != 1 || view.AllowedServers[0] != "srvB" {
		t.Fatalf("删除 srvA 后 SHARED_TOKEN 应只剩 srvB，实际：%+v", view)
	}
}

// TestExportSnapshotForProfile_SecretsFollowRevokedGrant 覆盖 profile 撤销
// 服务器授权后的下一次快照：只因这台服务器才下发的机密项随之消失。
func TestExportSnapshotForProfile_SecretsFollowRevokedGrant(t *testing.T) {
	s := newTestStore(t)
	profileA := seedScopedVault(t, s)
	if err := s.AddSecret("A_ONLY_TOKEN", "仅授权内服务器", []byte("a-only-secret-value")); err != nil {
		t.Fatal(err)
	}
	if err := s.AllowSecrets([]string{"A_ONLY_TOKEN"}, []string{"srvA"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SyncServers(profileA, nil); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.ExportSnapshotForProfile(profileA, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Secrets) != 0 || len(snapshot.SecretAllows) != 0 {
		t.Fatalf("撤销全部授权后客户端快照不应再带机密项：%+v %+v",
			snapshot.Secrets, snapshot.SecretAllows)
	}
}

// TestExportSnapshotForProfile_SecretsFollowServerDeletion 覆盖删除服务器条
// 目后的下一次客户端快照：只因该条目才下发的机密项消失，仍下发机密项的关
// 联行随之裁剪；全量导出（备份命令的底层导出）不受这次裁剪影响。
func TestExportSnapshotForProfile_SecretsFollowServerDeletion(t *testing.T) {
	s := newTestStore(t)
	profileA := seedScopedVault(t, s)
	serverA, err := s.GetServerByName("srvA")
	if err != nil || serverA == nil {
		t.Fatalf("取 srvA 失败：%v %+v", err, serverA)
	}
	serverNoCred, err := s.GetServerByName("srvNoCred")
	if err != nil || serverNoCred == nil {
		t.Fatalf("取 srvNoCred 失败：%v %+v", err, serverNoCred)
	}
	if err := s.AddSecret("A_ONLY_TOKEN", "只因 srvA 下发", []byte("a-only-secret-value")); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSecret("SHARED_TOKEN", "删掉 srvA 后仍有交集", []byte("shared-secret-value")); err != nil {
		t.Fatal(err)
	}
	if err := s.AllowSecrets([]string{"A_ONLY_TOKEN"}, []string{"srvA"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AllowSecrets([]string{"SHARED_TOKEN"}, []string{"srvA", "srvNoCred"}); err != nil {
		t.Fatal(err)
	}

	before, err := s.ExportSnapshotForProfile(profileA, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Secrets) != 2 || len(before.SecretAllows) != 3 {
		t.Fatalf("删除前客户端快照应带两个机密项与三条关联，实际：%+v %+v",
			before.Secrets, before.SecretAllows)
	}

	if err := s.DeleteServerCascading(serverA.ID); err != nil {
		t.Fatal(err)
	}
	after, err := s.ExportSnapshotForProfile(profileA, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Secrets) != 1 || after.Secrets[0].Name != "SHARED_TOKEN" {
		t.Fatalf("删除 srvA 后只因 srvA 下发的机密项应消失，实际：%+v", after.Secrets)
	}
	if len(after.SecretAllows) != 1 ||
		after.SecretAllows[0].SecretID != after.Secrets[0].ID ||
		after.SecretAllows[0].ServerID != serverNoCred.ID {
		t.Fatalf("删除 srvA 后仍下发机密项的关联应只剩 srvNoCred，实际：%+v", after.SecretAllows)
	}

	full, err := s.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Secrets) != 2 || len(full.SecretAllows) != 1 {
		t.Fatalf("备份的底层全量导出应保留全部机密项与未删除的关联，实际：%+v %+v",
			full.Secrets, full.SecretAllows)
	}
	if full.SecretAllows[0].ServerID != serverNoCred.ID {
		t.Fatalf("全量导出不应再带已删除服务器的关联，实际：%+v", full.SecretAllows)
	}
}

// TestSecretViewsFromSnapshotSkipsDanglingAllows 钉住快照读取的防御：关联行
// 引用了快照里不存在的机密项或服务器条目时跳过该行，不生成名字为空的
// 机密项条目，也不把不存在的服务器算进可写入列表。
func TestSecretViewsFromSnapshotSkipsDanglingAllows(t *testing.T) {
	snapshot := &Snapshot{
		Secrets: []SnapshotSecret{{
			ID: "secret-1", Name: "KNOWN_TOKEN", CreatedAt: 1, ValueUpdatedAt: 2,
		}},
		Servers: []SnapshotServer{{ID: "server-1", Name: "srv1"}},
		SecretAllows: []SnapshotSecretAllow{
			{SecretID: "ghost-secret", ServerID: "server-1"},
			{SecretID: "secret-1", ServerID: "ghost-server"},
			{SecretID: "secret-1", ServerID: "server-1"},
		},
	}
	views := SecretViewsFromSnapshot(snapshot)
	if len(views) != 1 || views[0].Name != "KNOWN_TOKEN" {
		t.Fatalf("悬空关联不应生成额外或空名字的机密项条目，实际：%+v", views)
	}
	if servers := views[0].AllowedServers; len(servers) != 1 || servers[0] != "srv1" {
		t.Fatalf("悬空关联不应进入可写入列表，实际：%v", servers)
	}
}

func secretViewNames(views []SecretView) []string {
	names := make([]string, 0, len(views))
	for _, view := range views {
		names = append(names, view.Name)
	}
	return names
}
