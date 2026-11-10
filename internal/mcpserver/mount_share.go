package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sharemount"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
)

var errMountShareNotFound = errors.New("共享存储或服务器不存在")

// MountShareSnapshotSyncer 是客户端缓存面注入的挂载前快照保鲜能力：向
// 权威端查询快照修订号，本地落后就先同步。具体实现由客户端操作包提供
// （那里才有权威端请求入口）；nil 表示单机与权威端，挂载前没有这一步。
type MountShareSnapshotSyncer interface {
	// SyncBeforeMount 返回权威端当前的快照修订号；返回 nil 错误表示本地
	// 缓存已经达到该修订号（本来就一致，或刚完成同步）。返回错误表示无
	// 法确认或同步失败，调用方继续使用本地快照。
	SyncBeforeMount(ctx context.Context) (int64, error)
}

// MountShareInput 是 mount_share 工具的输入。除共享名与服务器编号外没有
// 任何命令或路径参数。
type MountShareInput struct {
	Share    string `json:"share" jsonschema:"share name from list_shares"`
	ServerID string `json:"server_id" jsonschema:"stable server id from list_servers"`
}

// MountSharePath 是一条目录约定在目标服务器上的完整路径。
type MountSharePath struct {
	Purpose  string `json:"purpose" jsonschema:"owner-registered purpose of the subdirectory"`
	Path     string `json:"path" jsonschema:"subdirectory path inside the SMB share"`
	FullPath string `json:"full_path" jsonschema:"absolute path on the target server"`
}

// MountShareOutput 是 mount_share 的安全输出；不包含地址、挂载账号或密码。
type MountShareOutput struct {
	MaskedOutput
	Share           string           `json:"share" jsonschema:"share name"`
	ServerID        string           `json:"server_id" jsonschema:"stable server id"`
	ServerName      string           `json:"server_name" jsonschema:"server entry name"`
	MountPoint      string           `json:"mount_point" jsonschema:"mounted path on the target server"`
	Version         int64            `json:"version" jsonschema:"shared storage registration version"`
	NetworkName     string           `json:"network_name" jsonschema:"selected registered network name"`
	NetworkSequence int              `json:"network_sequence" jsonschema:"address sequence within the selected network"`
	Result          string           `json:"result" jsonschema:"first-mount-success, update-success, remount-success, or no-change"`
	MountNotes      []string         `json:"mount_notes,omitempty" jsonschema:"safe follow-up notes; network names, registration sequence, probe error categories and durations only, never an IP address, domain name, or secret"`
	Paths           []MountSharePath `json:"paths" jsonschema:"directory agreements translated to target-server paths"`
}

// emptyMountShareOutput 返回能通过输出结构校验的空结果；报错路径也需要
// 数组而不是 JSON null。
func emptyMountShareOutput() MountShareOutput {
	return MountShareOutput{Paths: []MountSharePath{}}
}

