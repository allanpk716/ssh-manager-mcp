package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"ssh-manager-mcp/internal/dialplan"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/shareaddress"
	"ssh-manager-mcp/internal/sharemount"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
)

const relayLocalCopyMode = "local_copy"

type relayShareFallbackMetadata struct {
	ShareName          string
	EndpointServerID   string
	EndpointServerName string
	// ShareTargetKeys 在共享地址作为目标端时携带共享级写冲突键；流式中
	// 继的既有路径把它与按服务器路径构造的键一起登记。
	ShareTargetKeys []string
}

func relayShareFallbackEndpointID(metadata *relayShareFallbackMetadata) string {
	if metadata == nil {
		return ""
	}
	return metadata.EndpointServerID
}

func relayShareFallbackNotice(metadata *relayShareFallbackMetadata) string {
	if metadata == nil {
		return ""
	}
	return fmt.Sprintf(
		"流式转发：共享地址由服务器 %s（%s）暴露；中断、停止或失败后按目标端块清单续传",
		metadata.EndpointServerName, metadata.EndpointServerID)
}

// relayShareTargetConflictKeys 构造共享地址作为目标端时的附加写冲突键。
// 键包含共享存储标识符与规范化子路径（子路径按共享地址解析的等价规则统
// 一分隔符后不区分大小写），因此两台服务器经各自挂载点写同一个共享文件
// 也会被判为同目标冲突。
func relayShareTargetConflictKeys(
	share *models.SharedStorage, resolvedSubPath string, streaming bool,
) []string {
	base := "shared-storage\x00" + share.ID + "\x00" + strings.ToLower(resolvedSubPath)
	suffixes := []string{"", relayPartialSuffix}
	if streaming {
		suffixes = append(suffixes, relayManifestSuffix, relayManifestSuffix+".tmp")
	}
	keys := make([]string, 0, len(suffixes))
	for _, suffix := range suffixes {
		keys = append(keys, base+suffix)
	}
	return keys
}

