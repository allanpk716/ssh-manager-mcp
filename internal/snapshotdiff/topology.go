package snapshotdiff

import (
	"reflect"

	"ssh-manager-mcp/internal/store"
)

// topologyChangedText 是网络拓扑有差异时的固定分句。
const topologyChangedText = "网络拓扑已变化，list_servers 的 site、networks、device 字段可能不同"

// NetworkTopology 比较裁剪快照里的网络拓扑：站点、网络、设备、网络成员
// 与服务器条目的所属设备引用。布局坐标和创建、更新时间戳不参与比较。
type NetworkTopology struct{}

// Describe 在网络拓扑有语义差异时返回一条固定分句，没有差异时返回空。
func (NetworkTopology) Describe(old, fresh *store.Snapshot) []string {
	if !reflect.DeepEqual(sitesByID(old), sitesByID(fresh)) ||
		!reflect.DeepEqual(networksByID(old), networksByID(fresh)) ||
		!reflect.DeepEqual(devicesByID(old), devicesByID(fresh)) ||
		!reflect.DeepEqual(networkMembershipsByKey(old), networkMembershipsByKey(fresh)) ||
		!reflect.DeepEqual(serverDeviceIDs(old), serverDeviceIDs(fresh)) {
		return []string{topologyChangedText}
	}
	return nil
}

func sitesByID(snap *store.Snapshot) map[string]store.SnapshotSite {
	if snap == nil {
		return nil
	}
	out := make(map[string]store.SnapshotSite, len(snap.Sites))
	for _, site := range snap.Sites {
		site.CreatedAt, site.UpdatedAt = 0, 0
		out[site.ID] = site
	}
	return out
}

func networksByID(snap *store.Snapshot) map[string]store.SnapshotNetwork {
	if snap == nil {
		return nil
	}
	out := make(map[string]store.SnapshotNetwork, len(snap.Networks))
	for _, network := range snap.Networks {
		network.CreatedAt, network.UpdatedAt = 0, 0
		out[network.ID] = network
	}
	return out
}

func devicesByID(snap *store.Snapshot) map[string]store.SnapshotDevice {
	if snap == nil {
		return nil
	}
	out := make(map[string]store.SnapshotDevice, len(snap.Devices))
	for _, device := range snap.Devices {
		device.CreatedAt, device.UpdatedAt = 0, 0
		out[device.ID] = device
	}
	return out
}

type networkMembershipKey struct {
	deviceID  string
	networkID string
}

func networkMembershipsByKey(snap *store.Snapshot) map[networkMembershipKey]store.SnapshotNetworkMembership {
	if snap == nil {
		return nil
	}
	out := make(
		map[networkMembershipKey]store.SnapshotNetworkMembership,
		len(snap.NetworkMemberships),
	)
	for _, membership := range snap.NetworkMemberships {
		membership.CreatedAt, membership.UpdatedAt = 0, 0
		out[networkMembershipKey{
			deviceID: membership.DeviceID, networkID: membership.NetworkID,
		}] = membership
	}
	return out
}

func serverDeviceIDs(snap *store.Snapshot) map[string]string {
	if snap == nil {
		return nil
	}
	out := make(map[string]string, len(snap.Servers))
	for _, server := range snap.Servers {
		// 空引用代表条目没有登记设备；新增或删除这类条目不是拓扑变化。
		if server.DeviceID != "" {
			out[server.ID] = server.DeviceID
		}
	}
	return out
}
