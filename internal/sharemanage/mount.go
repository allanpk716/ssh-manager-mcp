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

var ErrWindowsMountUnsupported = errors.New(
	"Windows 服务器不能持久挂载共享存储；盘符由每条命令执行前的会话内注入提供，不需要单独挂载")

var ErrMountServerNotWritable = errors.New(
	"服务器不在共享存储的可挂载范围内，请先在详情页勾选这台服务器")

// Source 描述一次共享存储远端操作来自哪里：命令行，或 Web UI（附来源地
// 址）。模块按它给审计行填来源列。
type Source struct {
	kind string
	addr string
}

func CLISource() Source { return Source{kind: store.AuditSourceCLI} }

func WebUISource(addr string) Source {
	return Source{kind: store.AuditSourceWebUI, addr: addr}
}

// MountRequest 是一次单台服务器幂等挂载请求。Share 与 Server 都接受登记
// 名或标识符。
type MountRequest struct {
	Share  string
	Server string
	Source Source
}

// MountResult 是一次挂载后的安全结论，不包含地址、挂载账号或密码。
type MountResult struct {
	ShareName       string
	ServerID        string
	ServerName      string
	Version         int64
	MountPoint      string
	Network         string
	NetworkSequence int
	Action          string
	Notices         []string
}

// Mount 在一台 Linux 或 macOS 服务器上执行 agent 挂载工具使用的同一套幂
// 等挂载服务，并把结果写 owner 审计。
func (m *Manager) Mount(ctx context.Context, request MountRequest) (*MountResult, error) {
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
	share, err := m.sharedStorage(request.Share)
	if err != nil {
		return nil, err
	}
	if !sharedStorageHasMountableServer(share, server.ID) {
		_ = m.writeMountAudit(share, server, nil, ErrMountServerNotWritable, request.Source)
		return nil, ErrMountServerNotWritable
	}
	if models.NormalizeShellType(string(server.ShellType)).IsWindows() {
		_ = m.writeMountAudit(share, server, nil, ErrWindowsMountUnsupported, request.Source)
		return nil, ErrWindowsMountUnsupported
	}
	secretValue, err := m.store.SecretValueForSharedStorageMount(share.PasswordSecretID)
	if err != nil {
		err = errors.New("读取挂载密码机密项失败")
		_ = m.writeMountAudit(share, server, nil, err, request.Source)
		return nil, err
	}
	writable := false
	for _, serverID := range secretValue.WritableServerIDs {
		if serverID == server.ID {
			writable = true
			break
		}
	}
	if !writable || len(secretValue.Value) == 0 {
		_ = m.writeMountAudit(share, server, nil, ErrMountServerNotWritable, request.Source)
		return nil, ErrMountServerNotWritable
	}

	remote, err := m.mounter.Mount(ctx, sharemount.Request{
		Share: share, Server: server, Password: append([]byte(nil), secretValue.Value...),
	})
	if err != nil {
		_ = m.writeMountAudit(share, server, remote, err, request.Source)
		return nil, err
	}
	result := &MountResult{
		ShareName: share.Name, ServerID: server.ID, ServerName: server.Name,
		Version: share.Version, MountPoint: remote.MountPoint,
		Network: remote.Network, NetworkSequence: remote.NetworkSequence,
		Action: remote.Action, Notices: mountNotices(remote),
	}
	if err := m.writeMountAudit(share, server, remote, nil, request.Source); err != nil {
		return nil, err
	}
	return result, nil
}

func mountNotices(remote *sharemount.Result) []string {
	if remote == nil {
		return nil
	}
	notes := append([]string(nil), remote.Notices...)
	if !remote.Physical {
		var physicalFailures []string
		for _, attempt := range remote.Attempts {
			if attempt.Physical && !attempt.OK {
				physicalFailures = append(
					physicalFailures, sharemount.FormatProbeAttemptFailure(attempt))
			}
		}
		if len(physicalFailures) > 0 {
			notes = append(notes,
				fmt.Sprintf("已使用虚拟局域网：%s 第%d个地址", remote.Network, remote.NetworkSequence),
				"物理局域网地址不通："+strings.Join(physicalFailures, "；"))
		}
	}
	return notes
}

func (m *Manager) writeMountAudit(
	share *models.SharedStorage, server *models.Server, remote *sharemount.Result,
	operationErr error, source Source,
) error {
	summary := map[string]any{
		"share": share.Name, "server": server.Name, "version": share.Version,
		"result": mountAuditResult(operationErr, remote),
	}
	if remote != nil {
		summary["action"] = remote.Action
		summary["network_name"] = remote.Network
		summary["network_sequence"] = remote.NetworkSequence
	}
	row := store.OwnerAuditRow("share-mount", summary)
	row.Source = source.kind
	row.SourceIP = source.addr
	if operationErr != nil {
		row.Status = "error"
	}
	return m.store.WriteAudit(row)
}

func mountAuditResult(err error, remote *sharemount.Result) string {
	if err == nil && remote != nil {
		if remote.Action == "no-change" {
			return "no-change"
		}
		return "success"
	}
	switch {
	case errors.Is(err, ErrWindowsMountUnsupported):
		return "windows-unsupported"
	case errors.Is(err, ErrMountServerNotWritable):
		return "not-in-writable-servers"
	}
	return "error"
}
