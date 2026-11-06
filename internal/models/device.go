package models

import "time"

// Device 是网络拓扑里的节点，代表一台机器。客户端实例按设备码的一对一
// 关联只保存标识符与名字；设备码本体仍是客户端登记里的凭据行。
type Device struct {
	ID            string
	Name          string
	Note          string
	ClientTokenID string
	ClientName    string
	Memberships   []*NetworkMembership
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// NetworkMembership 是一台设备在某个网络里的身份及地址。地址可以是 IP 或
// 域名，登记时不解析、不校验格式。
type NetworkMembership struct {
	DeviceID    string
	DeviceName  string
	NetworkID   string
	NetworkName string
	NetworkType NetworkType
	SiteName    string
	Address     string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}
