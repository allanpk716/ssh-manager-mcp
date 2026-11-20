package webuiservers

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/servermanage"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

func newAdapterManager(t *testing.T) (*servermanage.Manager, *store.Store) {
	t.Helper()
	key, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return servermanage.New(st), st
}

// TestListEntriesCarryConnectionTestResults 用真实 vault 验证列表适配：
// 最近一次连接测试结果随列表一次换成页面数据，从未测试的条目保持为空。
func TestListEntriesCarryConnectionTestResults(t *testing.T) {
	manager, st := newAdapterManager(t)
	tested, err := st.AddServer(&models.Server{Name: "web", Host: "192.0.2.10", Port: 22})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddServer(&models.Server{Name: "new", Host: "192.0.2.11", Port: 22}); err != nil {
		t.Fatal(err)
	}
	saved := models.ServerConnectionTestResult{
		CompletedAt: time.Unix(1_782_000_000, 0), Succeeded: true,
		SuccessAddress: "192.0.2.10", SuccessNetworkName: "办公网",
		SuccessSource: models.DialAddressSourcePhysical, Duration: 120 * time.Millisecond,
	}
	if err := st.SaveServerConnectionTestResult(tested, saved); err != nil {
		t.Fatal(err)
	}

	rows, err := FromManager(manager).ServerList("", false)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]webui.ServerListEntry{}
	for _, row := range rows {
		byName[row.Name] = row
	}
	got := byName["web"].ConnectionTest
	if got == nil || !got.Succeeded || got.SuccessAddress != "192.0.2.10" ||
		got.SuccessNetworkName != "办公网" || got.Duration != 120*time.Millisecond {
		t.Fatalf("web 的页面连接测试结果不正确：%+v", got)
	}
	if byName["new"].ConnectionTest != nil {
		t.Fatalf("从未测试的 new 不应有页面结果：%+v", byName["new"].ConnectionTest)
	}
}

