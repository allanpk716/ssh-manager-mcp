// Package probeobserve 把真实 SSH 连接的候选地址尝试接入本地探测结果文件，
// 并按部署角色解析探测结果文件位置。探测工具本身在 internal/probe。
package probeobserve

import (
	"path/filepath"
	"time"

	"ssh-manager-mcp/internal/clientcache"
	"ssh-manager-mcp/internal/dialdevice"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/proberesults"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
)

var _ sshbroker.DialAttemptObserver = (*DialObserver)(nil)

// DialObserver 把统一拨号入口交出的候选地址观察写进探测结果文件。起点
// 设备取当前实例登记的本机设备；当前实例没有登记设备时不记。记录失败
// 只忽略，不影响连接本身。
type DialObserver struct {
	store    *store.Store
	instance string
	results  *proberesults.FileProbeResults
}

// NewDialObserver 返回绑定到指定存储与探测结果文件的观察者。st 为 nil 时
// 无法解析本机设备，观察全部跳过。
func NewDialObserver(st *store.Store, instance, path string) *DialObserver {
	return &DialObserver{store: st, instance: instance, results: proberesults.NewFileProbeResults(path)}
}

// ObserveDialAttempt implements sshbroker.DialAttemptObserver.
func (o *DialObserver) ObserveDialAttempt(server *models.Server, candidate sshbroker.DialAddress, outcome sshbroker.DialAttemptOutcome) {
	if o == nil || o.store == nil || server == nil || server.DeviceID == "" || candidate.Source == sshbroker.DialAddressFallback {
		return
	}
	current, err := dialdevice.Current(o.store, o.instance)
	if err != nil || !current.Known || current.Device == nil {
		return
	}
	category := outcome.Category
	if outcome.OK {
		category = models.ProbeErrorNone
	}
	_ = o.results.Record(proberesults.Observation{
		OriginDeviceID: current.Device.ID, TargetDeviceID: server.DeviceID,
		Network: candidate.NetworkName, Address: candidate.Address, Port: server.Port,
		OK: outcome.OK, Category: category, Detail: outcome.Detail,
		Duration: outcome.Duration, ObservedAt: time.Now(), Source: proberesults.SourceDial,
	})
}

// PathFor 按当前部署角色解析探测结果文件位置：客户端实例放实例目录，
// 单机部署与权威端放本机状态目录（role.json 所在目录）。角色未初始化时
// 返回 false，表示当前形态没有可写位置。
func PathFor(instance string) (string, bool, error) {
	state, err := roles.Load()
	if err != nil {
		return "", false, err
	}
	if state == nil {
		return "", false, nil
	}
	switch state.Role {
	case roles.RoleClient:
		directory, err := clientcache.DirectoryFor(instance)
		if err != nil {
			return "", false, err
		}
		return filepath.Join(directory, "probe-results.json"), true, nil
	case roles.RoleStandalone, roles.RoleServer:
		statePath, err := roles.LocalDevicePath()
		if err != nil {
			return "", false, err
		}
		return filepath.Join(filepath.Dir(statePath), "probe-results.json"), true, nil
	default:
		return "", false, nil
	}
}
