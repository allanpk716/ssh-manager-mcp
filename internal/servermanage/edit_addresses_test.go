package servermanage

import (
	"errors"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

func TestManagerEditAddressesMovesOtherAddressToNetwork(t *testing.T) {
	manager := newTestManager(t)
	if _, err := manager.st.AddSite("机房", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.st.AddNetwork(models.Network{
		Name: "机房局域网", Type: models.NetworkPhysical, SiteName: "机房",
	}); err != nil {
		t.Fatal(err)
	}
	device, err := manager.st.AddDevice("工作站", "", "")
	if err != nil {
		t.Fatal(err)
	}
	added, err := manager.st.AddServer(&models.Server{
		Name: "web", Host: "203.0.113.10", Port: 22, User: "owner", DeviceID: device.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := manager.EditAddresses(EditAddressesRequest{
		Identifier: added, ExpectedRevision: 0, CheckRevision: true,
		OtherAddress: "", Memberships: []NetworkMembershipInput{
			{Network: "机房局域网", Address: " 10.0.0.8 "},
		},
	}, WebUISource("192.0.2.7"))
	if err != nil {
		t.Fatalf("管理层保存地址列表失败：%v", err)
	}
	if result.Server == nil || result.Server.Host != "" || result.Server.Revision != 1 {
		t.Fatalf("保存后的服务器条目不正确：%+v", result.Server)
	}
	if len(result.Memberships) != 1 || result.Memberships[0].Address != "10.0.0.8" {
		t.Fatalf("保存后的网络成员不正确：%+v", result.Memberships)
	}
}

func TestManagerEditAddressesMapsValidationAndRevisionErrors(t *testing.T) {
	manager := newTestManager(t)
	device, err := manager.st.AddDevice("工作站", "", "")
	if err != nil {
		t.Fatal(err)
	}
	added, err := manager.st.AddServer(&models.Server{
		Name: "web", Host: "203.0.113.10", Port: 22, User: "owner", DeviceID: device.ID, Revision: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := manager.st.GetServer(added)
	if err != nil {
		t.Fatal(err)
	}
	srv.Description = "用于地址编辑测试"
	if err := manager.st.UpdateServer(srv); err != nil {
		t.Fatal(err)
	}

	_, err = manager.EditAddresses(EditAddressesRequest{
		Identifier: added, OtherAddress: "203.0.113 10",
	}, CLISource())
	var fieldErr *FieldError
	if !errors.As(err, &fieldErr) || fieldErr.Page != "地址不能包含空白字符。" {
		t.Fatalf("空白地址应换成页面校验错误：%v", err)
	}

	_, err = manager.EditAddresses(EditAddressesRequest{
		Identifier: added, ExpectedRevision: 0, CheckRevision: true,
		OtherAddress: "203.0.113.11",
	}, CLISource())
	var conflict *RevisionConflict
	if !errors.As(err, &conflict) || conflict.Current == nil || conflict.Current.Revision != 1 {
		t.Fatalf("修订号不一致应换成现有冲突错误：%v", err)
	}
	if rows, queryErr := manager.st.QueryAudit(store.AuditFilter{
		Actions: []string{"server.edit", "device.member-add", "device.member-rm"},
	}); queryErr != nil || len(rows) != 0 {
		t.Fatalf("校验与冲突失败不得写审计：%v %+v", queryErr, rows)
	}
}

func TestManagerEditAddressesMapsNetworkAddressConflict(t *testing.T) {
	manager := newTestManager(t)
	if _, err := manager.st.AddSite("机房", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.st.AddNetwork(models.Network{
		Name: "机房局域网", Type: models.NetworkPhysical, SiteName: "机房",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.st.AddDevice("NAS", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.st.AddNetworkMembership("NAS", "机房局域网", "10.0.0.8"); err != nil {
		t.Fatal(err)
	}
	device, err := manager.st.AddDevice("工作站", "", "")
	if err != nil {
		t.Fatal(err)
	}
	added, err := manager.st.AddServer(&models.Server{
		Name: "web", Host: "203.0.113.10", Port: 22, User: "owner", DeviceID: device.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = manager.EditAddresses(EditAddressesRequest{
		Identifier: added, OtherAddress: "", Memberships: []NetworkMembershipInput{
			{Network: "机房局域网", Address: "10.0.0.8"},
		},
	}, WebUISource("192.0.2.7"))
	var fieldErr *FieldError
	if !errors.As(err, &fieldErr) ||
		fieldErr.Page != "网络 机房局域网 的地址 10.0.0.8 已被设备 NAS 占用。" {
		t.Fatalf("网络地址冲突应换成页面校验错误：%v", err)
	}
}