// RelayForProfile 的共享地址分支：另一端可挂载时，在这台服务器上启动固定
// 本地拷贝命令——Linux 与 macOS 要求共享已实际挂载，Windows 在同一命令会话
// 内注入固定网络盘符；另一端不可挂载时，选择已挂载端点后递归复用既有流式
// 中继。任务仍使用后台任务表与中继的运行中工件冲突键，不建立第二套任务面。
func relayLocalCopyForProfile(
	ctx context.Context, st *store.Store, tm *TaskManager, projectID, profileID string,
	in RelayInput, chunkBytes int64, hk ...sshbroker.HostKeyStore,
) (RelayOutput, error) {
	if in.Via != "" && in.FromShareAddress == "" && in.ToShareAddress == "" {
		return RelayOutput{}, errors.New("via 只能与共享地址端点一起使用")
	}
	if in.FromShareAddress != "" && (in.FromServerID != "" || in.FromPath != "") {
		return RelayOutput{}, errors.New("from_share_address、from_server_id 与 from_path 不能同时作为源端使用")
	}
	if in.ToShareAddress != "" && (in.ToServerID != "" || in.ToPath != "") {
		return RelayOutput{}, errors.New("to_share_address、to_server_id 与 to_path 不能同时作为目标端使用")
	}
	if in.FromShareAddress != "" && in.ToShareAddress != "" {
		return RelayOutput{}, errors.New("源端和目标端不能都是共享地址；请先用 query_topology 查询实际路径，再在可挂载共享的服务器上用 exec_command 拷贝")
	}
	if st.IsReadOnly() && st.CacheRecommendedVersion() == "" {
		return RelayOutput{}, errTopologyAndSharedStorageAuthorityTooOld
	}
	start := time.Now()
	status := "error"
	startOwned := false
	auditServer := ""
	shareName := ""
	var sourcePath, targetPath string
	summary := func() string {
		return fmt.Sprintf("relay local-copy share=%s %s -> %s", shareName, sourcePath, targetPath)
	}
	defer func() {
		if startOwned {
			return
		}
		_ = st.WriteAudit(store.AuditRow{
			TS: start, ProjectID: projectID, ServerID: auditServer,
			Action: "relay-bg-start", Command: summary(), Status: status,
			DurationMS: time.Since(start).Milliseconds(),
		})
	}()

	sourceShareAddress, targetShareAddress := in.FromShareAddress, in.ToShareAddress
	serverID := in.FromServerID
	ordinaryPath := in.FromPath
	if sourceShareAddress != "" {
		serverID = in.ToServerID
		ordinaryPath = in.ToPath
	}
	// 目标端是共享地址、源端省略服务器编号时，另一端是执行实例本地文件。
	// 本地端不是受管服务器，不能做本地拷贝，只能按共享端点选择规则回退到
	// 本地源流式中继；其绝对路径语义由流式中继按执行实例操作系统校验。
	localOtherEnd := targetShareAddress != "" && serverID == ""
	if sourceShareAddress != "" && (serverID == "" || ordinaryPath == "") {
		return RelayOutput{}, errors.New(strings.Join([]string{
			"源端是共享地址时，目标端必须是受管服务器上的常规文件路径",
			"relay_file 不能把文件中继到执行实例本地路径",
		}, "；"))
	}
	if !localOtherEnd && (serverID == "" || ordinaryPath == "") {
		return RelayOutput{}, errors.New("共享地址端点的另一端必须是受管服务器上的常规文件路径")
	}
	if localOtherEnd && ordinaryPath == "" {
		return RelayOutput{}, errors.New("目标端是共享地址时，源端必须给出执行实例本地文件路径或服务器路径")
	}
	var server *models.Server
	if localOtherEnd {
		ordinaryPath = filepath.Clean(ordinaryPath)
		auditServer = ""
	} else {
		if !isAbsRemotePath(ordinaryPath) {
			return RelayOutput{}, fmt.Errorf(
				"共享地址另一端的路径 %q 必须是以 / 开头的绝对路径（或 Windows 盘符根 C:/）", ordinaryPath)
		}
		ordinaryPath = path.Clean(ordinaryPath)
		auditServer = serverID

		allowed, err := st.ServersForProfile(profileID)
		if err != nil {
			return RelayOutput{}, err
		}
		if gateErr := gateServerIn(allowed, serverID); gateErr != nil {
			status = "denied"
			return RelayOutput{}, gateErr
		}
		server, err = st.GetServer(serverID)
		if err != nil || server == nil {
			return RelayOutput{}, fmt.Errorf("server %s not found", serverID)
		}
	}

	shares, err := sharedStoragesVisibleInSession(st, profileID)
	if err != nil {
		return RelayOutput{}, err
	}
	rawShareAddress := sourceShareAddress
	if targetShareAddress != "" {
		rawShareAddress = targetShareAddress
	}
	resolved, err := shareaddress.Resolve(rawShareAddress, shares)
	if err != nil {
		return RelayOutput{}, err
	}
	shareName = resolved.Share.Name
	if localOtherEnd || !sharedStorageMountableOnServer(resolved.Share, serverID) {
		endpoint, fallbackErr := selectRelayShareFallbackEndpoint(
			ctx, st, profileID, resolved.Share, in.Via, hk...)
		if fallbackErr != nil {
			return RelayOutput{}, fallbackErr
		}
		endpointPath := path.Clean(resolved.PathOnServer(endpoint.ShellType))
		metadata := &relayShareFallbackMetadata{
			ShareName: resolved.Share.Name, EndpointServerID: endpoint.ID,
			EndpointServerName: endpoint.Name,
		}
		if targetShareAddress != "" {
			metadata.ShareTargetKeys = relayShareTargetConflictKeys(
				resolved.Share, resolved.Address.SubPath, true)
		}
		relayInput := in
		relayInput.Via = ""
		if sourceShareAddress != "" {
			relayInput.FromShareAddress = ""
			relayInput.FromServerID = endpoint.ID
			relayInput.FromPath = endpointPath
		} else {
			relayInput.ToShareAddress = ""
			relayInput.ToServerID = endpoint.ID
			relayInput.ToPath = endpointPath
		}
		// 递归进入既有流式中继后，启动与终态审计由那条路径完整记录；
		// 这里不能再落一条共享地址预处理的启动审计。
		startOwned = true
		return relayForProfile(ctx, st, tm, projectID, profileID, relayInput, metadata, chunkBytes, hk...)
	}
	windowsLocalCopy := server.ShellType.IsWindows()
	if windowsLocalCopy {
		if !isWindowsDrivePath(ordinaryPath) {
			return RelayOutput{}, fmt.Errorf(
				"共享地址另一端的路径 %q 在 Windows 服务器上必须是盘符根路径（例如 C:/）", ordinaryPath)
		}
	} else if !strings.HasPrefix(ordinaryPath, "/") {
		return RelayOutput{}, fmt.Errorf(
			"共享地址另一端的路径 %q 在 Linux 或 macOS 服务器上必须是以 / 开头的绝对路径", ordinaryPath)
	}
	if in.Via != "" {
		return RelayOutput{}, errors.New("另一端可挂载共享时直接在另一端执行本地拷贝，不能再用 via 指定共享地址端点")
	}
	sharedPath := path.Clean(resolved.PathOnServer(server.ShellType))
	if windowsLocalCopy {
		sharedPath = canonicalWindowsRemotePath(sharedPath)
		ordinaryPath = canonicalWindowsRemotePath(ordinaryPath)
	}
	if sourceShareAddress != "" {
		sourcePath = sharedPath
		targetPath = ordinaryPath
	} else {
		sourcePath = ordinaryPath
		targetPath = sharedPath
	}
	if sourcePath == targetPath {
		return RelayOutput{}, fmt.Errorf(
			"源路径与目标路径相同：%s；本地拷贝不能覆盖自己的源文件", sourcePath)
	}

	auth, err := vault.AuthForServer(st, server)
	if err != nil {
		if errors.Is(err, vault.ErrNoCredential) {
			status = "no_credential"
		} else {
			status = "auth_error"
		}
		return RelayOutput{}, err
	}
	dialSource := dialplan.NewStoreSource(st, "")
	var client *sshbroker.Client
	client, connectionNotes, connectErr := sshbroker.ConnectServerEntry(ctx, sshbroker.ServerDialRequest{
		Server: server, Auth: auth, HostKeys: hostKeyStoreFor(st, hk), KeepAlive: true,
		SuccessCache: dialSource, Topology: dialSource,
	})
	transferred := false
	defer func() {
		if !transferred && client != nil {
			_ = client.Close()
		}
	}()
	if connectErr != nil {
		switch {
		case errors.Is(connectErr, context.Canceled):
			status = "cancelled"
		case errors.Is(connectErr, sshbroker.ErrHostKeyMismatch):
			status = "hostkey_mismatch"
		default:
			status = "connect_error"
		}
		return RelayOutput{}, connectErr
	}

	var sourceSize int64
	spaceCheck := "ok"
	command := buildRelayLocalCopyCommand(sourcePath, targetPath)
	if windowsLocalCopy {
		command = buildRelayWindowsLocalCopyCommand(sourcePath, targetPath)
		spaceCheck = "unavailable"
	} else {
		checkService := &sharemount.Service{
			RemoteExec: sharemount.RemoteExecOverClient(client, st, server, sharemount.MaxOutputBytes),
		}
		checkResult, checkErr := checkService.Check(ctx, sharemount.CheckRequest{
			Server: server, Shares: []*models.SharedStorage{resolved.Share},
		})
		if checkErr != nil {
			return RelayOutput{}, fmt.Errorf("共享存储 %s 的挂载现场检查失败：%w", shareName, checkErr)
		}
		if len(checkResult.States) != 1 || !checkResult.States[0].Mounted {
			return RelayOutput{}, fmt.Errorf(
				"共享存储 %s 在服务器 %s 上尚未实际挂载；请先调用幂等的 mount_share 工具，再重试 relay_file",
				shareName, server.Name)
		}

		var inspectErr error
		sourceSize, inspectErr = relayLocalCopyInspect(ctx, client, sourcePath, targetPath)
		if inspectErr != nil {
			return RelayOutput{}, inspectErr
		}

		sftpClient, sftpErr := client.RelaySFTP()
		if sftpErr != nil {
			spaceCheck = "unavailable"
		} else {
			defer sftpClient.Close()
			available, availableOK, spaceErr := client.RelayAvailable(sftpClient, path.Dir(targetPath))
			if spaceErr != nil || !availableOK {
				spaceCheck = "unavailable"
			} else if available < sourceSize {
				return RelayOutput{}, fmt.Errorf(
					"目标可用空间 %d 字节，小于源文件 %d 字节；请先清理目标空间再重试", available, sourceSize)
			}
		}
	}

	readKeys := []string{relayKeyOf(serverID, sourcePath)}
	writeKeys := []string{
		relayKeyOf(serverID, targetPath),
		relayKeyOf(serverID, targetPath+relayPartialSuffix),
	}
	if targetShareAddress != "" {
		writeKeys = append(writeKeys, relayShareTargetConflictKeys(
			resolved.Share, resolved.Address.SubPath, false)...)
	}
	spec := BgTaskSpec{
		ProjectID: projectID, ServerID: serverID, Command: summary(), Timeout: relayRunCap,
		AuditStart: func() {
			_ = st.WriteAudit(store.AuditRow{
				TS: start, ProjectID: projectID, ServerID: serverID,
				Action: "relay-bg-start", Command: summary(), Status: "ok",
				DurationMS: time.Since(start).Milliseconds(),
			})
		},
		AuditAction: "relay-bg-end",
		AuditEnd:    func(row store.AuditRow) error { return st.WriteAudit(row) },
		Run: func(taskContext context.Context, task *bgTask) {
			if windowsLocalCopy {
				runRelayWindowsLocalCopyTask(
					taskContext, tm, task, st, profileID, server, client, command,
					sourcePath, targetPath)
				return
			}
			tm.mu.Lock()
			task.client = client
			tm.mu.Unlock()
			tm.runTask(taskContext, task, client,
				func(execContext context.Context, stdout, stderr io.Writer) (int, bool, error) {
					code, timedOut, err := client.ExecWriters(execContext, command, 0, stdout, stderr)
					if err == nil && code != 0 {
						err = errors.New(relayLocalCopyExitMessage(code, targetPath))
					}
					return code, timedOut, err
				}, nil)
		},
	}
	taskID, _, reserveErr := tm.ReserveRelay(spec, readKeys, writeKeys)
	if reserveErr != nil {
		return RelayOutput{}, reserveErr
	}
	transferred = true
	startOwned = true
	return RelayOutput{
		TaskID: taskID, Mode: relayLocalCopyMode, RestartAfterInterruption: true,
		BytesTotal: sourceSize, SpaceCheck: spaceCheck,
		ConnectionNotesOutput: ConnectionNotesOutput{ConnectionNotes: connectionNotes},
	}, nil
}