// MountShareForProfile 按授权顺序检查共享与服务器，调用挂载领域服务，并
// 为成败各写一条不含密码与地址的审计。snapshotSyncer 非空且存储只读（客
// 户端缓存面）时，先做挂载前快照保鲜，再从同步后的存储继续；storeSource
// 是统一换快照入口的重新解析函数，保鲜成功后用它取到热加载后的存储。
func MountShareForProfile(
	ctx context.Context, st *store.Store, projectID, profileID, shareName, serverIdentifier string,
	hk sshbroker.HostKeyStore,
	snapshotSyncer MountShareSnapshotSyncer, storeSource func() *store.Store,
) (output *MountShareOutput, err error) {
	start := time.Now()
	status := "error"
	var share *models.SharedStorage
	var server *models.Server
	var mountResult *sharemount.Result
	snapshotMayBeStale := false
	var mountErr error
	defer func() {
		if err != nil && snapshotMayBeStale {
			err = fmt.Errorf("%w；挂载前未能同步权威端快照，本地快照可能过期，请确认权威端可达后重试", err)
		}
		summary := map[string]any{"share": shareName, "version": int64(0), "result": "error"}
		var serverID, networkName string
		sequence := 0
		if share != nil {
			summary["version"] = share.Version
		}
		if server != nil {
			serverID = server.ID
		}
		if mountResult != nil {
			networkName = mountResult.Network
			sequence = mountResult.NetworkSequence
			summary["result"] = mountResult.Action
			notes := mountShareResultNotes(mountResult)
			selectionNotes := mountShareSelectionNotes(mountResult)
			if len(notes) > 0 {
				summary["mount_notes"] = notes
			}
			if mountResult.Action == "first-mount-success" && len(selectionNotes) > 0 {
				summary["result"] = "virtual-network-fallback"
			} else {
				summary["result"] = mountResult.Action
			}
		}
		var selectionFailure *sharemount.SelectionFailure
		if errors.As(mountErr, &selectionFailure) {
			summary["result"] = "address-selection-error"
			summary["selection_error"] = selectionFailure.Reason
			summary["provider_networks"] = selectionFailure.ProviderNetworks
			summary["target_networks"] = selectionFailure.TargetNetworks
			summary["attempts"] = selectionFailure.Attempts
		}
		if networkName != "" {
			summary["network_name"] = networkName
			summary["network_sequence"] = sequence
		}
		_ = st.WriteAudit(store.AuditRow{
			TS: start, ProjectID: projectID, ServerID: serverID, Action: "mount-share",
			Status: status, Source: store.AuditSourceAgent,
			Command: store.MarshalAuditSummary(summary),
		})
	}()

	if st.IsReadOnly() && st.CacheRecommendedVersion() == "" {
		return nil, errSharedStorageAuthorityTooOld
	}
	st, snapshotMayBeStale = ensureMountShareSnapshotFresh(ctx, st, snapshotSyncer, storeSource)
	found, err := st.GetSharedStorage(shareName)
	if err != nil {
		if errors.Is(err, store.ErrSharedStorageNotFound) {
			return nil, errMountShareNotFound
		}
		return nil, err
	}
	profileServerIDs, err := st.ServersForProfile(profileID)
	if err != nil {
		return nil, err
	}
	visible := make(map[string]bool, len(profileServerIDs))
	for _, serverID := range profileServerIDs {
		visible[serverID] = true
	}
	shareVisible := false
	mountable := make(map[string]bool, len(found.MountableServers))
	for _, candidate := range found.MountableServers {
		mountable[candidate.ID] = true
		if visible[candidate.ID] {
			shareVisible = true
		}
	}
	if !shareVisible {
		return nil, errMountShareNotFound
	}

	server, err = st.ResolveGrantedServer(profileID, serverIdentifier)
	if err != nil || server == nil {
		return nil, errMountShareNotFound
	}
	if !mountable[server.ID] {
		return nil, errors.New("服务器不在挂载密码机密项的可写入服务器列表内，请先由 owner 把服务器加入该机密项的可写入服务器列表")
	}
	if models.NormalizeShellType(string(server.ShellType)).IsWindows() {
		return nil, errors.New("Windows 服务器不能持久挂载共享存储；网络盘符由每条命令执行前的会话内注入提供，不需要 mount_share")
	}

	secretValue, err := st.SecretValueForSharedStorageMount(found.PasswordSecretID)
	if err != nil {
		return nil, errors.New("读取挂载密码机密项失败")
	}
	password := secretValue.Value
	writable := false
	for _, serverID := range secretValue.WritableServerIDs {
		if serverID == server.ID {
			writable = true
			break
		}
	}
	if !writable || len(password) == 0 {
		return nil, errors.New("服务器不在挂载密码机密项的可写入服务器列表内，请先由 owner 把服务器加入该机密项的可写入服务器列表")
	}

	service := &sharemount.Service{
		Store: st, Instance: bridgeCacheInstance(), HostKeys: hk,
	}
	share = found
	mountResult, err = service.Mount(ctx, sharemount.Request{
		Share: found, Server: server, Password: append([]byte(nil), password...),
	})
	if err != nil {
		mountErr = err
		return nil, err
	}
	status = "ok"
	output = &MountShareOutput{
		Share: found.Name, ServerID: server.ID, ServerName: server.Name,
		MountPoint: mountResult.MountPoint, Version: found.Version,
		NetworkName: mountResult.Network, NetworkSequence: mountResult.NetworkSequence,
		Result: mountResult.Action, MountNotes: mountShareResultNotes(mountResult),
		Paths: []MountSharePath{},
	}
	for _, directory := range found.Directories {
		output.Paths = append(output.Paths, MountSharePath{
			Purpose: directory.Purpose, Path: directory.Path,
			FullPath: path.Join(mountResult.MountPoint, directory.Path),
		})
	}
	return output, nil
}

// mountShareResultNotes 合并选地址降级说明与重复挂载的安全提示，审计与
// 工具输出使用同一份文本。
func mountShareResultNotes(result *sharemount.Result) []string {
	notes := mountShareSelectionNotes(result)
	return append(notes, result.Notices...)
}

// ensureMountShareSnapshotFresh 在客户端上执行挂载前的快照保鲜：先让同步
// 器确认本地缓存没有落后于权威端，再经统一换快照入口重新解析当前存储，
// 让这次挂载（含共享登记、授权与挂载密码）全部来自同步后的快照。返回的
// 第二个值表示是否无法确认本地快照仍与权威端一致；单机、权威端与同步失
// 败的客户端都继续使用传入的本地快照。
func ensureMountShareSnapshotFresh(
	ctx context.Context, st *store.Store,
	syncer MountShareSnapshotSyncer, resolveStore func() *store.Store,
) (*store.Store, bool) {
	if syncer == nil || st == nil || !st.IsReadOnly() {
		return st, false
	}
	remoteRevision, err := syncer.SyncBeforeMount(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sshmgr: mount_share 快照保鲜失败（继续使用本地快照）：%v\n", err)
		return st, true
	}
	if resolveStore == nil {
		return st, true
	}
	next := resolveStore()
	if next == nil {
		return st, true
	}
	revision, err := next.SnapshotRevision()
	if err != nil || revision < remoteRevision {
		fmt.Fprintf(os.Stderr, "sshmgr: mount_share 同步后的快照尚未装入内存（继续使用当前快照）：%v\n", err)
		return st, true
	}
	return next, false
}

// mountShareSelectionNotes 渲染成功但放弃物理局域网时的说明。它沿用连通
// 探测的逐地址格式，只使用网络名、登记序号、错误分类和耗时。
func mountShareSelectionNotes(result *sharemount.Result) []string {
	if result == nil || result.Physical {
		return nil
	}
	physicalFailures := make([]string, 0)
	for _, attempt := range result.Attempts {
		if !attempt.Physical || attempt.OK {
			continue
		}
		line := fmt.Sprintf("%s 第%d个地址  ✗%s", attempt.NetworkName,
			attempt.NetworkSequence, models.ProbeErrorCategoryLabel(attempt.ErrorCategory))
		if attempt.DurationMilli >= 0 {
			line += fmt.Sprintf(" %dms", attempt.DurationMilli)
		}
		physicalFailures = append(physicalFailures, line)
	}
	if len(physicalFailures) == 0 {
		return nil
	}
	return []string{
		fmt.Sprintf("已使用虚拟局域网：%s 第%d个地址", result.Network, result.NetworkSequence),
		"物理局域网地址不通：" + strings.Join(physicalFailures, "；"),
	}
}
