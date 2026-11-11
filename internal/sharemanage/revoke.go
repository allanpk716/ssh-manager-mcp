package sharemanage

import (
	"context"
	"errors"
	"fmt"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sharemount"
	"ssh-manager-mcp/internal/store"
)

var errManagerWithoutStore = errors.New("共享存储管理模块没有绑定存储")

// RevokeRequest 是一次共享存储撤销请求。Share 与 Server 都接受登记名或
// 标识符；Unmount 控制登记变更提交后是否清理远端现有挂载。
type RevokeRequest struct {
	Share   string
	Server  string
	Unmount bool
	Force   bool
	// NewPassword 为空指针表示不换密码；非空时与移出可挂载范围在同一
	// 事务里提交，绝不进入结构化结果。
	NewPassword []byte
}

// RevokeResult 是撤销登记与远端清理的安全结论，不包含共享地址、挂载账号
// 或密码。
type RevokeResult struct {
	ShareName       string
	ServerID        string
	ServerName      string
	System          string
	Result          string
	Version         int64
	Unmount         bool
	Force           bool
	MountPreserved  bool
	PasswordChanged bool
	// RemainingMountServers 是撤销后仍可挂载的 Linux 与 macOS 服务
	// 器名，它们需要等 agent 按新版本号重新挂载后才会使用新密码。
	RemainingMountServers []string
	BusyProcesses         []sharemount.BusyProcess
	Notices               []string
}

// RevokeRemoteRequest 是登记已提交后的远端撤销步骤。删除提供方设备的
// 级联流程可以直接复用这个入口，不需要再改挂载密码的可写入服务器列表。
type RevokeRemoteRequest struct {
	Server    *models.Server
	ShareName string
	Version   int64
	Unmount   bool
	Force     bool
}

// Revoke 先把服务器移出共享存储的可挂载范围，再按新版本号清理 Linux 或
// macOS 服务器上的持久配置。Windows 服务器不留持久配置，因此不发起连接。
func (m *Manager) Revoke(ctx context.Context, request RevokeRequest) (*RevokeResult, error) {
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
	share, err := m.store.GetSharedStorage(request.Share)
	if err != nil {
		return nil, err
	}
	inRange := false
	for _, candidate := range share.MountableServers {
		if candidate.ID == server.ID {
			inRange = true
			break
		}
	}
	if !inRange {
		return nil, fmt.Errorf("服务器条目 %s 不在共享存储 %s 的可挂载范围内",
			server.Name, share.Name)
	}

	var revoked *models.SharedStorage
	if request.NewPassword == nil {
		revoked, err = m.store.RevokeSharedStorageMount(
			share.ID, server.ID, request.Unmount, request.Force, store.CLISource())
	} else {
		revoked, err = m.store.RevokeSharedStorageMountAndPassword(
			share.ID, server.ID, request.Unmount, request.Force, request.NewPassword,
			store.CLISource())
	}
	if err != nil {
		return nil, err
	}
	result := &RevokeResult{
		ShareName: revoked.Name, ServerID: server.ID, ServerName: server.Name,
		Version: revoked.Version, Unmount: request.Unmount, Force: request.Force,
		PasswordChanged: request.NewPassword != nil,
	}
	for _, candidate := range revoked.MountableServers {
		if !models.NormalizeShellType(string(candidate.ShellType)).IsWindows() {
			result.RemainingMountServers = append(result.RemainingMountServers, candidate.Name)
		}
	}
	if result.PasswordChanged {
		result.Notices = append(result.Notices, "桌面上手动添加的共享连接也需要更换密码")
	}
	if models.NormalizeShellType(string(server.ShellType)).IsWindows() {
		result.System = "Windows"
		result.Result = "success"
		result.Notices = append(result.Notices, windowsRevokeNotice())
		return result, nil
	}

	remote, err := m.RevokeRemote(ctx, RevokeRemoteRequest{
		Server: server, ShareName: revoked.Name, Version: revoked.Version,
		Unmount: request.Unmount, Force: request.Force,
	})
	if remote != nil {
		result.System = remote.System
		result.Result = remote.Result
		result.BusyProcesses = remote.BusyProcesses
		if !request.Unmount && remote.Result == "success" {
			result.MountPreserved = true
		}
	}
	if err != nil {
		if errors.Is(err, sharemount.ErrUnmountSSH) {
			return nil, fmt.Errorf(
				"%w\n登记变更已保留；远端未能清理，请用 shares residue 检查残留", err)
		}
		return nil, err
	}
	return result, nil
}

// RevokeRemote 在服务器级挂载锁内删除远端配置、写撤销标记，并按请求决定
// 是否卸载。远端版本号更新时停止，不改动服务器现场。
func (m *Manager) RevokeRemote(
	ctx context.Context, request RevokeRemoteRequest,
) (*sharemount.UnmountResult, error) {
	if m == nil || m.mounter == nil {
		return nil, errManagerWithoutStore
	}
	return m.mounter.Unmount(ctx, sharemount.UnmountRequest{
		Server: request.Server, ShareName: request.ShareName, Version: request.Version,
		Force: request.Force, WriteRevokedMarker: true,
		SkipUnmount: !request.Unmount, RejectRemoteVersionNewer: true,
	})
}

func windowsRevokeNotice() string {
	return "Windows 服务器不留共享存储配置；各客户端同步快照后停止注入，在线客户端默认 5 分钟内生效，正在执行的命令保留盘符直到结束；长期离线的客户端同步前仍会注入；真正的撤销手段是更换共享密码"
}
