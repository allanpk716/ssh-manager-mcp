package cli

import (
	"testing"

	"ssh-manager-mcp/internal/store"
)

// TestSnapshotEntryPreferredAddressUsesCommonNetwork：客户端本地快照里的
// 空主机条目取本机设备（唯一带客户端令牌的设备推断路径）与目标设备共同物
// 理局域网的首选成员地址；主机非空或没有共同网络时返回空串。
func TestSnapshotEntryPreferredAddressUsesCommonNetwork(t *testing.T) {
	snap := &store.Snapshot{
		Networks: []store.SnapshotNetwork{{ID: "net-1", Name: "家A局域网", Type: "physical"}},
		Devices: []store.SnapshotDevice{
			{ID: "device-target", Name: "工作站"},
			{ID: "device-local", Name: "笔记本", ClientTokenID: "tok-1"},
		},
		NetworkMemberships: []store.SnapshotNetworkMembership{
			{DeviceID: "device-target", NetworkID: "net-1", Address: "192.0.2.5"},
			{DeviceID: "device-local", NetworkID: "net-1", Address: "192.0.2.99"},
		},
		Servers: []store.SnapshotServer{
			{ID: "srv-bare", Name: "裸条目", Host: "", Port: 22, DeviceID: "device-target"},
			{ID: "srv-hosted", Name: "实主机", Host: "198.51.100.7", Port: 22, DeviceID: "device-target"},
		},
	}
	if got := snapshotEntryPreferredAddress(snap, "", &snap.Servers[0]); got != "192.0.2.5" {
		t.Fatalf("快照空主机条目的首选地址应为 192.0.2.5，实际 %q", got)
	}
	if got := snapshotEntryPreferredAddress(snap, "", &snap.Servers[1]); got != "" {
		t.Fatalf("主机非空条目不应参与首选地址计算，实际 %q", got)
	}

	isolated := &store.Snapshot{
		Networks: []store.SnapshotNetwork{{ID: "net-1", Name: "家A局域网", Type: "physical"}},
		Devices: []store.SnapshotDevice{
			{ID: "device-target", Name: "工作站"},
			{ID: "device-local", Name: "笔记本", ClientTokenID: "tok-1"},
		},
		NetworkMemberships: []store.SnapshotNetworkMembership{
			{DeviceID: "device-target", NetworkID: "net-1", Address: "192.0.2.5"},
		},
		Servers: []store.SnapshotServer{
			{ID: "srv-bare", Name: "裸条目", Host: "", Port: 22, DeviceID: "device-target"},
		},
	}
	if got := snapshotEntryPreferredAddress(isolated, "", &isolated.Servers[0]); got != "" {
		t.Fatalf("没有共同网络时应返回空串，实际 %q", got)
	}
}
