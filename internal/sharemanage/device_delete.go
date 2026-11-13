package sharemanage

import (
	"context"
	"fmt"
	"strings"

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
	deletedServerIDs := deviceDeleteDeletedServerIDs(request.Confirmed)
	targets, err := m.deviceDeleteRemoteTargets(request.Confirmed, deletedServerIDs)
	if err != nil {
		return nil, err
	}
	deleted, err := m.store.DeleteDeviceCascading(request.Device, request.Confirmed)
	if err != nil {
		return nil, err
	}

	result := &DeviceDeleteCascadingResult{DeviceName: deleted.Device.Name}
	skippedCleanupCommands := make([]string, 0)
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
			if deletedServerIDs[mountable.ID] {
				item.Result = "not-cleaned"
				item.Failed = true
				item.Error = "服务器条目随设备一起删除，删除后无法再连接"
				skippedCleanupCommands = append(skippedCleanupCommands,
					fmt.Sprintf("shares unmount %s %s", share.Name, mountable.Name))
				_ = m.writeRevokeRemoteAudit(mountable.Name, share.Name, item.Version,
					true, false, item.Result, true, CLISource())
				result.Results = append(result.Results, item)
				continue
			}
			remote, remoteErr := m.RevokeRemote(ctx, RevokeRemoteRequest{
				Server: server, ShareName: share.Name, ShareID: share.ID,
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
				item.Result = unmountAuditResult(remoteErr, remote)
				item.Error = remoteErr.Error()
			}
			if item.Result == "busy" {
				item.Failed = true
			}
			_ = m.writeRevokeRemoteAudit(mountable.Name, share.Name, item.Version,
				true, false, item.Result, item.Failed, CLISource())
			result.Results = append(result.Results, item)
		}
	}
	if len(skippedCleanupCommands) > 0 {
		result.Notices = append(result.Notices,
			"有服务器条目随设备一起删除，本次未做远端清理；如需清理，先运行 "+
				strings.Join(skippedCleanupCommands, "、")+" 再删除设备")
	}
	if deviceDeleteHasRemoteFailure(result) {
		result.Notices = append(result.Notices,
			"远端清理未全部完成：请用 shares residue 检查残留；被占用的挂载可用 shares unmount --force 强制卸载")
	}
	return result, nil
}

func deviceDeleteDeletedServerIDs(preview *store.DeviceDeletePreview) map[string]bool {
	deletedIDs := make(map[string]bool)
	if preview == nil {
		return deletedIDs
	}
	for _, server := range preview.Servers {
		deletedIDs[server.ID] = true
	}
	return deletedIDs
}

func (m *Manager) deviceDeleteRemoteTargets(
	preview *store.DeviceDeletePreview, deletedServerIDs map[string]bool,
) (map[string]*models.Server, error) {
	targets := make(map[string]*models.Server)
	if preview == nil {
		return targets, nil
	}
	for _, share := range preview.SharedStorages {
		for _, mountable := range share.MountableServers {
			if models.NormalizeShellType(string(mountable.ShellType)).IsWindows() ||
				deletedServerIDs[mountable.ID] ||
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

func deviceDeleteHasRemoteFailure(result *DeviceDeleteCascadingResult) bool {
	for _, item := range result.Results {
		if item.Failed {
			return true
		}
	}
	return false
}
