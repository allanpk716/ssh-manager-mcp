package models

import (
	"fmt"
	"time"
)

// Site 是网络拓扑里的物理场所，物理局域网必须归属且只归属一个站点。
type Site struct {
	ID        string
	Name      string
	Note      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NetworkType 表示网络的登记类型：物理局域网必须归属一个站点，虚拟局域
// 网不属于任何站点。类型登记后不可修改。
type NetworkType string

const (
	NetworkPhysical NetworkType = "physical"
	NetworkVirtual  NetworkType = "virtual"
)

// ParseNetworkType 解析命令行给出的网络类型，只接受物理局域网与虚拟局
// 域网的存储代号。
func ParseNetworkType(value string) (NetworkType, error) {
	switch NetworkType(value) {
	case NetworkPhysical:
		return NetworkPhysical, nil
	case NetworkVirtual:
		return NetworkVirtual, nil
	default:
		return "", fmt.Errorf("未知网络类型 %q；只接受 physical（物理局域网）或 virtual（虚拟局域网）", value)
	}
}

// Network 是网络拓扑里的一个地址唯一性边界。网段只给人看，不参与判断。
type Network struct {
	ID            string
	Name          string
	Type          NetworkType
	SiteID        string
	SiteName      string
	Note          string
	NetworkRange  string
	BandwidthMbps int64
	Metered       bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
}
