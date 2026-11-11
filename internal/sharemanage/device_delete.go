package sharemanage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sharemount"
	"ssh-manager-mcp/internal/store"
)

// DeviceDeleteCascadingRequest 是一次提供方设备删除请求。Confirmed 是
// owner 已确认的删除预览；远端目标在删除事务开始前从这份预览补齐。
type DeviceDeleteCascadingRequest struct {
	Device    string
	Confirmed *store.DeviceDeletePreview
}

// DeviceDeleteShareServerResult 是一台 Linux 或 macOS 服务器上执行共享存储
// 远端撤销的安全结论，不包含共享地址、挂载账号或密码。
type DeviceDeleteShareServerResult struct {
	ShareName     string
	ServerID      string
	ServerName    string
	System        string
	Result        string
	Version       int64
	Failed        bool
	Error         string
	BusyProcesses []sharemount.BusyProcess
}

// DeviceDeleteCascadingResult 是设备登记删除后的远端级联汇总。登记删除
// 已提交；Results 中的失败只代表远端清理未完成。
type DeviceDeleteCascadingResult struct {
	DeviceName string
	Results    []DeviceDeleteShareServerResult
	Notices    []string
}

// DeleteDeviceCascading 先提交现有设备级联删除事务，再对预览列出的每台
// Linux 与 macOS 服务器执行远端撤销。远端失败不回滚登记删除。
func (m *Manager) DeleteDeviceCascading(
	ctx context.Context, request DeviceDeleteCascadingRequest,
) (*DeviceDeleteCascadingResult, error) {
	if m == nil || m.store == nil {
		return nil, errManagerWithoutStore
	}
	targets, err := m.deviceDeleteRemoteTargets(request.Confirmed)
	if err != nil {
		return nil, err
	}
	remoteManager, closeRemoteStore, err := m.deviceDeleteRemoteManager(request.Confirmed)
	if err != nil {
		return nil, err
	}
	if closeRemoteStore != nil {
		defer closeRemoteStore()
	}
	deleted, err := m.store.DeleteDeviceCascading(request.Device, request.Confirmed)
	if err != nil {
		return nil, err
	}

	result := &DeviceDeleteCascadingResult{DeviceName: deleted.Device.Name}
	for _, share := range deleted.SharedStorages {
		for _, mountable := range share.MountableServers {
			if models.NormalizeShellType(string(mountable.ShellType)).IsWindows() {
				continue
			}
			server := targets[mountable.ID]
			item := DeviceDeleteShareServerResult{
				ShareName: share.Name, ServerID: mountable.ID,
				ServerName: mountable.Name, Version: share.Version + 1,
			}
			remote, remoteErr := remoteManager.RevokeRemote(ctx, RevokeRemoteRequest{
				Server: server, ShareName: share.Name,
				Version: share.Version + 1, Unmount: true,
			})
			if remote != nil {
				item.System = remote.System
				item.Result = remote.Result
				item.BusyProcesses = remote.BusyProcesses
				if remote.Version > 0 {
					item.Version = remote.Version
				}
			}
			if remoteErr != nil {
				item.Failed = true
				item.Result = deviceDeleteRemoteResult(remoteErr, remote)
				item.Error = remoteErr.Error()
			}
			if item.Result == "busy" {
				item.Failed = true
			}
			result.Results = append(result.Results, item)
		}
	}
	if deviceDeleteHasRemoteFailure(result) {
		result.Notices = append(result.Notices,
			"远端清理未全部完成：请用 shares residue 检查残留；被占用的挂载可用 shares unmount --force 强制卸载")
	}
	return result, nil
}