// sharedStoragesVisibleInSession 返回至少有一个可挂载服务器进入当前授权
// 范围的共享存储；这是共享地址识别共用的授权裁剪口径。
func sharedStoragesVisibleInSession(st *store.Store, profileID string) ([]*models.SharedStorage, error) {
	visible, err := sessionVisibleServerIDs(st, profileID, "")
	if err != nil {
		return nil, err
	}
	allShares, err := st.ListSharedStorages(store.SharedStorageFilter{})
	if err != nil {
		return nil, err
	}
	out := make([]*models.SharedStorage, 0, len(allShares))
	for _, share := range allShares {
		if sharedStorageHasVisibleServer(share, visible) {
			out = append(out, share)
		}
	}
	return out, nil
}

func sharedStorageHasVisibleServer(share *models.SharedStorage, visible map[string]bool) bool {
	for _, mountable := range share.MountableServers {
		if visible[mountable.ID] {
			return true
		}
	}
	return false
}

func sharedStorageMountableOnServer(share *models.SharedStorage, serverID string) bool {
	for _, mountable := range share.MountableServers {
		if mountable.ID == serverID {
			return true
		}
	}
	return false
}

type relayShareEndpointCandidate struct {
	server   *models.Server
	physical bool
}

func selectRelayShareFallbackEndpoint(
	ctx context.Context, st *store.Store, profileID string,
	share *models.SharedStorage, via string, hk ...sshbroker.HostKeyStore,
) (*models.Server, error) {
	allowedIDs, err := st.ServersForProfile(profileID)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]bool, len(allowedIDs))
	for _, serverID := range allowedIDs {
		allowed[serverID] = true
	}
	if via != "" {
		return selectRelayShareFallbackEndpointByVia(ctx, st, share, via, allowed, hk...)
	}

	providerMemberships, err := st.ListNetworkMembershipsByRegistration(share.DeviceName)
	if err != nil {
		return nil, err
	}
	candidates := make([]relayShareEndpointCandidate, 0, len(share.MountableServers))
	for _, mountable := range share.MountableServers {
		if !allowed[mountable.ID] {
			continue
		}
		server, serverErr := st.GetServer(mountable.ID)
		if serverErr != nil {
			return nil, serverErr
		}
		if server == nil || server.ShellType.IsWindows() {
			continue
		}
		physical, physicalErr := relayShareEndpointOnPhysicalNetwork(st, server, providerMemberships)
		if physicalErr != nil {
			return nil, physicalErr
		}
		candidates = append(candidates, relayShareEndpointCandidate{server: server, physical: physical})
	}
	sort.SliceStable(candidates, func(left, right int) bool {
		if candidates[left].physical != candidates[right].physical {
			return candidates[left].physical
		}
		if candidates[left].server.Name != candidates[right].server.Name {
			return candidates[left].server.Name < candidates[right].server.Name
		}
		return candidates[left].server.ID < candidates[right].server.ID
	})
	for _, candidate := range candidates {
		mounted, mountedErr := relayShareActuallyMounted(ctx, st, candidate.server, share, hk...)
		if mountedErr == nil && mounted {
			return candidate.server, nil
		}
	}
	return nil, errors.New(strings.Join([]string{
		"没有可作为共享地址端点的已实际挂载 Linux 或 macOS 服务器",
		"请先在当前授权范围内可挂载这处共享的服务器上调用 mount_share，或请 owner 调整授权后重试",
		"Windows 服务器不能承担这个端点：它的共享盘符只存在于命令会话，relay_file 的独立文件传输会话看不到",
	}, "；"))
}

