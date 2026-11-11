// Package sharemanage 提供与界面无关的共享存储 owner 管理模块。命令行与
// Web UI 都调用这里；远端执行复用共享存储挂载领域服务的统一拨号与提权
// 路径。
package sharemanage

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sharemount"
	"ssh-manager-mcp/internal/store"
)

var ErrWindowsUnmountUnsupported = errors.New(
	"Windows 服务器不能执行共享存储卸载：sshmgr 不在其上留下任何持久改动")

// Manager 是共享存储 owner 管理模块的入口，提供卸载、撤销、重选地址与
// 残留检查四个操作。
type Manager struct {
	store   *store.Store
	mounter *sharemount.Service
}

// New 返回挂在一个已解锁可写存储上的共享存储管理模块。
func New(st *store.Store) *Manager {
	return &Manager{store: st, mounter: &sharemount.Service{Store: st}}
}

// UnmountRequest 是一次卸载请求。Share 与 Server 都接受登记名或标识符。
type UnmountRequest struct {
	Share  string
	Server string
	Force  bool
}

// UnmountResult 是一次卸载的安全结论，不包含共享地址、账号或密码。
type UnmountResult struct {
	ShareName     string
	ServerID      string
	ServerName    string
	Result        string
	Version       int64
	Force         bool
	Notices       []string
	BusyProcesses []sharemount.BusyProcess
}

// Unmount 删除一台 Linux 或 macOS 服务器上的共享存储配置，再卸载挂载
// 点。服务器不在可挂载范围或共享登记已删除时，远端会留下撤销标记。
func (m *Manager) Unmount(ctx context.Context, request UnmountRequest) (*UnmountResult, error) {
	if m == nil || m.store == nil {
		return nil, errManagerWithoutStore
	}
	server, err := m.store.ResolveServer(request.Server)
	if err != nil {
		return nil, err
	}
	if server == nil {
		return nil, fmt.Errorf("服务器条目 %q 不存在", request.Server)
	}
	shareName := strings.TrimSpace(request.Share)
	share, shareErr := m.store.GetSharedStorage(shareName)
	if shareErr != nil && !errors.Is(shareErr, store.ErrSharedStorageNotFound) {
		return nil, shareErr
	}
	version := int64(0)
	inRange := false
	if share != nil {
		shareName = share.Name
		version = share.Version
		inRange = sharedStorageHasMountableServer(share, server.ID)
	}
	if models.NormalizeShellType(string(server.ShellType)).IsWindows() {
		_ = m.writeUnmountAudit(server.Name, shareName, version, request.Force,
			"windows-unsupported", true)
		return nil, ErrWindowsUnmountUnsupported
	}
	shareID := ""
	if share != nil {
		shareID = share.ID
	}

	remote, remoteErr := m.mounter.Unmount(ctx, sharemount.UnmountRequest{
		Server: server, ShareName: shareName, ShareID: shareID, Version: version,
		Force: request.Force, WriteRevokedMarker: share == nil || !inRange,
	})
	result := &UnmountResult{
		ShareName: shareName, ServerID: server.ID, ServerName: server.Name,
		Force: request.Force,
	}
	if remote != nil {
		result.Result = remote.Result
		result.Version = remote.Version
		result.BusyProcesses = remote.BusyProcesses
		if inRange && remote.Result == "success" {
			result.Notices = append(result.Notices,
				"服务器仍在可挂载范围内，agent 之后仍可能重新挂载；彻底撤下请使用 shares revoke")
		}
	}
	if remoteErr != nil {
		_ = m.writeUnmountAudit(server.Name, shareName, version,
			request.Force, unmountAuditResult(remoteErr, remote), true)
		return nil, remoteErr
	}
	if err := m.writeUnmountAudit(server.Name, shareName, remote.Version,
		request.Force, remote.Result, false); err != nil {
		return nil, err
	}
	return result, nil
}

func sharedStorageHasMountableServer(share *models.SharedStorage, serverID string) bool {
	for _, candidate := range share.MountableServers {
		if candidate.ID == serverID {
			return true
		}
	}
	return false
}

func (m *Manager) writeUnmountAudit(
	serverName, shareName string, version int64, force bool, result string, failed bool,
) error {
	summary := map[string]any{
		"share": shareName, "server": serverName, "result": result, "force": force,
	}
	if version > 0 {
		summary["version"] = version
	}
	if failed {
		row := store.OwnerAuditRow("share-unmount", summary)
		row.Status = "error"
		return m.store.WriteAudit(row)
	}
	return m.store.WriteOwnerAudit("share-unmount", summary)
}

func unmountAuditResult(err error, remote *sharemount.UnmountResult) string {
	switch {
	case errors.Is(err, ErrWindowsUnmountUnsupported):
		return "windows-unsupported"
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
