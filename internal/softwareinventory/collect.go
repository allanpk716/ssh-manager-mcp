package softwareinventory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"ssh-manager-mcp/internal/dialplan"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
)

// 固定限制来自规格：单次采集建议 5 分钟超时、16 MiB 输出上限。
const (
	DefaultCollectTimeout = 5 * time.Minute
	MaxCollectOutputBytes = int64(16 << 20)
)

// RemoteExecResult 是一次固定采集命令的执行结果。TimedOut 与 Truncated 都
// 会带着已收到的部分输出返回，由领域模块继续解析并标为不完整。
type RemoteExecResult struct {
	Stdout    string
	Stderr    string
	TimedOut  bool
	Truncated bool
}

// RemoteExecFunc 是在受管服务器上执行固定采集命令的可替换注入点。sudo 为
// true 时按服务器条目的 sudo 凭据走现有 sudo 包装；命令本身仍是固定模
// 板，包装只发生在这一层。
type RemoteExecFunc func(
	ctx context.Context,
	server *models.Server,
	shell models.ShellType,
	command string,
	sudo bool,
) (RemoteExecResult, error)

// Service 是软件清单采集服务。测试注入 RemoteExec 与 Now；生产路径复用
// 按服务器条目解析凭据、统一拨号、执行的链路（与连通探测相同）。
type Service struct {
	Store    *store.Store
	Instance string
	// HostKeys 是远程执行使用的主机密钥存储；空值使用存储本身。
	HostKeys sshbroker.HostKeyStore
	// RemoteExec 为空时使用生产实现。
	RemoteExec RemoteExecFunc
	// Now 提供采集时间；空值使用真实时钟。
	Now func() time.Time
	// Timeout 与 MaxOutputBytes 覆盖规格默认值，主要供测试使用。
	Timeout        time.Duration
	MaxOutputBytes int64
}

// CollectRequest 是一次立即采集的输入。ServerEntry 可用服务器条目名或条
// 目标识符指定；为空时按默认规则选。
type CollectRequest struct {
	DeviceName  string
	ServerEntry string
	// Source 是这次采集落库的审计来源；零值沿用命令行来源。
	Source store.WriteSource
}

// CollectionReport 是一次采集的对外结论。
type CollectionReport struct {
	DeviceName        string
	ServerEntryName   string
	Sudo              bool
	CommandSetVersion string
	SystemType        string
	Incomplete        bool
	Notes             []string
	ItemCounts        map[models.SoftwareCategory]int
	StartedAt         time.Time
	EndedAt           time.Time
}

// Collect 对一台设备立即采集一次：选择服务器条目、生成固定命令、执行、
// 解析并在一个存储事务里落库。连接失败保留上一次结果并记录原因；输出
// 截断保留已解析部分并标为不完整。
func (s *Service) Collect(parent context.Context, request CollectRequest) (*CollectionReport, error) {
	if s == nil || s.Store == nil {
		return nil, fmt.Errorf("软件清单采集服务没有绑定存储")
	}
	deviceName := strings.TrimSpace(request.DeviceName)
	if deviceName == "" {
		return nil, fmt.Errorf("设备名不能为空")
	}
	startedAt := s.now()
	if _, err := s.Store.GetDevice(deviceName); err != nil {
		return nil, err
	}
	servers, err := s.Store.FullServersForDevice(deviceName)
	if err != nil {
		return nil, err
	}
	if len(servers) == 0 {
		return nil, fmt.Errorf("设备 %s 没有服务器条目，无法采集软件清单", deviceName)
	}
	sudoMap, err := s.Store.SoftwareCollectSudoEnabled()
	if err != nil {
		return nil, err
	}
	chosen, err := s.chooseServer(servers, sudoMap, request.ServerEntry)
	if err != nil {
		return nil, err
	}
	sudo := sudoMap[chosen.ID]
	if sudo && chosen.SudoCredentialID == "" {
		failure := fmt.Errorf("服务器条目 %s 打开了 sudo 采集开关但没有配置 sudo 凭据", chosen.Name)
		s.recordFailure(deviceName, failure.Error(), chosen.ID, sudo)
		return nil, failure
	}
	shell := models.NormalizeShellType(string(chosen.ShellType))
	command, err := BuildInventoryCommand(shell)
	if err != nil {
		s.recordFailure(deviceName, err.Error(), chosen.ID, sudo)
		return nil, err
	}

	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultCollectTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	execResult, execErr := s.remoteExec()(ctx, chosen, shell, command, sudo)
	var collection *ParsedCollection
	var parseErr error
	switch shell {
	case models.ShellPowerShell, models.ShellWindowsCmd:
		collection, parseErr = parseWindowsInventoryOutput(execResult.Stdout)
	default:
		collection, parseErr = parsePOSIXInventoryOutput(execResult.Stdout)
	}
	if parseErr != nil {
		reason := fmt.Sprintf("固定命令集输出无法解析：%v", parseErr)
		if execErr != nil {
			reason = fmt.Sprintf("固定命令执行失败：%v", execErr)
		}
		s.recordFailure(deviceName, reason, chosen.ID, sudo)
		return nil, fmt.Errorf("%s", reason)
	}
	if execResult.TimedOut {
		collection.Incomplete = true
		collection.Notes = append(collection.Notes, "固定命令执行超时")
	}
	if execErr != nil {
		collection.Incomplete = true
		collection.Notes = append(collection.Notes, fmt.Sprintf("固定命令执行中断：%v", execErr))
	}
	if execResult.Truncated {
		collection.Incomplete = true
		collection.Notes = append(collection.Notes, "输出超过单次采集上限")
	}
	for i := range collection.Items {
		collection.Items[i].CollectedAt = s.now()
		sortObservedPortsByName(collection.Items[i].ListeningPorts)
	}
	incomplete := collection.Incomplete
	if err := s.Store.ReplaceSoftwareObservedItems(deviceName, store.SoftwareObservedWrite{
		Items: collection.Items, ServerID: chosen.ID, Sudo: sudo,
		CommandSetVersion: collection.CommandSetVersion, SystemType: collection.SystemType,
		Incomplete: incomplete,
	}, collectWriteSource(request.Source)); err != nil {
		return nil, err
	}
	return &CollectionReport{
		DeviceName: deviceName, ServerEntryName: chosen.Name, Sudo: sudo,
		CommandSetVersion: collection.CommandSetVersion, SystemType: collection.SystemType,
		Incomplete: incomplete, Notes: collection.Notes,
		ItemCounts: itemCounts(collection.Items),
		StartedAt:  startedAt, EndedAt: s.now(),
	}, nil
}