func (m *Manager) deviceDeleteRemoteManager(
	preview *store.DeviceDeletePreview,
) (*Manager, func(), error) {
	if preview == nil || !deviceDeleteNeedsAccessSnapshot(preview) {
		return m, nil, nil
	}
	// 待清理的服务器条目也可能属于被删除的提供方设备。登记删除事务会
	// 同时删除这类条目的登录凭据、提权凭据与主机密钥锚；先做一份只在
	// 进程内保存主密钥的临时加密存储副本，事务提交后仍用原统一拨号路径。
	snapshot, err := m.store.ExportSnapshot()
	if err != nil {
		return nil, nil, err
	}
	masterKey, err := store.GenerateMasterKey()
	if err != nil {
		return nil, nil, err
	}
	directory, err := os.MkdirTemp("", "sshmgr-device-delete-")
	if err != nil {
		return nil, nil, err
	}
	databasePath := filepath.Join(directory, "remote-access.db")
	remoteStore, err := store.Open(databasePath, masterKey)
	if err != nil {
		_ = os.Remove(databasePath)
		_ = os.Remove(directory)
		return nil, nil, err
	}
	if err := remoteStore.ImportSnapshot(snapshot); err != nil {
		_ = remoteStore.Close()
		_ = os.Remove(databasePath)
		_ = os.Remove(directory)
		return nil, nil, err
	}
	cleanup := func() {
		_ = remoteStore.Close()
		_ = os.Remove(databasePath)
		_ = os.Remove(databasePath + "-wal")
		_ = os.Remove(databasePath + "-shm")
		_ = os.Remove(databasePath + "-journal")
		_ = os.Remove(directory)
	}
	return New(remoteStore), cleanup, nil
}

func deviceDeleteNeedsAccessSnapshot(preview *store.DeviceDeletePreview) bool {
	deletedIDs := make(map[string]bool, len(preview.Servers))
	for _, server := range preview.Servers {
		deletedIDs[server.ID] = true
	}
	for _, share := range preview.SharedStorages {
		for _, mountable := range share.MountableServers {
			if deletedIDs[mountable.ID] &&
				!models.NormalizeShellType(string(mountable.ShellType)).IsWindows() {
				return true
			}
		}
	}
	return false
}

func (m *Manager) deviceDeleteRemoteTargets(
	preview *store.DeviceDeletePreview,
) (map[string]*models.Server, error) {
	targets := make(map[string]*models.Server)
	if preview == nil {
		return targets, nil
	}
	for _, share := range preview.SharedStorages {
		for _, mountable := range share.MountableServers {
			if models.NormalizeShellType(string(mountable.ShellType)).IsWindows() ||
				targets[mountable.ID] != nil {
				continue
			}
			server, err := m.store.ResolveServer(mountable.ID)
			if err != nil {
				return nil, err
			}
			if server == nil || server.ID != mountable.ID {
				return nil, fmt.Errorf("共享存储 %s 的可挂载服务器 %s 不存在，设备未删除",
					share.Name, mountable.Name)
			}
			targets[mountable.ID] = server
		}
	}
	return targets, nil
}

func deviceDeleteRemoteResult(
	err error, remote *sharemount.UnmountResult,
) string {
	switch {
	case errors.Is(err, sharemount.ErrUnmountSSH):
		return "ssh-error"
	case errors.Is(err, sharemount.ErrNoSudo):
		return "missing-sudo"
	case errors.Is(err, sharemount.ErrUnmountUnsupportedSystem):
		return "unsupported-system"
	case errors.Is(err, sharemount.ErrUnmountNotManaged):
		return "not-managed"
	case errors.Is(err, sharemount.ErrUnmountLockTimeout):
		return "lock-timeout"
	case errors.Is(err, sharemount.ErrUnmountFailed):
		return "unmount-failed"
	case errors.Is(err, sharemount.ErrUnmountOutputInvalid):
		return "output-invalid"
	case errors.Is(err, sharemount.ErrUnmountRemoteVersionNewer):
		return "remote-version-newer"
	case remote != nil && remote.Result != "":
		return remote.Result
	default:
		return "error"
	}
}

func deviceDeleteHasRemoteFailure(result *DeviceDeleteCascadingResult) bool {
	for _, item := range result.Results {
		if item.Failed {
			return true
		}
	}
	return false
}
