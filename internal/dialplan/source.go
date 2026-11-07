// Package dialplan 把当前存储快照与本机拨号设备整理成统一拨号入口使用的
// 网络拓扑视图。
package dialplan

import (
	"ssh-manager-mcp/internal/dialdevice"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
)

// StoreSource 每次拨号时从存储读取网络拓扑。instance 是客户端缓存实例名；
// 空值表示单机部署与权威端读取本机设备设定，或客户端缓存按裁剪快照里唯一
// 保留的设备码关联识别本机。
type StoreSource struct {
	store    *store.Store
	instance string
}

// NewStoreSource 返回绑定到指定存储的拓扑来源；这个存储可以是当前热更新
// 后的客户端缓存存储。
func NewStoreSource(st *store.Store, instance string) *StoreSource {
	return &StoreSource{store: st, instance: instance}
}

// ServerDialTopology 从当前存储代解析目标设备与本机设备的网络成员。
func (s *StoreSource) ServerDialTopology(server *models.Server) (sshbroker.DialTopology, error) {
	if s == nil || s.store == nil || server == nil || server.DeviceID == "" {
		return sshbroker.DialTopology{}, nil
	}
	target, err := deviceByID(s.store, server.DeviceID)
	if err != nil || target == nil {
		return sshbroker.DialTopology{}, err
	}
	targetMemberships, err := s.store.ListNetworkMembershipsByRegistration(target.Name)
	if err != nil {
		return sshbroker.DialTopology{}, err
	}

	var current dialdevice.Result
	if s.instance != "" {
		current, err = dialdevice.Current(s.store, s.instance)
	} else {
		current, err = dialdevice.CurrentForClientCache(s.store)
	}
	if err != nil {
		return sshbroker.DialTopology{}, err
	}
	if !current.Known || current.Device == nil {
		return sshbroker.DialTopology{TargetMemberships: targetMemberships}, nil
	}
	dialerMemberships, err := s.store.ListNetworkMembershipsByRegistration(current.Device.Name)
	if err != nil {
		return sshbroker.DialTopology{}, err
	}
	return sshbroker.DialTopology{
		TargetMemberships: targetMemberships, DialerMemberships: dialerMemberships,
		DialerKnown: true,
	}, nil
}

func deviceByID(st *store.Store, id string) (*models.Device, error) {
	devices, err := st.ListDevices(store.DeviceFilter{})
	if err != nil {
		return nil, err
	}
	for _, device := range devices {
		if device.ID == id {
			return device, nil
		}
	}
	return nil, nil
}
