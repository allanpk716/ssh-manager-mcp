package models

// NetworkAddressCandidate 是一台设备的一个候选连接地址与它的来源级别，
// 供首选网络地址等读取口径把不同来源的候选列表归一后共用同一套选取规则。
type NetworkAddressCandidate struct {
	Address string
	Source  DialAddressSource
}

// PreferredNetworkAddress 返回候选列表里第一个来自物理局域网或虚拟局域
// 网的成员地址；没有这样的候选时返回空串。列表必须保持候选规划的连接优
// 先级排序（物理局域网、虚拟局域网在前，服务器条目的其他地址在后）；
// 其他地址不会被选为首选网络地址。
func PreferredNetworkAddress(candidates []NetworkAddressCandidate) string {
	for _, candidate := range candidates {
		if candidate.Source != DialAddressSourcePhysical && candidate.Source != DialAddressSourceVirtual {
			continue
		}
		return candidate.Address
	}
	return ""
}
