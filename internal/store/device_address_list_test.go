package store

import (
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