func selectRelayShareFallbackEndpointByVia(
	ctx context.Context, st *store.Store, share *models.SharedStorage,
	via string, allowed map[string]bool, hk ...sshbroker.HostKeyStore,
) (*models.Server, error) {
	if !allowed[via] {
		return nil, errors.New("via 指定的服务器不存在或不在当前授权范围内")
	}
	server, err := st.GetServer(via)
	if err != nil || server == nil {
		return nil, errors.New("via 指定的服务器不存在或不在当前授权范围内")
	}
	if !sharedStorageMountableOnServer(share, via) {
		return nil, fmt.Errorf(
			"via 指定的服务器 %s 不在共享存储 %s 的可挂载范围内", server.Name, share.Name)
	}
	if server.ShellType.IsWindows() {
		return nil, fmt.Errorf(
			"via 指定的服务器 %s 是 Windows；Windows 的共享盘符只存在于命令会话，relay_file 的独立文件传输会话看不到，不能作为共享地址端点",
			server.Name)
	}
	mounted, mountedErr := relayShareActuallyMounted(ctx, st, server, share, hk...)
	if mountedErr != nil {
		return nil, fmt.Errorf("via 指定的服务器 %s 现场检查失败：%w", server.Name, mountedErr)
	}
	if !mounted {
		return nil, fmt.Errorf(
			"via 指定的服务器 %s 尚未实际挂载共享存储 %s；请先调用 mount_share 再重试",
			server.Name, share.Name)
	}
	return server, nil
}

