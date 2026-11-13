package shareaddress

import (
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sshbroker"
)

// DesktopAddress 是桌面手动添加引导里的一个共享存储地址。
type DesktopAddress struct {
	Address         string
	NetworkName     string
	PhysicalNetwork bool
}

// DesktopAddressPlan 是按 owner 桌面设备与共享存储提供方设备推算出的地址
// 视图。推荐地址与共同网络排序复用真实拨号使用的规划函数。
type DesktopAddressPlan struct {
	DesktopInTopology  bool
	RecommendedAddress *DesktopAddress
	ProviderAddresses  []DesktopAddress
	Reason             string
}

// PlanDesktopAddresses 按共同网络推算桌面手动添加引导的推荐地址；桌面设备
// 不在拓扑里时不做推荐，改列提供方设备的全部网络成员地址。
func PlanDesktopAddresses(
	providerMemberships, desktopMemberships []*models.NetworkMembership, desktopInTopology bool,
) DesktopAddressPlan {
	plan := DesktopAddressPlan{DesktopInTopology: desktopInTopology}
	if !desktopInTopology {
		plan.ProviderAddresses = providerAddressViews(providerMemberships)
		return plan
	}
	common := sshbroker.PlanCommonNetworkAddresses(providerMemberships, desktopMemberships)
	if len(common.Candidates) == 0 {
		plan.Reason = "这台电脑与共享存储提供方设备没有共同网络，因此不给出推荐地址。"
		return plan
	}
	candidate := common.Candidates[0]
	plan.RecommendedAddress = &DesktopAddress{
		Address: candidate.Address, NetworkName: candidate.NetworkName,
		PhysicalNetwork: candidate.Source == sshbroker.DialAddressPhysical,
	}
	return plan
}

func providerAddressViews(memberships []*models.NetworkMembership) []DesktopAddress {
	addresses := make([]DesktopAddress, 0, len(memberships))
	for _, membership := range memberships {
		if membership == nil {
			continue
		}
		addresses = append(addresses, DesktopAddress{
			Address: membership.Address, NetworkName: membership.NetworkName,
			PhysicalNetwork: membership.NetworkType == models.NetworkPhysical,
		})
	}
	return addresses
}