// collectWriteSource 返回采集落库使用的审计来源；未指定时按命令行立即采
// 集处理。
func collectWriteSource(source store.WriteSource) store.WriteSource {
	if source.Kind() == "" {
		return store.CLISource()
	}
	return source
}

// chooseServer 选择采集用的服务器条目：显式指定时必须在设备上；未指定时
// 优先用打开 sudo 采集开关的条目，否则名字排序第一个。
func (s *Service) chooseServer(
	servers []*models.Server,
	sudoMap map[string]bool,
	requested string,
) (*models.Server, error) {
	sorted := make([]*models.Server, len(servers))
	copy(sorted, servers)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	if strings.TrimSpace(requested) == "" {
		for _, server := range sorted {
			if sudoMap[server.ID] {
				return server, nil
			}
		}
		return sorted[0], nil
	}
	request := strings.TrimSpace(requested)
	for _, server := range sorted {
		if server.Name == request || server.ID == request {
			return server, nil
		}
	}
	return nil, fmt.Errorf("服务器条目 %q 不在这台设备上", request)
}

func (s *Service) recordFailure(deviceName, reason, serverID string, sudo bool) {
	_ = s.Store.RecordSoftwareCollectionFailure(deviceName, reason, serverID, sudo, store.CLISource())
}

func (s *Service) remoteExec() RemoteExecFunc {
	if s.RemoteExec != nil {
		return s.RemoteExec
	}
	return s.defaultRemoteExec
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func itemCounts(items []models.SoftwareObservedItem) map[models.SoftwareCategory]int {
	counts := map[models.SoftwareCategory]int{}
	for _, item := range items {
		counts[item.Category]++
	}
	return counts
}

// defaultRemoteExec 是生产环境的远程执行实现：按服务器条目解析凭据、经
// 统一入口拨号、执行固定命令。固定采集命令在会话内直接执行，不经过命令
// 策略判定；sudo 路径复用现有 sudo 包装并要求提权结果被标记行证实。
func (s *Service) defaultRemoteExec(
	ctx context.Context,
	server *models.Server,
	_ models.ShellType,
	command string,
	sudo bool,
) (RemoteExecResult, error) {
	auth, err := vault.AuthForServer(s.Store, server)
	if err != nil {
		return RemoteExecResult{}, fmt.Errorf("解析服务器条目凭据失败：%w", err)
	}
	dialSource := dialplan.NewStoreSource(s.Store, s.Instance)
	hostKeys := sshbroker.HostKeyStore(s.Store)
	if s.HostKeys != nil {
		hostKeys = s.HostKeys
	}
	client, _, err := sshbroker.ConnectServerEntry(ctx, sshbroker.ServerDialRequest{
		Server: server, Auth: auth, HostKeys: hostKeys,
		SuccessCache: dialSource, Topology: dialSource,
	})
	if err != nil {
		return RemoteExecResult{}, fmt.Errorf("连接服务器失败：%w", err)
	}
	defer client.Close()
	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		deadline = time.Now().Add(DefaultCollectTimeout)
	}
	maxBytes := s.MaxOutputBytes
	if maxBytes <= 0 {
		maxBytes = MaxCollectOutputBytes
	}
	if !sudo {
		result, err := client.Exec(ctx, command, time.Until(deadline), maxBytes)
		return remoteExecResultFrom(result), err
	}
	if server.SudoCredentialID == "" {
		return RemoteExecResult{}, fmt.Errorf("服务器条目 %s 没有配置 sudo 凭据", server.Name)
	}
	credential, err := s.Store.GetCredential(server.SudoCredentialID)
	if err != nil {
		return RemoteExecResult{}, fmt.Errorf("读取 sudo 凭据失败：%w", err)
	}
	if credential == nil {
		return RemoteExecResult{}, fmt.Errorf("服务器条目 %s 的 sudo 凭据不存在", server.Name)
	}
	result, err := client.ExecSudo(ctx, command, credential.Secret, time.Until(deadline), maxBytes)
	if err == nil && result.Sudo != nil && result.Sudo.Outcome != sshbroker.SudoElevated {
		return remoteExecResultFrom(result), fmt.Errorf(
			"sudo 提权未完成（%s）：%s", result.Sudo.Outcome, result.Sudo.Diagnostic)
	}
	return remoteExecResultFrom(result), err
}

func remoteExecResultFrom(result sshbroker.ExecResult) RemoteExecResult {
	return RemoteExecResult{
		Stdout: result.Stdout, Stderr: result.Stderr,
		TimedOut: result.TimedOut, Truncated: result.Truncated,
	}
}
