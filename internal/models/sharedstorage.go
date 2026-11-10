package models

import "time"

// SharedStorageDirectory 是共享存储内部的一条目录约定：用途对应共享内部
// 子路径。列表顺序就是 owner 登记的顺序。
type SharedStorageDirectory struct {
	Purpose string `json:"purpose"`
	Path    string `json:"path"`
}

// SharedStorageServer 是一台能挂载某个共享存储的服务器条目，携带按系统
// 类型推算出的挂载点。
type SharedStorageServer struct {
	ID         string
	Name       string
	DeviceName string
	ShellType  ShellType
	MountPoint string
}

// SharedStorage 是 owner 登记的一处网络共享。密码只保存在机密项里，这里
// 只有机密项的名字；候选访问地址来自提供方设备的网络成员。
type SharedStorage struct {
	ID                 string
	Name               string
	DeviceID           string
	DeviceName         string
	MountUser          string
	PasswordSecretID   string
	PasswordSecretName string
	Drive              string
	Directories        []SharedStorageDirectory
	Note               string
	Version            int64
	ChangeNote         string
	CandidateAddresses []string
	MountableServers   []SharedStorageServer
	CreatedAt          time.Time
	UpdatedAt          time.Time
}
