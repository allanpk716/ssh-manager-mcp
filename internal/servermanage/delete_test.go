package servermanage

import (
	"errors"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// seedDeleteFixture 建一台待删服务器：独占 sudo 凭据、与 beta 共享登录凭
// 据、已锚定主机密钥、授权给 profile、在某个机密项的可写入服务器列表里、
// 挂在设备下。
func seedDeleteFixture(t *testing.T, m *Manager) (serverID, sharedCredID string) {
	t.Helper()
	keyPEM := genEncryptedKey(t, "pass")
	sharedCredID, err := m.st.SetCredential(&models.Credential{Type: models.CredPrivateKey, Secret: keyPEM})
	if err != nil {
		t.Fatal(err)
	}
	sudoID, err := m.st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("sudo")})
	if err != nil {
		t.Fatal(err)
	}
	added, err := m.st.AddServerWithCredentials(&models.Server{
		Name: "alpha", Host: "192.0.2.1", Port: 22, User: "u", Role: "r",
		Tags: []string{models.TagNeedsPassphrase},
	}, &models.Credential{ID: sharedCredID, Type: models.CredPrivateKey},
		&models.Credential{ID: sudoID, Type: models.CredPassword})
	if err != nil {
		t.Fatal(err)
	}
	serverID = added
	if _, err := m.st.AddServerWithCredentials(&models.Server{
		Name: "beta", Host: "192.0.2.2", Port: 22, User: "u", Role: "r",
	}, &models.Credential{ID: sharedCredID, Type: models.CredPrivateKey}, nil); err != nil {
		t.Fatal(err)
	}
	if err := m.st.AddSecret("deploy_note", "", []byte("secret-value-123"), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := m.st.AllowSecrets([]string{"deploy_note"}, []string{"alpha"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	profileID, err := m.st.AddProfile("运维")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.st.GrantServers(profileID, []string{serverID}); err != nil {
		t.Fatal(err)
	}
	if err := m.st.SaveEntryPin(&models.Server{ID: serverID, Name: "alpha", Host: "192.0.2.1", Port: 22}, []byte("host-key-blob")); err != nil {
		t.Fatal(err)
	}
	return serverID, sharedCredID
}

// TestDeletePreviewGroupsAndConfirmedDelete 校验预览的每一组内容与确认删除
// 的结果：预览变化被拒；共用凭据保留；设备保留；审计动作为 server.delete。
func TestDeletePreviewGroupsAndConfirmedDelete(t *testing.T) {
	m := newTestManager(t)
	serverID, sharedCredID := seedDeleteFixture(t, m)

	preview, err := m.DeletePreview("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if preview.Server.ID != serverID || preview.Server.Name != "alpha" {
		t.Fatalf("预览应按名字解析服务器条目：%+v", preview.Server)
	}
	if !preview.HasDevice || preview.DeviceName == "" {
		t.Fatalf("预览应显示所属设备：%+v", preview)
	}
	if len(preview.Anchors) != 1 {
		t.Fatalf("预览应列出主机密钥锚：%+v", preview.Anchors)
	}
	if len(preview.ProfileNames) != 1 || preview.ProfileNames[0] != "运维" {
		t.Fatalf("预览应列出会失去这台服务器的 profile：%v", preview.ProfileNames)
	}
	if len(preview.SecretNames) != 1 || preview.SecretNames[0] != "deploy_note" {
		t.Fatalf("预览应列出会移出可写入列表的机密项：%v", preview.SecretNames)
	}
	var deleted, kept []CredentialImpact
	for _, impact := range preview.Credentials {
		if impact.Deleted {
			deleted = append(deleted, impact)
		} else {
			kept = append(kept, impact)
		}
	}
	if len(deleted) != 1 || deleted[0].Kind != "sudo" {
		t.Fatalf("独占 sudo 凭据应标为删除：%+v", preview.Credentials)
	}
	if len(kept) != 1 || kept[0].ID != sharedCredID || kept[0].References != 1 {
		t.Fatalf("共用登录凭据应标为保留并注明引用数：%+v", preview.Credentials)
	}

	// 预览之后数据变化：再授权一个 profile，旧摘要必须被拒绝且不删除。
	newProfile, err := m.st.AddProfile("开发")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.st.GrantServers(newProfile, []string{serverID}); err != nil {
		t.Fatal(err)
	}
	fresh, err := m.Delete("alpha", preview.Digest, CLISource())
	if !errors.Is(err, ErrDeletePreviewChanged) {
		t.Fatalf("预览变化应拒绝删除，实际 err=%v preview=%+v", err, fresh)
	}
	if still, _ := m.st.GetServer(serverID); still == nil {
		t.Fatal("预览变化时不得删除服务器条目")
	}
	if fresh == nil || len(fresh.ProfileNames) != 2 {
		t.Fatalf("拒绝时应返回新预览：%+v", fresh)
	}

	if _, err := m.Delete("alpha", fresh.Digest, CLISource()); err != nil {
		t.Fatal(err)
	}
	if still, _ := m.st.GetServer(serverID); still != nil {
		t.Fatal("确认后应删除服务器条目")
	}
	if _, _, found, _ := m.st.CredentialSummary(sharedCredID); !found {
		t.Fatal("共用登录凭据应保留")
	}
	beta, err := m.st.GetServerByName("beta")
	if err != nil || beta == nil || beta.CredentialID != sharedCredID {
		t.Fatalf("beta 应继续引用共用凭据：%+v err=%v", beta, err)
	}
	if device, err := m.st.GetDevice("alpha"); err != nil || device == nil {
		t.Fatalf("所属设备应保留：%+v err=%v", device, err)
	}
	if _, err := m.st.GetEntryPinMeta(serverID); err != nil {
		t.Fatalf("读取锚状态失败：%v", err)
	}
	rows := auditActions(t, m)
	var deleteRow *store.AuditRow
	for i := range rows {
		if rows[i].Action == "server.delete" {
			deleteRow = &rows[i]
		}
	}
	if deleteRow == nil || deleteRow.Source != store.AuditSourceCLI {
		t.Fatalf("应写入来源为命令行的 server.delete 审计：%+v", rows)
	}

	missing, err := m.Delete("no-such", "digest", CLISource())
	if err != nil || missing != nil {
		t.Fatalf("不存在的服务器应安静返回：%+v %v", missing, err)
	}
}

// TestDeletePreviewSeparatesSharedStorageFromSecrets 校验共享存储挂载授权在
// 删除预览里单独成组，摘要覆盖这组影响；确认后服务器不再可挂载。
func TestDeletePreviewSeparatesSharedStorageFromSecrets(t *testing.T) {
	m := newTestManager(t)
	device, err := m.st.AddDevice("NAS", "存放共享数据", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.st.AddSecret("NAS_MOUNT_PASSWORD", "共享存储挂载密码", []byte("mount-secret"), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := m.st.AddSecret("API_TOKEN", "接口令牌", []byte("api-secret"), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	web := mustAddServer(t, m, &models.Server{
		Name: "web", Host: "192.0.2.10", Port: 22, User: "deploy", DeviceID: device.ID,
	})
	if err := m.st.AllowSecrets([]string{"NAS_MOUNT_PASSWORD", "API_TOKEN"}, []string{"web"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	for _, input := range []store.SharedStorageInput{
		{Name: "datasets", Device: "NAS", User: "mount-user", PasswordSecret: "NAS_MOUNT_PASSWORD", Drive: "Z"},
		{Name: "scratch", Device: "NAS", User: "mount-user", PasswordSecret: "NAS_MOUNT_PASSWORD", Drive: "Y"},
	} {
		if _, err := m.st.AddSharedStorage(input, store.CLISource()); err != nil {
			t.Fatal(err)
		}
	}

	preview, err := m.DeletePreview("web")
	if err != nil || preview == nil {
		t.Fatalf("读取删除预览失败：preview=%+v err=%v", preview, err)
	}
	if len(preview.SharedStorageNames) != 2 ||
		preview.SharedStorageNames[0] != "datasets" ||
		preview.SharedStorageNames[1] != "scratch" {
		t.Fatalf("删除预览应逐项列出共享存储：%v", preview.SharedStorageNames)
	}
	if len(preview.SecretNames) != 1 || preview.SecretNames[0] != "API_TOKEN" {
		t.Fatalf("共享存储挂载密码不得重复出现在普通机密项组：%v", preview.SecretNames)
	}

	if _, err := m.st.AddSharedStorage(store.SharedStorageInput{
		Name: "models", Device: "NAS", User: "mount-user",
		PasswordSecret: "NAS_MOUNT_PASSWORD", Drive: "X",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	fresh, err := m.Delete("web", preview.Digest, CLISource())
	if !errors.Is(err, ErrDeletePreviewChanged) {
		t.Fatalf("共享存储影响变化后应整体拒绝删除，实际 err=%v preview=%+v", err, fresh)
	}
	if fresh == nil || len(fresh.SharedStorageNames) != 3 {
		t.Fatalf("拒绝删除时应返回包含最新共享存储的预览：%+v", fresh)
	}
	if still, _ := m.st.GetServer(web); still == nil {
		t.Fatal("预览变化时不得删除服务器条目")
	}

	if _, err := m.Delete("web", fresh.Digest, CLISource()); err != nil {
		t.Fatal(err)
	}
	shares, err := m.st.ListSharedStorages(store.SharedStorageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, share := range shares {
		for _, server := range share.MountableServers {
			if server.ID == web {
				t.Fatalf("确认删除后共享 %s 不应再可挂载到 web：%+v", share.Name, share.MountableServers)
			}
		}
	}
}
