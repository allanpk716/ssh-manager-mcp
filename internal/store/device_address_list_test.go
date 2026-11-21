package store

import (
	"errors"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// TestReplaceDeviceAddressListValidationCodes 逐条锁住地址列表五种校验错误
// 的错误码与错误文字；入口层按错误码映射字段归属与界面文案。
func TestReplaceDeviceAddressListValidationCodes(t *testing.T) {
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

	cases := []struct {
		name        string
		request     ReplaceDeviceAddressListRequest
		wantCode    string
		wantMessage string
	}{
		{
			name:        "其他地址含空白",
			request:     ReplaceDeviceAddressListRequest{ServerID: added, OtherAddress: "203.0.113 10"},
			wantCode:    TopologyCodeOtherAddressWhitespace,
			wantMessage: "地址不能包含空白字符",
		},
		{
			name: "成员行不完整",
			request: ReplaceDeviceAddressListRequest{ServerID: added, Memberships: []NetworkMembershipWrite{
				{Network: "机房局域网", Address: ""},
			}},
			wantCode:    TopologyCodeMembershipIncomplete,
			wantMessage: "每行网络地址必须同时选择网络并填写地址",
		},
		{
			name: "成员地址含空白",
			request: ReplaceDeviceAddressListRequest{ServerID: added, Memberships: []NetworkMembershipWrite{
				{Network: "机房局域网", Address: "10.0.0 8"},
			}},
			wantCode:    TopologyCodeMembershipAddressWhitespace,
			wantMessage: "地址不能包含空白字符",
		},
		{
			name: "网络重复",
			request: ReplaceDeviceAddressListRequest{ServerID: added, Memberships: []NetworkMembershipWrite{
				{Network: "机房局域网", Address: "10.0.0.8"},
				{Network: "机房局域网", Address: "10.0.0.9"},
			}},
			wantCode:    TopologyCodeDuplicateNetwork,
			wantMessage: "同一台机器在同一个网络里只能登记一个地址",
		},
		{
			name:        "没有任何地址",
			request:     ReplaceDeviceAddressListRequest{ServerID: added},
			wantCode:    TopologyCodeAddressRequired,
			wantMessage: "至少填写一个地址",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := st.ReplaceDeviceAddressList(testCase.request)
			var topologyErr *TopologyValidationError
			if !errors.As(err, &topologyErr) {
				t.Fatalf("校验失败应返回拓扑校验错误，实际：%v", err)
			}
			if topologyErr.Code != testCase.wantCode {
				t.Fatalf("错误码 = %q，应为 %q（%s）", topologyErr.Code, testCase.wantCode, topologyErr.Message)
			}
			if topologyErr.Message != testCase.wantMessage {
				t.Fatalf("错误文字 = %q，应为 %q", topologyErr.Message, testCase.wantMessage)
			}
		})
	}
}

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

	beforeServer, err := st.GetServer(added)
	if err != nil {
		t.Fatal(err)
	}
	beforeMemberships, err := st.ListNetworkMembershipsByRegistration("工作站")
	if err != nil {
		t.Fatal(err)
	}
	beforeAuditRows, err := st.QueryAudit(AuditFilter{
		Actions: []string{"device.member-add", "device.member-edit", "device.member-rm", "server.edit"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if beforeServer.Host != "203.0.113.10" || beforeServer.Revision != 0 ||
		len(beforeMemberships) != 0 || len(beforeAuditRows) != 0 {
		t.Fatalf("网段确认只改编辑草稿时不应提前写入成员、清空其他地址或审计：%+v %+v %+v",
			beforeServer, beforeMemberships, beforeAuditRows)
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

func TestReplaceDeviceAddressListMemberToOtherClearAndMembershipOnly(t *testing.T) {
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
	if _, err := st.AddNetworkMembership("工作站", "虚拟网", "100.64.0.8"); err != nil {
		t.Fatal(err)
	}
	added, err := st.AddServer(&models.Server{
		Name: "web", Host: "203.0.113.10", Port: 22, User: "owner", DeviceID: device.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	toOther, err := st.ReplaceDeviceAddressList(ReplaceDeviceAddressListRequest{
		ServerID: added, OtherAddress: "10.0.0.8", Memberships: []NetworkMembershipWrite{
			{Network: "虚拟网", Address: "100.64.0.8"},
		},
	})
	if err != nil {
		t.Fatalf("网络成员改成其他地址失败：%v", err)
	}
	if toOther.Server.Host != "10.0.0.8" || toOther.Server.Revision != 1 {
		t.Fatalf("网络成员改成其他地址后的服务器条目不正确：%+v", toOther.Server)
	}

	cleared, err := st.ReplaceDeviceAddressList(ReplaceDeviceAddressListRequest{
		ServerID: added, ExpectedRevision: 1, CheckRevision: true,
		OtherAddress: "", Memberships: []NetworkMembershipWrite{
			{Network: "虚拟网", Address: "100.64.0.8"},
		},
	})
	if err != nil {
		t.Fatalf("清空其他地址失败：%v", err)
	}
	if cleared.Server.Host != "" || cleared.Server.Revision != 2 {
		t.Fatalf("清空其他地址后的服务器条目不正确：%+v", cleared.Server)
	}

	membershipOnly, err := st.ReplaceDeviceAddressList(ReplaceDeviceAddressListRequest{
		ServerID: added, ExpectedRevision: 2, CheckRevision: true,
		OtherAddress: "", Memberships: []NetworkMembershipWrite{
			{Network: "机房局域网", Address: "10.0.0.9"},
		},
	})
	if err != nil {
		t.Fatalf("只修改网络成员失败：%v", err)
	}
	if membershipOnly.Server.Host != "" || membershipOnly.Server.Revision != 2 {
		t.Fatalf("只修改网络成员不应推进服务器条目修订号：%+v", membershipOnly.Server)
	}
	memberships, err := st.ListNetworkMembershipsByRegistration("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if len(memberships) != 1 || memberships[0].NetworkName != "机房局域网" ||
		memberships[0].Address != "10.0.0.9" {
		t.Fatalf("只修改网络成员后的成员不正确：%+v", memberships)
	}
	rows, err := st.QueryAudit(AuditFilter{
		Actions: []string{"device.member-rm", "device.member-add", "device.member-edit", "server.edit"},
	})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, row := range rows {
		counts[row.Action]++
	}
	if counts["server.edit"] != 2 || counts["device.member-rm"] != 2 ||
		counts["device.member-add"] != 3 {
		t.Fatalf("地址状态转换审计不正确：%+v", rows)
	}
}
