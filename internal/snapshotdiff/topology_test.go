package snapshotdiff

import (
	"testing"

	"ssh-manager-mcp/internal/store"
)

func TestDefaultHintTopologyChanges(t *testing.T) {
	const topologyChangedText = "网络拓扑已变化，list_servers 的 site、networks、device 字段可能不同"
	baseSnapshot := func(revision int64) *store.Snapshot {
		return &store.Snapshot{
			SnapshotRevision: revision,
			Servers: []store.SnapshotServer{{
				ID: "server-1", Name: "gpu", DeviceID: "device-1",
			}},
			Sites: []store.SnapshotSite{{
				ID: "site-1", Name: "办公室", Note: "原备注",
				CreatedAt: 1000, UpdatedAt: 1001,
			}},
			Networks: []store.SnapshotNetwork{{
				ID: "network-1", Name: "办公局域网", Type: "physical", SiteID: "site-1",
				CIDR: "192.0.2.0/24", CreatedAt: 1000, UpdatedAt: 1001,
			}},
			Devices: []store.SnapshotDevice{{
				ID: "device-1", Name: "工作站", ClientTokenID: "client-token-1", ClientName: "laptop",
				CreatedAt: 1000, UpdatedAt: 1001,
			}},
			NetworkMemberships: []store.SnapshotNetworkMembership{{
				DeviceID: "device-1", NetworkID: "network-1", Address: "192.0.2.10",
				CreatedAt: 1000, UpdatedAt: 1001,
			}},
		}
	}
	tests := []struct {
		name  string
		fresh func(*store.Snapshot)
	}{
		{
			name: "站点变化",
			fresh: func(snap *store.Snapshot) {
				snap.Sites[0].Note = "新备注"
			},
		},
		{
			name: "网络变化",
			fresh: func(snap *store.Snapshot) {
				snap.Networks[0].CIDR = "198.51.100.0/24"
			},
		},
		{
			name: "设备变化",
			fresh: func(snap *store.Snapshot) {
				snap.Devices[0].Name = "新工作站"
			},
		},
		{
			name: "网络成员变化",
			fresh: func(snap *store.Snapshot) {
				snap.NetworkMemberships[0].Address = "198.51.100.10"
			},
		},
		{
			name: "服务器条目所属设备引用变化",
			fresh: func(snap *store.Snapshot) {
				snap.Servers[0].DeviceID = "device-2"
			},
		},
		{
			name: "只改布局坐标",
			fresh: func(snap *store.Snapshot) {
				snap.DeviceLayoutCoordinates = []store.SnapshotDeviceLayoutCoordinate{{
					ViewKey: "主视图", DeviceID: "device-1", X: 12.5, Y: -3,
					CreatedAt: 1000, UpdatedAt: 1001,
				}}
			},
		},
		{
			name: "只改拓扑时间戳",
			fresh: func(snap *store.Snapshot) {
				snap.Sites[0].CreatedAt, snap.Sites[0].UpdatedAt = 2000, 2001
				snap.Networks[0].CreatedAt, snap.Networks[0].UpdatedAt = 2000, 2001
				snap.Devices[0].CreatedAt, snap.Devices[0].UpdatedAt = 2000, 2001
				snap.NetworkMemberships[0].CreatedAt = 2000
				snap.NetworkMemberships[0].UpdatedAt = 2001
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fresh := baseSnapshot(11)
			test.fresh(fresh)
			got := Default().Hint(baseSnapshot(10), fresh)
			if test.name == "只改布局坐标" || test.name == "只改拓扑时间戳" {
				if got != "" {
					t.Fatalf("不该提示的拓扑变化产生了提示：%q", got)
				}
				return
			}
			want := "权威端配置已更新（修订号 10 → 11）：" + topologyChangedText
			if got != want {
				t.Fatalf("变更提示 = %q，应为 %q", got, want)
			}
		})
	}
}

func TestDefaultHintMergesServerAndTopologyChanges(t *testing.T) {
	old := &store.Snapshot{
		SnapshotRevision: 10,
		Servers:          []store.SnapshotServer{{ID: "server-1", Name: "gpu"}},
	}
	fresh := &store.Snapshot{
		SnapshotRevision: 11,
		Servers: []store.SnapshotServer{
			{ID: "server-1", Name: "gpu"},
			{ID: "server-2", Name: "extra", DeviceID: "device-1"},
		},
		Devices: []store.SnapshotDevice{{ID: "device-1", Name: "工作站"}},
	}
	got := Default().Hint(old, fresh)
	want := "权威端配置已更新（修订号 10 → 11）：新增服务器 extra；网络拓扑已变化，list_servers 的 site、networks、device 字段可能不同"
	if got != want {
		t.Fatalf("服务器与拓扑差异应合并成一条提示，实际 = %q，应为 %q", got, want)
	}
}
