// Package networkrange 计算一个 IP 地址落在哪些网络的已登记网段内。结果
// 只用于服务器详情页提示与编辑草稿确认，不写网络成员，也不参与可达判断
// 与连接选址。
package networkrange

import (
	"net/netip"
	"strings"

	"ssh-manager-mcp/internal/models"
)

// Network 是参与一次提示计算的网络。Physical 为真表示物理局域网，必须
// 满足设备地点被覆盖地点包含；为假表示虚拟局域网，只按网段判断。
type Network struct {
	Name     string
	Range    string
	SiteID   string
	Physical bool
}

// Input 是一次提示计算的完整输入。CurrentNetwork 是地址行当前归属的网络
// 名，other 表示其他（公网或未登记网络）地址。
type Input struct {
	Address        string
	CurrentNetwork string
	DeviceSiteID   string
	Networks       []Network
	Sites          []*models.Site
}

// Suggestion 是一条可展示的候选网络，只代表网段覆盖，不代表已写入。
type Suggestion struct {
	NetworkName  string
	NetworkRange string
}

// Suggestions 返回地址可以确认归属的全部候选网络。域名地址、未登记网
// 段、非法网段、网段主机位不为零、地址家族不同、当前行已归属的网络，以
// 及设备地点不在覆盖范围内的物理局域网都不会返回；不同网络登记相同网段
// 时逐个返回，由 owner 选择其中一个。
func Suggestions(input Input) []Suggestion {
	address, err := netip.ParseAddr(strings.TrimSpace(input.Address))
	if err != nil {
		return nil
	}
	currentNetwork := strings.TrimSpace(input.CurrentNetwork)
	siteTree := models.NewSiteTree(input.Sites)
	var suggestions []Suggestion
	for _, network := range input.Networks {
		network.Name = strings.TrimSpace(network.Name)
		if network.Name == "" || network.Name == currentNetwork {
			continue
		}
		prefix, err := netip.ParsePrefix(strings.TrimSpace(network.Range))
		if err != nil || prefix.Addr() != prefix.Masked().Addr() || !prefix.Contains(address) {
			continue
		}
		if network.Physical &&
			(input.DeviceSiteID == "" || network.SiteID == "" ||
				!siteTree.Covers(network.SiteID, input.DeviceSiteID)) {
			continue
		}
		suggestions = append(suggestions, Suggestion{
			NetworkName: network.Name, NetworkRange: prefix.String(),
		})
	}
	return suggestions
}
