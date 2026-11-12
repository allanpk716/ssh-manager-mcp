package store

import (
	"errors"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

func TestAddServerWithRelationsReportsExistingDeviceName(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.AddDevice("工作站", "", ""); err != nil {
		t.Fatal(err)
	}
	_, err := st.AddServerWithRelations(AddServerRelationsRequest{
		Server:     models.Server{Name: "web", User: "deploy", Host: "192.0.2.10", Port: 22},
		DeviceName: "工作站",
	})
	if !errors.Is(err, ErrDeviceNameExists) {
		t.Fatalf("重复设备名应返回可按 errors.Is 匹配的错误：%v", err)
	}
}

func TestAddServerWithRelationsRollsBackAllRowsWhenMembershipFails(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.AddSite("家A", ""); err != nil {
		t.Fatal(err)
	}
	network, err := st.AddNetwork(models.Network{Name: "家A局域网", Type: models.NetworkPhysical, SiteName: "家A"})
	if err != nil {
		t.Fatal(err)
	}
	profileID, err := st.AddProfile("运维")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`CREATE TRIGGER block_membership_insert
BEFORE INSERT ON network_memberships
BEGIN
	SELECT RAISE(ABORT, 'membership rejected');
END`); err != nil {
		t.Fatal(err)
	}

	_, err = st.AddServerWithRelations(AddServerRelationsRequest{
		Server:      models.Server{Name: "web", User: "deploy", Host: "192.0.2.10", Port: 22},
		DeviceName:  "工作站",
		Memberships: []NetworkMembershipWrite{{Network: network.Name, Address: "192.0.2.20"}},
		ProfileIDs:  []string{profileID}, Source: WebUISource("192.0.2.8"),
	})
	if err == nil || !strings.Contains(err.Error(), "membership rejected") {
		t.Fatalf("网络成员写入失败应返回底层错误：%v", err)
	}
	if servers, listErr := st.ListServers(); listErr != nil || len(servers) != 0 {
		t.Fatalf("事务失败后不得留下服务器条目：%v %+v", listErr, servers)
	}
	if devices, listErr := st.ListDevices(DeviceFilter{}); listErr != nil || len(devices) != 0 {
		t.Fatalf("事务失败后不得留下设备：%v %+v", listErr, devices)
	}
	if granted, listErr := st.ServersForProfile(profileID); listErr != nil || len(granted) != 0 {
		t.Fatalf("事务失败后不得留下 profile 授权：%v %v", granted, listErr)
	}
}

func TestAddOtherDeviceWithMembershipsRollsBackDeviceWhenMembershipFails(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.AddSite("家A", ""); err != nil {
		t.Fatal(err)
	}
	network, err := st.AddNetwork(models.Network{Name: "家A局域网", Type: models.NetworkPhysical, SiteName: "家A"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`CREATE TRIGGER block_other_membership_insert
BEFORE INSERT ON network_memberships
BEGIN
	SELECT RAISE(ABORT, 'membership rejected');
END`); err != nil {
		t.Fatal(err)
	}

	_, err = st.AddOtherDeviceWithMemberships(AddOtherDeviceRequest{
		Name: "交换机", Note: "不受管设备",
		Memberships: []NetworkMembershipWrite{{Network: network.Name, Address: "192.0.2.30"}},
		Source:      WebUISource("192.0.2.8"),
	})
	if err == nil || !strings.Contains(err.Error(), "membership rejected") {
		t.Fatalf("网络成员写入失败应返回底层错误：%v", err)
	}
	if devices, listErr := st.ListDevices(DeviceFilter{}); listErr != nil || len(devices) != 0 {
		t.Fatalf("事务失败后不得留下设备：%v %+v", listErr, devices)
	}
}