// TestDetailViewIncludesDeliveryRecords 用真实 vault 验证 Web UI 的服务器
// 详情数据完整映射第四组下发记录，并给客户端裁剪留下明确的显示开关。
func TestDetailViewIncludesDeliveryRecords(t *testing.T) {
	manager, st := newAdapterManager(t)
	device, err := st.AddDevice("workstation", "", "")
	if err != nil {
		t.Fatal(err)
	}
	primary, err := st.AddServer(&models.Server{
		Name: "web", Host: "192.0.2.10", Port: 22, User: "deploy", DeviceID: device.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := st.AddServer(&models.Server{
		Name: "worker", Host: "192.0.2.11", Port: 22, User: "deploy", DeviceID: device.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret(
		"OLD_TOKEN", "已下线令牌", []byte("deleted-secret-value"), store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	if err := st.AllowSecrets([]string{"OLD_TOKEN"}, []string{"web", "worker"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	profileID, err := st.AddProfile("agents")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{primary, worker}); err != nil {
		t.Fatal(err)
	}
	record := func(serverID, path, instance string, writtenAt int64) store.SecretDeliveryRecordInput {
		return store.SecretDeliveryRecordInput{
			ServerID: serverID, Path: path,
			Template:   []byte("token: {{sshmgr-secret:OLD_TOKEN}}"),
			FileSHA256: strings.Repeat("5", 64), WrittenAt: time.Unix(writtenAt, 0),
			Secrets:              []store.SecretDeliverySecret{{Name: "OLD_TOKEN", ValueVersion: 2}},
			InitiatingDeviceName: "office-laptop", InitiatingInstanceName: instance,
			AuthorizationProfileID: profileID,
		}
	}
	if _, err := st.RecordSecretDelivery(
		record(primary, "/etc/app/config.yml", "agent-a", 1760000900), store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordSecretDelivery(
		record(worker, "/etc/worker/config.yml", "agent-b", 1760001000), store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteSecret("OLD_TOKEN", store.CLISource()); err != nil {
		t.Fatal(err)
	}

	detail, err := FromManager(manager).ServerDetail("", "web")
	if err != nil || detail == nil {
		t.Fatalf("详情适配失败：%+v %v", detail, err)
	}
	if !detail.ShowDeliveryRecords {
		t.Fatal("单机与权威端的服务器详情应显示下发记录组")
	}
	if len(detail.DeliveryRecords) != 1 {
		t.Fatalf("页面下发记录组不正确：%+v", detail.DeliveryRecords)
	}
	got := detail.DeliveryRecords[0]
	if got.Path != "/etc/app/config.yml" || got.Template != "token: {{sshmgr-secret:OLD_TOKEN}}" ||
		got.FileSHA256 != strings.Repeat("5", 64) || got.WrittenAt.Unix() != 1760000900 ||
		got.InitiatingDeviceName != "office-laptop" || got.InitiatingInstanceName != "agent-a" ||
		len(got.Secrets) != 1 || got.Secrets[0].Name != "OLD_TOKEN" ||
		got.Secrets[0].ValueVersion != 2 || !got.Secrets[0].Deleted {
		t.Fatalf("页面下发记录字段不正确：%+v", got)
	}
	if len(detail.SiblingDeliveryRecords) != 1 ||
		detail.SiblingDeliveryRecords[0].Path != "/etc/worker/config.yml" ||
		detail.SiblingDeliveryRecords[0].SourceServerName != "worker" ||
		detail.SiblingDeliveryRecords[0].InitiatingInstanceName != "agent-b" {
		t.Fatalf("页面同设备下发记录提示不正确：%+v", detail.SiblingDeliveryRecords)
	}
}

// TestWriterAdapterEditsAndAuditsWebUISource 用真实 vault 验证适配器：编辑走
// 服务器管理模块，审计来源为 Web UI 并带来源地址。
func TestWriterAdapterEditsAndAuditsWebUISource(t *testing.T) {
	manager, st := newAdapterManager(t)
	id, err := st.AddServerWithCredentials(&models.Server{
		Name: "web", Host: "192.0.2.10", Port: 22, User: "root",
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	writer := WriterFromManager(manager)

	detail, err := FromManager(manager).ServerDetail("", "web")
	if err != nil || detail == nil || detail.ID != id {
		t.Fatalf("详情适配不正确：%+v %v", detail, err)
	}
	role := "网站"
	result, conflict, err := writer.EditServer(id, webui.ServerEditForm{
		Role: &role, ExpectedRevision: detail.Revision,
	}, webui.ServerWriteSource{RemoteAddress: "192.0.2.7"})
	if err != nil || conflict != nil {
		t.Fatalf("编辑失败：%v conflict=%+v", err, conflict)
	}
	if result.Detail.Role != "网站" {
		t.Fatalf("编辑结果不正确：%+v", result.Detail)
	}

	stale := detail.Revision
	_, conflict, err = writer.EditServer(id, webui.ServerEditForm{
		Role: &role, ExpectedRevision: stale,
	}, webui.ServerWriteSource{RemoteAddress: "192.0.2.7"})
	if err != nil || conflict == nil || conflict.Current.Role != "网站" {
		t.Fatalf("修订号冲突应返回当前值：%v %+v", err, conflict)
	}

	rows, err := st.QueryAudit(store.AuditFilter{Actions: []string{"server.edit"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("应有两条编辑审计（成功与冲突失败），实际 %d", len(rows))
	}
	for _, row := range rows {
		if row.Source != store.AuditSourceWebUI || row.SourceIP != "192.0.2.7" {
			t.Fatalf("审计来源不正确：%+v", row)
		}
		if strings.Contains(row.Command, "网站") {
			t.Fatalf("审计摘要不得包含字段值：%s", row.Command)
		}
	}
}

// TestWriterAdapterEditsServerAddressList 用真实 vault 验证地址列表适配：
// 其他地址改归网络后返回最新详情，修订号冲突换成分区编辑的冲突视图。
func TestWriterAdapterEditsServerAddressList(t *testing.T) {
	manager, st := newAdapterManager(t)
	if _, err := st.AddSite("机房", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{
		Name: "机房局域网", Type: models.NetworkPhysical, SiteName: "机房",
	}); err != nil {
		t.Fatal(err)
	}
	device, err := st.AddDevice("workstation", "", "")
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.AddServer(&models.Server{
		Name: "web", Host: "203.0.113.10", Port: 22, User: "owner", DeviceID: device.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	writer := WriterFromManager(manager)

	result, conflict, err := writer.EditServerAddresses(id, webui.ServerAddressListForm{
		ExpectedRevision: 0,
		Rows:             []webui.ServerAddressRow{{Network: "机房局域网", Address: "10.0.0.8"}},
	}, webui.ServerWriteSource{RemoteAddress: "192.0.2.7"})
	if err != nil || conflict != nil {
		t.Fatalf("地址列表编辑失败：%v conflict=%+v", err, conflict)
	}
	if result.Detail == nil || result.Detail.Host != "" || result.Detail.Revision != 1 {
		t.Fatalf("地址列表编辑结果不正确：%+v", result.Detail)
	}

	_, conflict, err = writer.EditServerAddresses(id, webui.ServerAddressListForm{
		ExpectedRevision: 0,
		Rows:             []webui.ServerAddressRow{{Network: "机房局域网", Address: "10.0.0.9"}},
	}, webui.ServerWriteSource{RemoteAddress: "192.0.2.7"})
	if err != nil || conflict == nil || conflict.Current.Host != "" || conflict.Current.Revision != 1 {
		t.Fatalf("修订号冲突应返回当前详情：%v %+v", err, conflict)
	}
	rows, err := st.QueryAudit(store.AuditFilter{
		Actions: []string{"server.edit", "device.member-add"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("成功保存应各有一行服务器与网络成员审计，实际 %d", len(rows))
	}
	for _, row := range rows {
		if row.Source != store.AuditSourceWebUI || row.SourceIP != "192.0.2.7" {
			t.Fatalf("地址列表审计来源不正确：%+v", row)
		}
	}
}

// TestWriterAdapterEditServerAddressesParsesRowsAndRejectsDuplicates 验证
// other 行与网络行的分工，以及整份提交里的重复选择在进入存储前被拒绝。
func TestWriterAdapterEditServerAddressesParsesRowsAndRejectsDuplicates(t *testing.T) {
	manager, st := newAdapterManager(t)
	if _, err := st.AddSite("机房", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{
		Name: "机房局域网", Type: models.NetworkPhysical, SiteName: "机房",
	}); err != nil {
		t.Fatal(err)
	}
	device, err := st.AddDevice("workstation", "", "")
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.AddServer(&models.Server{
		Name: "web", Host: "203.0.113.10", Port: 22, User: "owner", DeviceID: device.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	writer := WriterFromManager(manager)

	result, conflict, err := writer.EditServerAddresses(id, webui.ServerAddressListForm{
		Rows: []webui.ServerAddressRow{
			{Network: "other", Address: "198.51.100.10"},
			{Network: "机房局域网", Address: "10.0.0.8"},
		},
	}, webui.ServerWriteSource{RemoteAddress: "192.0.2.7"})
	if err != nil || conflict != nil {
		t.Fatalf("混合地址行保存失败：%v conflict=%+v", err, conflict)
	}
	if result.Detail.Host != "198.51.100.10" {
		t.Fatalf("other 行应写入服务器条目其他地址：%+v", result.Detail)
	}
	memberships, err := st.ListNetworkMemberships("workstation")
	if err != nil {
		t.Fatal(err)
	}
	if len(memberships) != 1 || memberships[0].NetworkName != "机房局域网" {
		t.Fatalf("网络行应写入网络成员：%+v", memberships)
	}

	for _, testCase := range []struct {
		name string
		rows []webui.ServerAddressRow
		want string
	}{
		{
			name: "两个其他地址",
			rows: []webui.ServerAddressRow{
				{Network: "other", Address: "198.51.100.10"},
				{Network: "other", Address: "198.51.100.11"},
			},
			want: "其他（公网或未登记网络）地址最多只能有一行。",
		},
		{
			name: "两个相同网络",
			rows: []webui.ServerAddressRow{
				{Network: "机房局域网", Address: "10.0.0.8"},
				{Network: "机房局域网", Address: "10.0.0.9"},
			},
			want: "同一设备在同一网络里只能有一个网络成员。",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, _, err := writer.EditServerAddresses(id, webui.ServerAddressListForm{
				Rows: testCase.rows,
			}, webui.ServerWriteSource{RemoteAddress: "192.0.2.7"})
			var formErr *webui.ServerFormError
			if !errors.As(err, &formErr) || formErr.Message != testCase.want {
				t.Fatalf("重复选择应返回页面表单错误 %q，实际：%v", testCase.want, err)
			}
		})
	}
}

// TestDetailViewIncludesReverseGroups 用真实 vault 验证页面详情数据来自服
// 务器管理模块的同一份反向查询结果。
func TestDetailViewIncludesReverseGroups(t *testing.T) {
	manager, st := newAdapterManager(t)
	device, err := st.AddDevice("workstation", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddServer(&models.Server{
		Name: "web", Host: "192.0.2.10", Port: 22, User: "deploy", DeviceID: device.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecretWithEnvironmentName(
		"API_TOKEN", "接口令牌", []byte("ordinary-secret-value"), "API_TOKEN", store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret(
		"DATASET_PASSWORD", "挂载密码", []byte("share-secret-value"), store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	if err := st.AllowSecrets(
		[]string{"API_TOKEN", "DATASET_PASSWORD"}, []string{"web"}, store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	addedShare, err := st.AddSharedStorage(store.SharedStorageInput{
		Name: "datasets", Device: "workstation", User: "mount-user",
		PasswordSecret: "DATASET_PASSWORD", Drive: "Z",
		Note: "共享数据集",
	}, store.CLISource())
	if err != nil {
		t.Fatal(err)
	}
	profileID, err := st.AddProfile("运维")
	if err != nil {
		t.Fatal(err)
	}
	web, err := st.GetServerByName("web")
	if err != nil || web == nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{web.ID}); err != nil {
		t.Fatal(err)
	}

	detail, err := FromManager(manager).ServerDetail("", "web")
	if err != nil || detail == nil {
		t.Fatalf("详情适配失败：%+v %v", detail, err)
	}
	if len(detail.Secrets) != 1 || detail.Secrets[0].ID == "" || detail.Secrets[0].Name != "API_TOKEN" ||
		detail.Secrets[0].Note != "接口令牌" || detail.Secrets[0].EnvironmentName != "API_TOKEN" {
		t.Fatalf("页面机密项组不正确：%+v", detail.Secrets)
	}
	if len(detail.SharedStorages) != 1 || detail.SharedStorages[0].ID != addedShare.ID ||
		detail.SharedStorages[0].PasswordSecretID != addedShare.PasswordSecretID ||
		detail.SharedStorages[0].Name != "datasets" ||
		detail.SharedStorages[0].MountPoint != "/mnt/sshmgr/datasets" {
		t.Fatalf("页面共享存储组不正确：%+v", detail.SharedStorages)
	}
	if len(detail.Profiles) != 1 || detail.Profiles[0].ID != profileID || detail.Profiles[0].Name != "运维" {
		t.Fatalf("页面 profile 授权组不正确：%+v", detail.Profiles)
	}
	if len(detail.SiblingSharedStorages) != 0 {
		t.Fatalf("没有兄弟条目时页面不应有同设备提示：%+v", detail.SiblingSharedStorages)
	}
}
