package store

import (
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

func TestReplaceDeviceAddressListMovesOtherAddressToNetwork(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.AddSite("机房", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{
		Name: "机房局域网", Type: models.NetworkPhysical, SiteName: "机房",
	}); err != nil {
		t.Fatal(err)
	}
	device, err := st.AddDevice("工作站", "", "")
	if err != nil {
		t.Fatal(err)
	}
	added, err := st.AddServer(&models.Server{
		Name: "web", Host: "203.0.113.10", Port: 22, User: "owner", DeviceID: device.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := st.ReplaceDeviceAddressList(ReplaceDeviceAddressListRequest{
		ServerID: added, ExpectedRevision: 0, CheckRevision: true,
		OtherAddress: "", Memberships: []NetworkMembershipWrite{
			{Network: "机房局域网", Address: "10.0.0.8"},
		},
		Source: WebUISource("192.0.2.7"),
	})
	if err != nil {
		t.Fatalf("其他地址改归网络失败：%v", err)
	}
	if result.Server == nil || result.Server.Host != "" || result.Server.Revision != 1 {
		t.Fatalf("服务器条目其他地址应清空且修订号加一：%+v", result.Server)
	}
	memberships, err := st.ListNetworkMembershipsByRegistration("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if len(memberships) != 1 || memberships[0].NetworkName != "机房局域网" ||
		memberships[0].Address != "10.0.0.8" {
		t.Fatalf("改归后网络成员不正确：%+v", memberships)
	}
	auditRows, err := st.QueryAudit(AuditFilter{
		Actions: []string{"device.member-add", "server.edit"}, Source: AuditSourceWebUI,
	})
	if err != nil {
		t.Fatal(err)
	}
	actions := make(map[string]int, len(auditRows))
	for _, row := range auditRows {
		actions[row.Action]++
	}
	if actions["device.member-add"] != 1 || actions["server.edit"] != 1 {
		t.Fatalf("改归网络应同事务写网络成员与服务器条目审计：%+v", auditRows)
	}
}

func TestReplaceDeviceAddressListRollsBackMembershipAndAudit(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.AddSite("机房", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{
		Name: "机房局域网", Type: models.NetworkPhysical, SiteName: "机房",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "虚拟网", Type: models.NetworkVirtual}); err != nil {
		t.Fatal(err)
	}
	device, err := st.AddDevice("工作站", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership("工作站", "机房局域网", "10.0.0.8"); err != nil {
		t.Fatal(err)
	}
	added, err := st.AddServer(&models.Server{
		Name: "web", Host: "203.0.113.10", Port: 22, User: "owner", DeviceID: device.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	revisionBefore, err := st.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`CREATE TRIGGER block_server_host_update
BEFORE UPDATE ON servers
WHEN NEW.host <> OLD.host
BEGIN
	SELECT RAISE(ABORT, 'server host rejected');
END`); err != nil {
		t.Fatal(err)
	}

	_, err = st.ReplaceDeviceAddressList(ReplaceDeviceAddressListRequest{
		ServerID: added, OtherAddress: "", Memberships: []NetworkMembershipWrite{
			{Network: "虚拟网", Address: "100.64.0.8"},
		},
		Source: WebUISource("192.0.2.7"),
	})
	if err == nil || !strings.Contains(err.Error(), "server host rejected") {
		t.Fatalf("服务器条目更新失败应返回注入错误：%v", err)
	}
	srv, err := st.GetServer(added)
	if err != nil {
		t.Fatal(err)
	}
	if srv.Host != "203.0.113.10" || srv.Revision != 0 {
		t.Fatalf("事务失败后服务器条目应保持原状：%+v", srv)
	}
	memberships, err := st.ListNetworkMembershipsByRegistration("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if len(memberships) != 1 || memberships[0].NetworkName != "机房局域网" ||
		memberships[0].Address != "10.0.0.8" {
		t.Fatalf("事务失败后网络成员应保持原状：%+v", memberships)
	}
	auditRows, err := st.QueryAudit(AuditFilter{
		Actions: []string{"device.member-rm", "device.member-add", "device.member-edit", "server.edit"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(auditRows) != 1 || auditRows[0].Action != "device.member-add" {
		t.Fatalf("事务失败后只应保留初始审计：%+v", auditRows)
	}
	revisionAfter, err := st.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if revisionAfter != revisionBefore {
		t.Fatalf("事务失败后快照修订号应保持 %d，实际 %d", revisionBefore, revisionAfter)
	}
}
