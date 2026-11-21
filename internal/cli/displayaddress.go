package cli

import (
	"fmt"

	"ssh-manager-mcp/internal/dialdevice"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
	"ssh-manager-mcp/internal/webuitopology"
)

// entryPreferredAddress 返回空主机条目给人看的首选网络地址（纯地址，不带
// 端口）：设备连接候选里第一个物理局域网或虚拟局域网成员地址，与 Web UI
// 列表卡片、详情页页头同为本机起点、同一选取规则，保证同一个条目在所有
// 显示面算出同一个结果。条目主机非空、找不到设备或任何读取失败时返回空
// 串，调用方按无候选退化。
func entryPreferredAddress(s *store.Store, srv *models.Server) string {
	if s == nil || srv == nil || srv.Host != "" || srv.DeviceID == "" {
		return ""
	}
	devices, err := s.ListDevices(store.DeviceFilter{})
	if err != nil {
		return ""
	}
	var target *models.Device
	for _, device := range devices {
		if device.ID == srv.DeviceID {
			target = device
			break
		}
	}
	if target == nil {
		return ""
	}
	local, err := dialdevice.Current(s, "")
	if err != nil || !local.Known || local.Device == nil {
		return ""
	}
	return preferredAddressFromMemberships(target.Memberships, local.Device.Memberships)
}

// preferredAddressFromMemberships 用统一拨号入口的共同网络规划（物理局域
// 网优先、虚拟局域网其次）排候选，再取第一个局域网成员地址。
func preferredAddressFromMemberships(targetMemberships, dialerMemberships []*models.NetworkMembership) string {
	plan := sshbroker.PlanCommonNetworkAddresses(targetMemberships, dialerMemberships)
	candidates := make([]models.NetworkAddressCandidate, 0, len(plan.Candidates))
	for _, candidate := range plan.Candidates {
		candidates = append(candidates, models.NetworkAddressCandidate{
			Address: candidate.Address, Source: candidate.Source,
		})
	}
	return models.PreferredNetworkAddress(candidates)
}

// snapshotEntryPreferredAddress 返回客户端本地快照里空主机条目的首选网
// 络地址：与 Web UI 客户端页同一个拓扑来源、同为本机起点，保证同一个条
// 目在所有显示面算出同一个结果。读取失败或找不到设备时返回空串。
func snapshotEntryPreferredAddress(snap *store.Snapshot, instance string, srv *store.SnapshotServer) string {
	if snap == nil || srv == nil || srv.Host != "" || srv.DeviceID == "" {
		return ""
	}
	source := webuitopology.FromClientSnapshot(func(string) (*store.Snapshot, error) { return snap, nil })
	data, err := source.Topology(instance, webui.TopologyProbeOriginSelf)
	if err != nil {
		return ""
	}
	for index := range data.Devices {
		if data.Devices[index].ID != srv.DeviceID {
			continue
		}
		candidates := data.Devices[index].Candidates
		converted := make([]models.NetworkAddressCandidate, 0, len(candidates))
		for _, candidate := range candidates {
			converted = append(converted, models.NetworkAddressCandidate{
				Address: candidate.Address, Source: models.DialAddressSource(candidate.Source),
			})
		}
		return models.PreferredNetworkAddress(converted)
	}
	return ""
}

// entryConnectionText 渲染命令行给人看的「用户@地址:端口」段：条目主机非
// 空时照旧；主机为空时用首选网络地址；没有局域网候选时退为「user (port N)」。
func entryConnectionText(s *store.Store, srv *models.Server) string {
	if srv.Host != "" {
		return fmt.Sprintf("%s@%s:%d", srv.User, srv.Host, srv.Port)
	}
	if address := entryPreferredAddress(s, srv); address != "" {
		return fmt.Sprintf("%s@%s:%d", srv.User, address, srv.Port)
	}
	return fmt.Sprintf("%s (port %d)", srv.User, srv.Port)
}

// entryHostText 渲染 pin-hostkey 各输出 host= 位置的条目地址段：主机非空时
// 照旧「Host:端口」；主机为空时用首选网络地址拼「地址:端口」；没有局域网
// 候选时退为「user (port N)」。
func entryHostText(s *store.Store, srv *models.Server) string {
	if srv.Host != "" {
		return store.EntryFallbackAddr(srv)
	}
	if address := entryPreferredAddress(s, srv); address != "" {
		return fmt.Sprintf("%s:%d", address, srv.Port)
	}
	return fmt.Sprintf("%s (port %d)", srv.User, srv.Port)
}
