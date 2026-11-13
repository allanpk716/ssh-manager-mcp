package servermanage

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// TestDetailReverseViewSummarizesAuthorizations 校验服务器详情以单个服务
// 器条目为中心汇总反向授权：共享存储密码机密项只归共享存储组，一个密码
// 机密项驱动的多个共享逐项展示，同设备兄弟条目只进入只读提示。
func TestDetailReverseViewSummarizesAuthorizations(t *testing.T) {
	m := newTestManager(t)
	device, err := m.st.AddDevice("gpu-workstation", "同一台设备", "")
	if err != nil {
		t.Fatal(err)
	}
	primary := mustAddServer(t, m, &models.Server{
		Name: "web", Host: "192.0.2.10", Port: 22, User: "deploy", DeviceID: device.ID,
	})
	sibling := mustAddServer(t, m, &models.Server{
		Name: "worker", Host: "192.0.2.11", Port: 22, User: "deploy", DeviceID: device.ID,
		ShellType: models.ShellPowerShell,
	})

	if err := m.st.AddSecretWithEnvironmentName(
		"API_TOKEN", "接口令牌", []byte("ordinary-secret-value"), "API_TOKEN", store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	if err := m.st.AddSecret(
		"DATASET_PASSWORD", "数据集挂载密码", []byte("share-secret-value"), store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	if err := m.st.AddSecret(
		"WORKER_PASSWORD", "工作机挂载密码", []byte("worker-secret-value"), store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	if err := m.st.AllowSecrets(
		[]string{"API_TOKEN", "DATASET_PASSWORD"}, []string{"web"}, store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	if err := m.st.AllowSecrets([]string{"WORKER_PASSWORD"}, []string{"worker"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	for i, share := range []string{"datasets", "models"} {
		drive := "Z"
		if i == 1 {
			drive = "Y"
		}
		if _, err := m.st.AddSharedStorage(store.SharedStorageInput{
			Name: share, Device: "gpu-workstation", User: "mount-user",
			PasswordSecret: "DATASET_PASSWORD", Drive: drive,
		}, store.CLISource()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.st.AddSharedStorage(store.SharedStorageInput{
		Name: "worker-data", Device: "gpu-workstation", User: "mount-user",
		PasswordSecret: "WORKER_PASSWORD", Drive: "X",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}

	opsProfile, err := m.st.AddProfile("运维")
	if err != nil {
		t.Fatal(err)
	}
	agentProfile, err := m.st.AddProfile("agent")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.st.GrantServers(opsProfile, []string{primary}); err != nil {
		t.Fatal(err)
	}
	if err := m.st.GrantServers(agentProfile, []string{sibling}); err != nil {
		t.Fatal(err)
	}

	before, err := m.st.QueryAudit(store.AuditFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	detail, err := m.Detail(primary)
	if err != nil || detail == nil {
		t.Fatalf("读取服务器详情失败：detail=%+v err=%v", detail, err)
	}

	if len(detail.Secrets) != 1 || detail.Secrets[0].Name != "API_TOKEN" ||
		detail.Secrets[0].Note != "接口令牌" || detail.Secrets[0].EnvironmentName != "API_TOKEN" {
		t.Fatalf("机密项组应只含普通机密项：%+v", detail.Secrets)
	}
	if detail.Secrets[0].ID == "" {
		t.Fatalf("机密项组应保留标识符，供后续授权编辑使用：%+v", detail.Secrets[0])
	}
	if len(detail.SharedStorages) != 2 {
		t.Fatalf("同一个密码机密项驱动的共享存储应逐项列出：%+v", detail.SharedStorages)
	}
	for i, want := range []string{"datasets", "models"} {
		share := detail.SharedStorages[i]
		if share.Name != want || share.ProviderDeviceName != "gpu-workstation" ||
			share.PasswordSecretName != "DATASET_PASSWORD" || share.MountUser != "mount-user" ||
			share.MountPoint != "/mnt/sshmgr/"+want {
			t.Fatalf("共享存储组第 %d 行不正确：%+v", i, share)
		}
		if share.ID == "" || share.PasswordSecretID == "" {
			t.Fatalf("共享存储组应保留共享与密码机密项标识符：%+v", share)
		}
	}
	if len(detail.Profiles) != 1 || detail.Profiles[0].ID != opsProfile || detail.Profiles[0].Name != "运维" {
		t.Fatalf("profile 授权组不正确：%+v", detail.Profiles)
	}
	if len(detail.SiblingSharedStorages) != 1 ||
		detail.SiblingSharedStorages[0].Name != "worker-data" ||
		detail.SiblingSharedStorages[0].SourceServerName != "worker" ||
		detail.SiblingSharedStorages[0].MountPoint != "X:" {
		t.Fatalf("同设备共享存储提示不正确：%+v", detail.SiblingSharedStorages)
	}

	siblingDetail, err := m.Detail(sibling)
	if err != nil || siblingDetail == nil {
		t.Fatalf("读取兄弟条目详情失败：detail=%+v err=%v", siblingDetail, err)
	}
	if len(siblingDetail.Secrets) != 0 {
		t.Fatalf("兄弟条目的机密项组应独立：%+v", siblingDetail.Secrets)
	}
	if len(siblingDetail.SharedStorages) != 1 || siblingDetail.SharedStorages[0].Name != "worker-data" {
		t.Fatalf("兄弟条目的共享存储组应独立：%+v", siblingDetail.SharedStorages)
	}
	if len(siblingDetail.Profiles) != 1 || siblingDetail.Profiles[0].Name != "agent" {
		t.Fatalf("兄弟条目的 profile 授权组应独立：%+v", siblingDetail.Profiles)
	}
	if len(siblingDetail.SiblingSharedStorages) != 2 {
		t.Fatalf("兄弟条目应提示主条目带来的两个共享存储：%+v", siblingDetail.SiblingSharedStorages)
	}

	after, err := m.st.QueryAudit(store.AuditFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("读取服务器详情不应写审计：before=%d after=%d", len(before), len(after))
	}
}

// TestDetailReverseViewIncludesDeliveryRecords 校验服务器详情第四组只读消
// 费既有的下发记录查询：同路径只保留最新一条，兄弟条目的记录只进入同设备
// 可见性提示。
func TestDetailReverseViewIncludesDeliveryRecords(t *testing.T) {
	m := newTestManager(t)
	device, err := m.st.AddDevice("workstation", "同一台设备", "")
	if err != nil {
		t.Fatal(err)
	}
	primary := mustAddServer(t, m, &models.Server{
		Name: "web", Host: "192.0.2.10", Port: 22, User: "deploy", DeviceID: device.ID,
	})
	sibling := mustAddServer(t, m, &models.Server{
		Name: "worker", Host: "192.0.2.11", Port: 22, User: "deploy", DeviceID: device.ID,
	})
	if err := m.st.AddSecret(
		"API_TOKEN", "接口令牌", []byte("ordinary-secret-value"), store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	if err := m.st.AddSecret(
		"OLD_TOKEN", "已下线令牌", []byte("deleted-secret-value"), store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	if err := m.st.AllowSecrets(
		[]string{"API_TOKEN", "OLD_TOKEN"}, []string{"web", "worker"}, store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	profileID, err := m.st.AddProfile("agents")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.st.GrantServers(profileID, []string{primary, sibling}); err != nil {
		t.Fatal(err)
	}
	record := func(serverID, path, secretName, digest string, writtenAt int64, version int64) store.SecretDeliveryRecordInput {
		return store.SecretDeliveryRecordInput{
			ServerID: serverID, Path: path,
			Template:   []byte("token: {{sshmgr-secret:" + secretName + "}}"),
			FileSHA256: digest, WrittenAt: time.Unix(writtenAt, 0),
			Secrets:              []store.SecretDeliverySecret{{Name: secretName, ValueVersion: version}},
			InitiatingDeviceName: "office-laptop", InitiatingInstanceName: "agent-a",
			AuthorizationProfileID: profileID,
		}
	}
	if _, err := m.st.RecordSecretDelivery(record(
		primary, "/etc/app/config.yml", "API_TOKEN", strings.Repeat("4", 64), 1760000800, 1,
	), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.st.RecordSecretDelivery(record(
		primary, "/etc/app/config.yml", "API_TOKEN", strings.Repeat("5", 64), 1760000900, 2,
	), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.st.RecordSecretDelivery(record(
		primary, "/etc/app/old.yml", "OLD_TOKEN", strings.Repeat("6", 64), 1760000700, 1,
	), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	workerRecord := record(
		sibling, "/etc/worker/config.yml", "API_TOKEN", strings.Repeat("7", 64), 1760001000, 2,
	)
	workerRecord.InitiatingDeviceName = "worker-laptop"
	workerRecord.InitiatingInstanceName = "agent-b"
	if _, err := m.st.RecordSecretDelivery(workerRecord, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := m.st.DeleteSecret("OLD_TOKEN", store.CLISource()); err != nil {
		t.Fatal(err)
	}

	before, err := m.st.QueryAudit(store.AuditFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	detail, err := m.Detail(primary)
	if err != nil || detail == nil {
		t.Fatalf("读取服务器详情失败：detail=%+v err=%v", detail, err)
	}
	want, err := m.st.SecretDeliveryRecords(store.SecretDeliveryRecordFilter{ServerID: primary})
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != 2 {
		t.Fatalf("测试准备应留下两条最新记录：%+v", want)
	}
	if !reflect.DeepEqual(detail.DeliveryRecords, want) {
		t.Fatalf("详情下发记录应与按服务器查询结果一致：\nwant %+v\ngot  %+v", want, detail.DeliveryRecords)
	}
	if len(detail.DeliveryRecords) != 2 ||
		detail.DeliveryRecords[0].Path != "/etc/app/config.yml" ||
		detail.DeliveryRecords[0].WrittenAt.Unix() != 1760000900 ||
		detail.DeliveryRecords[0].FileSHA256 != strings.Repeat("5", 64) ||
		detail.DeliveryRecords[0].Secrets[0].ValueVersion != 2 {
		t.Fatalf("同一路径应只保留最新下发记录：%+v", detail.DeliveryRecords)
	}
	if len(detail.DeliveryRecords) < 2 || !detail.DeliveryRecords[1].Secrets[0].Deleted {
		t.Fatalf("已删除机密项的记录应保留并带删除标注：%+v", detail.DeliveryRecords)
	}
	if len(detail.SiblingDeliveryRecords) != 1 ||
		detail.SiblingDeliveryRecords[0].Path != "/etc/worker/config.yml" ||
		detail.SiblingDeliveryRecords[0].ServerName != "worker" ||
		detail.SiblingDeliveryRecords[0].InitiatingDeviceName != "worker-laptop" ||
		detail.SiblingDeliveryRecords[0].InitiatingInstanceName != "agent-b" {
		t.Fatalf("同设备下发记录提示不正确：%+v", detail.SiblingDeliveryRecords)
	}
	after, err := m.st.QueryAudit(store.AuditFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("读取下发记录组不应写审计：before=%d after=%d", len(before), len(after))
	}
}
