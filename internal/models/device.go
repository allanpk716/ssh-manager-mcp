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

// DeviceServer 是设备详情里的服务器条目摘要，只携带 owner 需要识别入口
// 与判断主机密钥锚状态的字段。
type DeviceServer struct {
	ID     string
	Name   string
	User   string
	Port   int
	Pinned bool
	// ExposeHost 表示是否向 agent 披露这台服务器条目的主机地址。
	ExposeHost bool
}

// NetworkMembership 是一台设备在某个网络里的身份及地址。地址可以是 IP 或
// 域名，登记时不解析、不校验格式。
type NetworkMembership struct {
	DeviceID          string
	DeviceName        string
	NetworkID         string
	NetworkName       string
	NetworkType       NetworkType
	SiteName          string
	Address           string
	RegistrationOrder int64
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// DeviceLayoutCoordinate 是拓扑图某个视图里一台设备的坐标。视图用字符串
// 键区分，坐标由拓扑图页面写入；拓扑图页面与设备删除、备份导出读取。
type DeviceLayoutCoordinate struct {
	ViewKey   string
	DeviceID  string
	X         float64
	Y         float64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ContainerLayoutCoordinate 是拓扑图某个视图里一个站点区域或网络分组框的
// 坐标。EntityType 是 site（站点区域）或 network（网络分组框）；坐标由拓
// 扑图页面写入。
type ContainerLayoutCoordinate struct {
	ViewKey    string
	EntityType string
	EntityID   string
	X          float64
	Y          float64
	CreatedAt  time.Time
	UpdatedAt  time.Time
}
