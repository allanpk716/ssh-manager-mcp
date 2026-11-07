// Package dialplan 把当前存储快照与本机拨号设备整理成统一拨号入口使用的
// 网络拓扑视图。
package dialplan

import (
	"os"
	"path/filepath"

	"ssh-manager-mcp/internal/dialdevice"
	"ssh-manager-mcp/internal/instname"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/paths"
	"ssh-manager-mcp/internal/roles"
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

var _ sshbroker.DialSuccessCacheSource = (*StoreSource)(nil)

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

// ServerDialSuccess 读取这个实例为服务器条目记录的最近成功地址。
func (s *StoreSource) ServerDialSuccess(server *models.Server) (sshbroker.DialSuccessRecord, bool, error) {
	cache, ok, err := s.successCache()
	if err != nil || !ok {
		return sshbroker.DialSuccessRecord{}, false, err
	}
	return cache.Success(server)
}

// RecordServerDialSuccess 写入服务器条目的最近成功地址。
func (s *StoreSource) RecordServerDialSuccess(server *models.Server, record sshbroker.DialSuccessRecord) error {
	cache, ok, err := s.successCache()
	if err != nil || !ok {
		return err
	}
	return cache.Record(server, record)
}

// DeleteServerDialSuccess 删除服务器条目的成功地址记录。
func (s *StoreSource) DeleteServerDialSuccess(server *models.Server, address string) error {
	cache, ok, err := s.successCache()
	if err != nil || !ok {
		return err
	}
	return cache.Delete(server, address)
}

func (s *StoreSource) successCache() (*sshbroker.FileDialSuccessCache, bool, error) {
	state, err := roles.Load()
	if err != nil {
		return nil, false, err
	}
	if state == nil {
		return nil, false, nil
	}

	var directory string
	switch state.Role {
	case roles.RoleClient:
		if s.instance != "" {
			if err := instname.Valid(s.instance); err != nil {
				return nil, false, err
			}
		}
		if override := os.Getenv("SSHMGR_CACHE_DIR"); override != "" {
			directory = override
		} else {
			base, err := paths.UserConfigDir()
			if err != nil {
				return nil, false, err
			}
			directory = filepath.Join(base, "ssh-manager")
			if s.instance != "" {
				directory = filepath.Join(directory, "instances", s.instance)
			}
		}
	case roles.RoleStandalone, roles.RoleServer:
		statePath, err := roles.LocalDevicePath()
		if err != nil {
			return nil, false, err
		}
		directory = filepath.Dir(statePath)
	default:
		return nil, false, nil
	}
	return sshbroker.NewFileDialSuccessCache(filepath.Join(directory, "ssh-dial-successes.json")), true, nil
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
