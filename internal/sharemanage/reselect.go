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

var ErrReselectOutsideMountableRange = errors.New(
	"服务器不在共享存储的可挂载范围内，重选地址不是重新授权入口")

var ErrWindowsReselectUnsupported = errors.New(
	"Windows 服务器不能重选共享存储地址：每条命令执行前本来就会重新选址")

// ReselectRequest 是一次共享存储重选地址请求。Share 与 Server 都接受登
// 记名或标识符。
type ReselectRequest struct {
	Share  string
	Server string
	Force  bool
}

// ReselectResult 是重选地址的安全结论，不包含共享地址、账号或密码。
type ReselectResult struct {
	ShareName       string
	ServerID        string
	ServerName      string
	Result          string
	Version         int64
	Force           bool
	NetworkName     string
	NetworkSequence int
	Physical        bool
	Notices         []string
	BusyProcesses   []sharemount.BusyProcess
}

// Reselect 让仍在可挂载范围内的 Linux 或 macOS 服务器重新选择共享地址。
func (m *Manager) Reselect(ctx context.Context, request ReselectRequest) (*ReselectResult, error) {
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
	share, err := m.store.GetSharedStorage(strings.TrimSpace(request.Share))
	if err != nil {
		return nil, err
	}
	inRange := sharedStorageHasMountableServer(share, server.ID)
	if !inRange {
		_ = m.writeReselectAudit(server.Name, share.Name, share.Version,
			request.Force, "outside-mountable-range", true)
		return nil, ErrReselectOutsideMountableRange
	}
	if models.NormalizeShellType(string(server.ShellType)).IsWindows() {
		_ = m.writeReselectAudit(server.Name, share.Name, share.Version,
			request.Force, "windows-unsupported", true)
		return nil, ErrWindowsReselectUnsupported
	}
	secret, err := m.store.SecretValueForSharedStorageMount(share.PasswordSecretID)
	if err != nil {
		return nil, errors.New("读取挂载密码机密项失败")
	}
	remoteResult, remoteErr := m.mounter.Reselect(ctx, sharemount.ReselectRequest{
		Server: server, Share: share, Password: append([]byte(nil), secret.Value...),
		Force: request.Force,
	})
	if remoteErr != nil {
		_ = m.writeReselectAudit(server.Name, share.Name, share.Version,
			request.Force, reselectAuditResult(remoteErr), true)
		return nil, remoteErr
	}
	result := &ReselectResult{
		ShareName: share.Name, ServerID: server.ID, ServerName: server.Name,
		Version: share.Version, Force: request.Force,
		NetworkName: remoteResult.Network, NetworkSequence: remoteResult.NetworkSequence,
		Physical:      remoteResult.Physical,
		BusyProcesses: remoteResult.BusyProcesses,
	}
	if remoteResult.Result == "no-change" {
		result.Result = "best-address-unchanged"
		result.Notices = append(result.Notices, "当前地址已是最优可达地址，未做改动")
	} else if remoteResult.Result == "busy" {
		result.Result = "busy"
	} else {
		result.Result = "success"
	}
	if err := m.writeReselectAudit(server.Name, share.Name, share.Version,
		request.Force, result.Result, false); err != nil {
		return nil, err
	}
	return result, nil
}

func (m *Manager) writeReselectAudit(
	serverName, shareName string, version int64, force bool, result string, failed bool,
) error {
	summary := map[string]any{
		"share": shareName, "server": serverName, "result": result, "force": force,
		"version": version,
	}
	if failed {
		row := store.OwnerAuditRow("share-reselect", summary)
		row.Status = "error"
		return m.store.WriteAudit(row)
	}
	return m.store.WriteOwnerAudit("share-reselect", summary)
}

func reselectAuditResult(err error) string {
	switch {
	case errors.Is(err, ErrWindowsReselectUnsupported):
		return "windows-unsupported"
	case errors.Is(err, ErrReselectOutsideMountableRange):
		return "outside-mountable-range"
	case errors.Is(err, sharemount.ErrReselectSSH):
		return "ssh-error"
	case errors.Is(err, sharemount.ErrNoSudo):
		return "missing-sudo"
	case errors.Is(err, sharemount.ErrUnsupportedSystem):
		return "unsupported-system"
	case errors.Is(err, sharemount.ErrReselectNotManaged):
		return "not-managed"
	case errors.Is(err, sharemount.ErrReselectLockTimeout):
		return "lock-timeout"
	case errors.Is(err, sharemount.ErrReselectUnmountFailed):
		return "unmount-failed"
	case errors.Is(err, sharemount.ErrReselectOutputInvalid):
		return "output-invalid"
	default:
		var selection *sharemount.SelectionFailure
		if errors.As(err, &selection) {
			return "selection-failed"
		}
		var mountFailure *sharemount.MountFailure
		if errors.As(err, &mountFailure) {
			return mountFailure.Class
		}
		return "error"
	}
}
