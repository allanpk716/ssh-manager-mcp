package servermanage

import (
	"testing"

	"ssh-manager-mcp/internal/models"
)

// TestManagerAddServerUsesMultipleAddressesAndLocation 覆盖分步新增与命令行
// 共用的公共写入边界：多行「网络 + 地址」和地点都随服务器条目一次写入。
func TestManagerAddServerUsesMultipleAddressesAndLocation(t *testing.T) {
	m := newTestManager(t)
	physical, virtual, _, _ := seedAddTopology(t, m)

	result, err := m.Add(AddRequest{
		Server:   models.Server{Name: "multi-address", User: "deploy", Port: 22},
		SiteName: "家A",
		Memberships: []NetworkMembershipInput{
			{Network: physical, Address: "192.168.1.41"},
			{Network: virtual, Address: "100.64.0.41"},
		},
	}, CLISource())
	if err != nil {
		t.Fatal(err)
	}

	srv, err := m.st.GetServer(result.ServerID)
	if err != nil {
		t.Fatal(err)
	}
	if srv.Host != "192.168.1.41" {
		t.Fatalf("兜底地址应取第一个物理局域网地址，实际 %q", srv.Host)
	}
	device, err := m.st.GetDevice(result.DeviceName)
	if err != nil {
		t.Fatal(err)
	}
	if device.SiteName != "家A" {
		t.Fatalf("新机器应登记所选地点，实际 %q", device.SiteName)
	}
	memberships, err := m.st.ListNetworkMemberships(device.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(memberships) != 2 {
		t.Fatalf("应保存两条网络地址，实际 %d 条", len(memberships))
	}
}