func relayShareEndpointOnPhysicalNetwork(
	st *store.Store, server *models.Server, providerMemberships []*models.NetworkMembership,
) (bool, error) {
	if server.DeviceID == "" {
		return false, nil
	}
	device, err := st.GetDeviceByID(server.DeviceID)
	if err != nil {
		return false, err
	}
	if device == nil {
		return false, nil
	}
	endpointMemberships, err := st.ListNetworkMembershipsByRegistration(device.Name)
	if err != nil {
		return false, err
	}
	plan := sshbroker.PlanCommonNetworkAddresses(providerMemberships, endpointMemberships)
	for _, candidate := range plan.Candidates {
		if candidate.Source == sshbroker.DialAddressPhysical {
			return true, nil
		}
	}
	return false, nil
}

func relayShareActuallyMounted(
	ctx context.Context, st *store.Store, server *models.Server, share *models.SharedStorage,
	hk ...sshbroker.HostKeyStore,
) (bool, error) {
	auth, err := vault.AuthForServer(st, server)
	if err != nil {
		return false, err
	}
	dialSource := dialplan.NewStoreSource(st, "")
	client, _, connectErr := sshbroker.ConnectServerEntry(ctx, sshbroker.ServerDialRequest{
		Server: server, Auth: auth, HostKeys: hostKeyStoreFor(st, hk), KeepAlive: true,
		SuccessCache: dialSource, Topology: dialSource,
	})
	if connectErr != nil {
		return false, connectErr
	}
	defer client.Close()
	checkService := &sharemount.Service{RemoteExec: sharemount.RemoteExecOverClient(client, st, server, sharemount.MaxOutputBytes)}
	checkResult, checkErr := checkService.Check(ctx, sharemount.CheckRequest{
		Server: server, Shares: []*models.SharedStorage{share},
	})
	if checkErr != nil {
		return false, checkErr
	}
	return len(checkResult.States) == 1 && checkResult.States[0].Mounted, nil
}

func relayLocalCopyInspect(
	ctx context.Context, client *sshbroker.Client, sourcePath, targetPath string,
) (int64, error) {
	command := strings.Join([]string{
		"set -eu",
		fmt.Sprintf("if [ -d %s ]; then echo source-directory; exit 2; fi",
			sshbroker.POSIXShellQuote(sourcePath)),
		fmt.Sprintf("if [ ! -f %s ] || [ -L %s ]; then echo source-not-regular; exit 3; fi",
			sshbroker.POSIXShellQuote(sourcePath), sshbroker.POSIXShellQuote(sourcePath)),
		fmt.Sprintf("if [ -d %s ]; then echo target-directory; exit 4; fi",
			sshbroker.POSIXShellQuote(targetPath)),
		fmt.Sprintf("printf 'size='; wc -c < %s", sshbroker.POSIXShellQuote(sourcePath)),
	}, "\n")
	result, err := client.Exec(ctx, command, 30*time.Second, sharemount.MaxOutputBytes)
	if err != nil {
		return 0, fmt.Errorf("检查本地拷贝源文件与目标路径失败：%w", err)
	}
	switch result.ExitCode {
	case 2:
		return 0, fmt.Errorf(
			"源路径 %s 是目录；relay_file 只支持常规文件，请先把目录打包成一个文件", sourcePath)
	case 3:
		return 0, fmt.Errorf("源路径 %s 不是常规文件；relay_file 只支持常规文件", sourcePath)
	case 4:
		return 0, fmt.Errorf(
			"目标路径 %s 是目录；relay_file 只支持常规文件，请先打包目录再传输", targetPath)
	case 0:
		size, parseErr := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(
			strings.TrimSpace(result.Stdout), "size=")), 10, 64)
		if parseErr != nil || size < 0 {
			return 0, fmt.Errorf("本地拷贝源文件大小输出不合法：%q", result.Stdout)
		}
		return size, nil
	default:
		return 0, fmt.Errorf("检查本地拷贝源文件与目标路径失败，退出码 %d", result.ExitCode)
	}
}

