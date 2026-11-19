package access

import (
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

func TestListClientsIncludesProfileServersAndMachine(t *testing.T) {
	m, st := newTestManager(t)
	web := mustServer(t, st, "web1")
	profileID, err := st.AddProfile("运维")
	if err != nil {
		t.Fatal(err)
	}
	mustGrant(t, st, profileID, web.ID)
	clientID, _, err := st.AddCacheToken("laptop-client", profileID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.TouchCacheToken(clientID, "v0.22.0"); err != nil {
		t.Fatal(err)
	}
	device, err := st.AddDevice("工作站", "", "laptop-client")
	if err != nil {
		t.Fatal(err)
	}

	clients, err := m.ListClients()
	if err != nil {
		t.Fatal(err)
	}
	if len(clients) != 1 {
		t.Fatalf("客户端数量 = %d，应为 1：%+v", len(clients), clients)
	}
	client := clients[0]
	if client.ID != clientID || client.Name != "laptop-client" || client.ProfileName != "运维" ||
		client.ServerCount != 1 || client.ClientVersion != "v0.22.0" || client.MachineName != device.Name {
		t.Fatalf("客户端列表字段不正确：%+v", client)
	}
	if client.Paired {
		t.Fatalf("没有配对生成的访问身份时不应判定为经配对接入：%+v", client)
	}
}

func TestClientDetailIncludesVisibleResourcesAndSoftwareSummary(t *testing.T) {
	m, st := newTestManager(t)
	web := mustServer(t, st, "web1")
	profileID, err := st.AddProfile("运维")
	if err != nil {
		t.Fatal(err)
	}
	mustGrant(t, st, profileID, web.ID)
	clientID, _, err := st.AddCacheToken("laptop-client", profileID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecretWithEnvironmentName("WEB_TOKEN", "网站机密项", []byte("browser-test-value"), "WEB_TOKEN_FILE", store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := st.AllowSecrets([]string{"WEB_TOKEN"}, []string{"web1"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSite("公司", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSiteWithParent("一号楼", "", "公司"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDeviceAtSite("工作站", "", "laptop-client", "公司 / 一号楼"); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceSoftwareObservedItems("工作站", store.SoftwareObservedWrite{
		Items: []models.SoftwareObservedItem{
			{Category: models.SoftwareCategoryOperatingSystem, Name: "Ubuntu", Version: "24.04"},
			{
				Category: models.SoftwareCategoryServiceProgram, Name: "nginx", Version: "1.24",
				ListeningPorts: []models.SoftwareListeningPort{{
					Address: "0.0.0.0", Port: 80, Protocol: "tcp",
					Exposure: models.SoftwareExposureAllAddresses,
				}},
			},
		},
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}

	detail, err := m.ClientDetail(clientID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Name != "laptop-client" || detail.TokenPrefix == "" || len(detail.Servers) != 1 || detail.Servers[0].Name != "web1" {
		t.Fatalf("客户端基础详情不正确：%+v", detail)
	}
	if detail.Machine == nil || detail.Machine.Name != "工作站" || detail.Machine.SitePath != "公司 / 一号楼" {
		t.Fatalf("所在机器不正确：%+v", detail.Machine)
	}
	if detail.Software == nil || detail.Software.ItemCount != 2 || detail.Software.PendingExposureCount != 1 ||
		len(detail.Software.TopItems) != 2 {
		t.Fatalf("软件清单摘要不正确：%+v", detail.Software)
	}
	if len(detail.Secrets) != 1 || detail.Secrets[0].Name != "WEB_TOKEN" ||
		detail.Secrets[0].Note != "网站机密项" || len(detail.Secrets[0].ViaServers) != 1 || detail.Secrets[0].ViaServers[0] != "web1" {
		t.Fatalf("可见机密项不正确：%+v", detail.Secrets)
	}
}

func TestClientProfilePreviewAndChange(t *testing.T) {
	m, st := newTestManager(t)
	web := mustServer(t, st, "web1")
	db := mustServer(t, st, "db1")
	ops, err := st.AddProfile("运维")
	if err != nil {
		t.Fatal(err)
	}
	dev, err := st.AddProfile("开发")
	if err != nil {
		t.Fatal(err)
	}
	mustGrant(t, st, ops, web.ID)
	mustGrant(t, st, dev, db.ID)
	clientID, _, err := st.AddCacheToken("laptop-client", ops)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret("DB_TOKEN", "数据库机密项", []byte("database-secret"), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := st.AllowSecrets([]string{"DB_TOKEN"}, []string{"db1"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret("WEB_TOKEN", "网站机密项", []byte("website-secret"), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := st.AllowSecrets([]string{"WEB_TOKEN"}, []string{"web1"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}

	preview, err := m.PreviewClientProfileChange(clientID, dev)
	if err != nil {
		t.Fatal(err)
	}
	if preview.ClientName != "laptop-client" || preview.CurrentProfileID != ops || preview.DesiredProfileID != dev {
		t.Fatalf("预览的基础信息不正确：%+v", preview)
	}
	if len(preview.AddedServers) != 1 || preview.AddedServers[0].Name != "db1" ||
		len(preview.LostServers) != 1 || preview.LostServers[0].Name != "web1" {
		t.Fatalf("服务器增减预览不正确：%+v", preview)
	}
	if len(preview.AddedSecrets) != 1 || preview.AddedSecrets[0].Name != "DB_TOKEN" ||
		len(preview.LostSecrets) != 1 || preview.LostSecrets[0].Name != "WEB_TOKEN" {
		t.Fatalf("机密项增减预览不正确：%+v", preview)
	}

	if err := m.ChangeClientProfile(clientID, ops, dev, CLISource()); err != nil {
		t.Fatal(err)
	}
	detail, err := m.ClientDetail(clientID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.ProfileID != dev {
		t.Fatalf("更换后的 profile = %q，应为 %q", detail.ProfileID, dev)
	}
	rows := ownerAuditRows(t, st, "cache-token.bind")
	if len(rows) != 1 {
		t.Fatalf("cache-token.bind 审计行数量 = %d，应为 1", len(rows))
	}
	assertAuditShape(t, rows[0], "cache-token.bind", "ok", `{"name":"laptop-client","profile":"开发"}`, store.AuditSourceCLI, "")
}

func TestClientMachineChoicesAndLink(t *testing.T) {
	m, st := newTestManager(t)
	profileID, err := st.AddProfile("运维")
	if err != nil {
		t.Fatal(err)
	}
	clientID, _, err := st.AddCacheToken("laptop-client", profileID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("空闲机器", "", ""); err != nil {
		t.Fatal(err)
	}

	choices, err := m.ListClientMachineChoices(clientID)
	if err != nil {
		t.Fatal(err)
	}
	if len(choices) != 1 || choices[0].Name != "空闲机器" || !choices[0].Available {
		t.Fatalf("机器选择不正确：%+v", choices)
	}
	if err := m.LinkClientMachine(clientID, choices[0].ID, WebUISource("192.0.2.7")); err != nil {
		t.Fatal(err)
	}
	detail, err := m.ClientDetail(clientID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Machine == nil || detail.Machine.Name != "空闲机器" {
		t.Fatalf("关联后的机器不正确：%+v", detail.Machine)
	}
}

func TestRemoveClientWritesAudit(t *testing.T) {
	m, st := newTestManager(t)
	profileID, err := st.AddProfile("运维")
	if err != nil {
		t.Fatal(err)
	}
	clientID, _, err := st.AddCacheToken("laptop-client", profileID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("工作站", "", "laptop-client"); err != nil {
		t.Fatal(err)
	}

	if err := m.RemoveClient(clientID, WebUISource("192.0.2.7")); err != nil {
		t.Fatal(err)
	}
	rows := ownerAuditRows(t, st, "client.remove")
	if len(rows) != 1 {
		t.Fatalf("client.remove 审计行数量 = %d，应为 1", len(rows))
	}
	assertAuditShape(t, rows[0], "client.remove", "ok", `{"client":"laptop-client","machine":"工作站"}`, store.AuditSourceWebUI, "192.0.2.7")
	detail, err := m.ClientDetail(clientID)
	if err != nil {
		t.Fatal(err)
	}
	if detail == nil || !detail.Revoked {
		t.Fatalf("移除后的客户端状态不正确：%+v", detail)
	}
}
