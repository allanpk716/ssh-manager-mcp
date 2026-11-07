// Package dialdevice resolves the topology device that represents the sshmgr
// instance currently making an outgoing SSH connection.
package dialdevice

import (
	"encoding/json"
	"os"
	"path/filepath"

	"ssh-manager-mcp/internal/instname"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/paths"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
)

// Reason explains why Current cannot identify a dial device.
type Reason string

const (
	// ReasonRoleUnknown means this machine has no completed deployment role.
	ReasonRoleUnknown Reason = "role-unknown"
	// ReasonClientDeviceNotRecorded means the pinned snapshot response did not
	// carry a topology device identifier for this client instance.
	ReasonClientDeviceNotRecorded Reason = "client-device-not-recorded"
	// ReasonDeviceMissingFromSnapshot means the recorded identifier is absent
	// from the snapshot generation being queried.
	ReasonDeviceMissingFromSnapshot Reason = "device-missing-from-snapshot"
	// ReasonLocalDeviceNotSet means the owner has not set the machine-local
	// device on a standalone or authority machine.
	ReasonLocalDeviceNotSet Reason = "local-device-not-set"
	// ReasonLocalDeviceDeleted means the machine-local setting points to a
	// device that no longer exists.
	ReasonLocalDeviceDeleted Reason = "local-device-deleted"
)

// Result is the dial-device resolution result. Known is false exactly when
// Device is nil; Reason then carries one of the explicit unknown states.
type Result struct {
	Device *models.Device
	Known  bool
	Reason Reason
}

// Current returns the topology device for this sshmgr instance. A client reads
// the device identifier recorded by its latest pinned snapshot pull and resolves
// it against the supplied current snapshot store. Standalone and authority
// machines read their machine-local setting. instance is the client cache
// instance name and is ignored by the standalone and authority roles.
func Current(st *store.Store, instance string) (Result, error) {
	state, err := roles.Load()
	if err != nil {
		return Result{}, err
	}
	if state == nil {
		return unknown(ReasonRoleUnknown), nil
	}

	var deviceID string
	switch state.Role {
	case roles.RoleClient:
		deviceID = cacheDeviceIDFor(instance)
		if deviceID == "" {
			return unknown(ReasonClientDeviceNotRecorded), nil
		}
	case roles.RoleStandalone, roles.RoleServer:
		deviceID, err = roles.ReadLocalDeviceID()
		if err != nil {
			return Result{}, err
		}
		if deviceID == "" {
			return unknown(ReasonLocalDeviceNotSet), nil
		}
	default:
		return unknown(ReasonRoleUnknown), nil
	}

	device, err := deviceByID(st, deviceID)
	if err != nil {
		return Result{}, err
	}
	if device == nil {
		reason := ReasonDeviceMissingFromSnapshot
		if state.Role == roles.RoleStandalone || state.Role == roles.RoleServer {
			reason = ReasonLocalDeviceDeleted
		}
		return unknown(reason), nil
	}
	return Result{Device: device, Known: true}, nil
}

// CurrentForClientCache 从客户端缓存存储解析本机设备。裁剪快照只给发起
// 拉取的客户端设备保留设备码标识；即使调用链没有实例名，这个唯一关联也能
// 识别本机，不会误读默认实例的元数据。
func CurrentForClientCache(st *store.Store) (Result, error) {
	state, err := roles.Load()
	if err != nil {
		return Result{}, err
	}
	if state == nil {
		return unknown(ReasonRoleUnknown), nil
	}
	if state.Role != roles.RoleClient {
		return Current(st, "")
	}
	devices, err := st.ListDevices(store.DeviceFilter{})
	if err != nil {
		return Result{}, err
	}
	var associated []*models.Device
	for _, device := range devices {
		if device.ClientTokenID != "" {
			associated = append(associated, device)
		}
	}
	if len(associated) != 1 {
		return unknown(ReasonClientDeviceNotRecorded), nil
	}
	return Result{Device: associated[0], Known: true}, nil
}

func unknown(reason Reason) Result {
	return Result{Known: false, Reason: reason}
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

// cacheDeviceIDFor 读取实例 cache.meta.json 里记录的拓扑设备标识。等价的
// 客户端缓存辅助函数位于依赖图上层；本机设备解析必须位于服务器代理层之
// 下，统一拨号入口才能使用它而不形成循环依赖。
func cacheDeviceIDFor(instance string) string {
	if instance != "" {
		if err := instname.Valid(instance); err != nil {
			return ""
		}
	}
	dir := os.Getenv("SSHMGR_CACHE_DIR")
	if dir == "" {
		base, err := paths.UserConfigDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(base, "ssh-manager")
		if instance != "" {
			dir = filepath.Join(dir, "instances", instance)
		}
	}
	blob, err := os.ReadFile(filepath.Join(dir, "cache.meta.json"))
	if err != nil {
		return ""
	}
	var meta struct {
		DeviceID string `json:"device_id"`
	}
	if json.Unmarshal(blob, &meta) != nil {
		return ""
	}
	return meta.DeviceID
}