func buildRelayLocalCopyCommand(sourcePath, targetPath string) string {
	partialPath := targetPath + relayPartialSuffix
	localCopyNotice := "本地拷贝模式：中断、停止或失败后不能续传，重跑会从文件开头覆盖旧半成品"
	partialDirectoryNotice := fmt.Sprintf(
		"半成品路径 %s 是目录；relay_file 的半成品文件不能覆盖目录，请先处理后再重试", partialPath)
	return strings.Join([]string{
		"set -eu",
		"printf '%s\\n' " + sshbroker.POSIXShellQuote(localCopyNotice),
		fmt.Sprintf("mkdir -p -- %s", sshbroker.POSIXShellQuote(path.Dir(targetPath))),
		fmt.Sprintf("if [ -d %s ] && [ ! -L %s ]; then printf '%%s\\n' %s; exit 5; fi",
			sshbroker.POSIXShellQuote(partialPath), sshbroker.POSIXShellQuote(partialPath),
			sshbroker.POSIXShellQuote(partialDirectoryNotice)),
		fmt.Sprintf("rm -f -- %s", sshbroker.POSIXShellQuote(partialPath)),
		"printf '%s\\n' " + sshbroker.POSIXShellQuote(fmt.Sprintf(
			"cp -- %s %s", sshbroker.POSIXShellQuote(sourcePath), sshbroker.POSIXShellQuote(partialPath))),
		fmt.Sprintf("cp -- %s %s", sshbroker.POSIXShellQuote(sourcePath), sshbroker.POSIXShellQuote(partialPath)),
		"printf '%s\\n' " + sshbroker.POSIXShellQuote(fmt.Sprintf(
			"mv -- %s %s", sshbroker.POSIXShellQuote(partialPath), sshbroker.POSIXShellQuote(targetPath))),
		fmt.Sprintf("mv -- %s %s", sshbroker.POSIXShellQuote(partialPath), sshbroker.POSIXShellQuote(targetPath)),
	}, "\n")
}

// relayLocalCopyExitMessage 把固定拷贝命令的可识别退出码翻译成操作提
// 示；未知退出码按通用失败呈现，细节仍在任务输出里。
func relayLocalCopyExitMessage(code int, targetPath string) string {
	switch code {
	case 5:
		return fmt.Sprintf(
			"半成品路径 %s 是目录；relay_file 的半成品文件不能覆盖目录，请先处理后再重试",
			targetPath+relayPartialSuffix)
	default:
		return fmt.Sprintf("本地拷贝命令失败，退出码 %d；详见任务输出", code)
	}
}

func isWindowsDrivePath(remotePath string) bool {
	return len(remotePath) >= 3 &&
		((remotePath[0] >= 'A' && remotePath[0] <= 'Z') ||
			(remotePath[0] >= 'a' && remotePath[0] <= 'z')) &&
		remotePath[1] == ':' && remotePath[2] == '/'
}

