package dialdevice

import (
	"database/sql"
	"errors"
	"fmt"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// ResolvedOrigin 是把出发点参数解析成的一台拓扑设备。self 解析为本机设
// 备；服务器编号解析为该条目关联的设备，并保留条目本身供经服务器操作使
// 用。
type ResolvedOrigin struct {
	Device *models.Device
	// Server 是按服务器编号解析时命中的服务器条目；self 解析时为空。
	Server *models.Server
	// Remote 表示出发点不是本机设备，而是按服务器条目换算出的设备。
	Remote bool
}

// GateServerFunc 检查一个服务器编号是否在当前授权范围内；空值表示调用方
// 是 owner 命令行路径，不限制授权范围。
type GateServerFunc func(serverID string) error

// ResolveOrigin 把 self 或服务器编号解析成出发点设备。连通探测与网络拓
// 扑查询共用这一条解析规则，保证两边的授权检查与补登记提示一致。
func ResolveOrigin(st *store.Store, instance, from string, gateServer GateServerFunc) (*ResolvedOrigin, error) {
	if from == "self" {
		current, err := Current(st, instance)
		if err != nil {
			return nil, err
		}
		if !current.Known || current.Device == nil {
			return nil, SelfDeviceUnknownError(current.Reason)
		}
		return &ResolvedOrigin{Device: current.Device}, nil
	}
	if gateServer != nil {
		if err := gateServer(from); err != nil {
			return nil, err
		}
	}
	server, err := st.GetServer(from)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("服务器 %s 不存在", from)
	}
	if err != nil {
		return nil, err
	}
	if server == nil {
		return nil, fmt.Errorf("服务器 %s 不存在", from)
	}
	if server.DeviceID == "" {
		return nil, fmt.Errorf("起点服务器未关联拓扑设备，请 owner 补登记")
	}
	device, err := ByID(st, server.DeviceID)
	if err != nil {
		return nil, err
	}
	if device == nil {
		return nil, fmt.Errorf("起点服务器 %s 关联的拓扑设备已不存在，请 owner 补登记", from)
	}
	return &ResolvedOrigin{Device: device, Server: server, Remote: true}, nil
}

// SelfDeviceUnknownError 按 Current 给出的无法识别原因，生成对应的
// owner 补登记提示；客户端实例与单机部署、权威端需要的是不同的补登记动
// 作，不能共用同一句提示。
func SelfDeviceUnknownError(reason Reason) error {
	switch reason {
	case ReasonClientDeviceNotRecorded:
		return errors.New("客户端实例没有记录本机设备，请 owner 在权威端把该客户端实例关联到设备，再重新拉取缓存")
	case ReasonDeviceMissingFromSnapshot:
		return errors.New("客户端实例记录的本机设备不在当前缓存快照里，请 owner 在权威端补登记该设备，再重新拉取缓存")
	case ReasonLocalDeviceNotSet:
		return errors.New("本机未设定设备，请 owner 补登记（sshmgr devices local set）")
	case ReasonLocalDeviceDeleted:
		return errors.New("本机设备设定指向的设备已不存在，请 owner 重新设定（sshmgr devices local set）")
	default:
		return errors.New("本机部署角色未知，请 owner 先完成本机部署设置")
	}
}