func canonicalWindowsRemotePath(remotePath string) string {
	return strings.ReplaceAll(remotePath, `\`, `/`)
}

func windowsRemotePath(remotePath string) string {
	return strings.ReplaceAll(remotePath, `/`, `\`)
}

// buildRelayWindowsLocalCopyCommand 生成跨 PowerShell 与 cmd 都可执行的
// PowerShell 拷贝脚本。robocopy 只接受“源目录 + 目标目录 + 文件名”，因此
// `<目标>.sshmgr-partial` 用作半成品暂存目录；成功后把其中的文件移动成目标
// 文件，再删除已空的暂存目录。暂存目录只能由 sshmgr 使用：位置被普通文件
// 占用时直接失败，重跑只清理与源文件同名的旧半成品，其他内容一概不动；
// 成功后目录不空就保留并提示，不把任务判为失败。
func buildRelayWindowsLocalCopyCommand(sourcePath, targetPath string) string {
	source := windowsRemotePath(sourcePath)
	target := windowsRemotePath(targetPath)
	sourceDirectory := windowsDriveRootDirectory(windowsRemotePath(path.Dir(sourcePath)))
	sourceName := path.Base(sourcePath)
	partialDirectory := windowsDriveRootDirectory(target + relayPartialSuffix)
	sourceDirectoryNotice := fmt.Sprintf(
		"源路径 %s 是目录；relay_file 只支持常规文件，请先把目录打包成一个文件", source)
	sourceNotRegularNotice := fmt.Sprintf(
		"源路径 %s 不是常规文件；relay_file 只支持常规文件", source)
	targetDirectoryNotice := fmt.Sprintf(
		"目标路径 %s 是目录；relay_file 只支持常规文件，请先打包目录再传输", target)
	script := strings.Join([]string{
		"$ErrorActionPreference = 'Stop'",
		"$__sshmgr_source = " + sshbroker.PowerShellSingleQuote(source),
		"$__sshmgr_target = " + sshbroker.PowerShellSingleQuote(target),
		"$__sshmgr_robocopy_source_directory = " + sshbroker.PowerShellSingleQuote(sourceDirectory),
		"$__sshmgr_source_name = " + sshbroker.PowerShellSingleQuote(sourceName),
		"if (Test-Path -LiteralPath $__sshmgr_source -PathType Container) { [Console]::Out.WriteLine(" +
			sshbroker.PowerShellSingleQuote(sourceDirectoryNotice) + "); exit 2 }",
		"if (-not (Test-Path -LiteralPath $__sshmgr_source -PathType Leaf)) { [Console]::Out.WriteLine(" +
			sshbroker.PowerShellSingleQuote(sourceNotRegularNotice) + "); exit 3 }",
		"$__sshmgr_source_item = Get-Item -LiteralPath $__sshmgr_source -Force",
		"if ($null -eq $__sshmgr_source_item -or -not ($__sshmgr_source_item -is [System.IO.FileInfo]) -or (($__sshmgr_source_item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0)) { [Console]::Out.WriteLine(" +
			sshbroker.PowerShellSingleQuote(sourceNotRegularNotice) + "); exit 3 }",
		"if (Test-Path -LiteralPath $__sshmgr_target -PathType Container) { [Console]::Out.WriteLine(" +
			sshbroker.PowerShellSingleQuote(targetDirectoryNotice) + "); exit 4 }",
		"$__sshmgr_partial_directory = " + sshbroker.PowerShellSingleQuote(partialDirectory),
		"if (Test-Path -LiteralPath $__sshmgr_partial_directory -PathType Leaf) { [Console]::Out.WriteLine(" +
			sshbroker.PowerShellSingleQuote("Windows 本地拷贝的半成品暂存位置 "+partialDirectory+" 不是目录；请先处理这个文件后重试") +
			"); exit 5 }",
		"New-Item -ItemType Directory -Path $__sshmgr_partial_directory -Force | Out-Null",
		"$null = Remove-Item -LiteralPath (Join-Path $__sshmgr_partial_directory $__sshmgr_source_name) -Force -ErrorAction SilentlyContinue",
		"[Console]::Out.WriteLine('本地拷贝模式：中断、停止或失败后不能续传，重跑会从文件开头覆盖旧半成品')",
		"[Console]::Out.WriteLine('robocopy ' + $__sshmgr_robocopy_source_directory + ' ' + $__sshmgr_partial_directory + ' ' + $__sshmgr_source_name)",
		"& robocopy " + strings.Join([]string{
			sshbroker.PowerShellSingleQuote(sourceDirectory),
			sshbroker.PowerShellSingleQuote(partialDirectory),
			sshbroker.PowerShellSingleQuote(sourceName),
			"'/NFL'", "'/NDL'", "'/NJH'", "'/NJS'", "'/NP'", "'/R:1'", "'/W:1'",
		}, " "),
		"$__sshmgr_robocopy_exit = $LASTEXITCODE",
		"if ($__sshmgr_robocopy_exit -ge 8) { exit $__sshmgr_robocopy_exit }",
		"Move-Item -LiteralPath (Join-Path $__sshmgr_partial_directory $__sshmgr_source_name) -Destination $__sshmgr_target -Force",
		"if (@(Get-ChildItem -LiteralPath $__sshmgr_partial_directory -Force).Count -eq 0) { Remove-Item -LiteralPath $__sshmgr_partial_directory -Force } else { [Console]::Out.WriteLine(" +
			sshbroker.PowerShellSingleQuote("Windows 半成品暂存目录 "+partialDirectory+" 不是空的，已保留；sshmgr 只清理与源文件同名的半成品") +
			") }",
		"exit 0",
	}, "\n")
	return sshbroker.PowerShellEncodedCommand(script)
}

// windowsDriveRootDirectory 把 robocopy 的目录参数从裸盘符补成盘符根。
// 「C:」会被 Windows 解释为该盘的当前目录，只有「C:\」才是盘符根本身。
func windowsDriveRootDirectory(directory string) string {
	if len(directory) == 2 && directory[1] == ':' &&
		((directory[0] >= 'A' && directory[0] <= 'Z') ||
			(directory[0] >= 'a' && directory[0] <= 'z')) {
		return directory + `\`
	}
	return directory
}

func runRelayWindowsLocalCopyTask(
	ctx context.Context, tm *TaskManager, task *bgTask, st *store.Store,
	profileID string, server *models.Server, client *sshbroker.Client, command string,
	sourcePath, targetPath string,
) {
	tm.mu.Lock()
	task.client = client
	tm.mu.Unlock()

	driveExecution := prepareNetworkDriveExecution(
		ctx, st, profileID, server, false, client,
		environmentInjectionPlan{shellType: server.ShellType},
	)
	drivePlan := driveExecution.plan
	statusMutex := &sync.Mutex{}
	var stdoutFilter, stderrFilter *networkDriveStatusFilter
	wrapStdout := func(writer io.Writer) io.Writer {
		stdoutFilter = newNetworkDriveStatusFilter(&drivePlan, writer, statusMutex)
		return stdoutFilter
	}
	wrapStderr := func(writer io.Writer) io.Writer {
		stderrFilter = newNetworkDriveStatusFilter(&drivePlan, writer, statusMutex)
		return stderrFilter
	}
	var stdoutWrapper, stderrWrapper func(io.Writer) io.Writer
	if len(drivePlan.shares) > 0 {
		stdoutWrapper, stderrWrapper = wrapStdout, wrapStderr
	}
	exec := func(execContext context.Context, stdout, stderr io.Writer) (int, bool, error) {
		commandStdout, commandStderr := stdout, stderr
		if stdoutWrapper != nil {
			commandStdout = stdoutWrapper(stdout)
		}
		if stderrWrapper != nil {
			commandStderr = stderrWrapper(stderr)
		}
		code, timedOut, err := client.ExecWriters(
			execContext, command, 0, commandStdout, commandStderr, driveExecution.execOption,
		)
		if stdoutFilter != nil {
			_ = stdoutFilter.Flush()
		}
		if stderrFilter != nil {
			_ = stderrFilter.Flush()
		}
		_, _ = drivePlan.consumeStatusLines("", "")
		if err == nil && code != 0 {
			if message := relayWindowsLocalCopyExitMessage(code, sourcePath, targetPath); message != "" {
				err = errors.New(message)
			} else {
				err = fmt.Errorf("Windows 本地拷贝命令失败，退出码 %d；详见任务输出", code)
			}
		}
		return code, timedOut, err
	}
	finish := func() BgExecOutcome {
		return BgExecOutcome{
			InjectionWarnings: networkDriveWarningLines(
				drivePlan.warnings[driveExecution.initialWarningCount:], nil),
			InjectedShareNames: drivePlan.injectedShareNames(),
		}
	}
	tm.mu.Lock()
	task.injectionWarnings = append(task.injectionWarnings, drivePlan.warningLines()...)
	task.finishExec = finish
	tm.mu.Unlock()
	tm.runTask(ctx, task, client, exec, nil)
}

// relayWindowsLocalCopyExitMessage 把固定脚本的可识别退出码翻译成与
// Linux/macOS 本地拷贝一致的操作提示；未知退出码仍按通用失败呈现。
func relayWindowsLocalCopyExitMessage(code int, sourcePath, targetPath string) string {
	switch code {
	case 2:
		return fmt.Sprintf(
			"源路径 %s 是目录；relay_file 只支持常规文件，请先把目录打包成一个文件",
			windowsRemotePath(sourcePath))
	case 3:
		return fmt.Sprintf(
			"源路径 %s 不是常规文件；relay_file 只支持常规文件", windowsRemotePath(sourcePath))
	case 4:
		return fmt.Sprintf(
			"目标路径 %s 是目录；relay_file 只支持常规文件，请先打包目录再传输",
			windowsRemotePath(targetPath))
	case 5:
		return fmt.Sprintf(
			"Windows 本地拷贝的半成品暂存位置 %s 不是目录；请先处理这个文件后重试",
			windowsRemotePath(targetPath)+relayPartialSuffix)
	default:
		return ""
	}
}
