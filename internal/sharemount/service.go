// Package sharemount 提供共享存储挂载的领域服务。它持有「在目标服务器上
// 执行固定命令」的注入点；Linux 与 macOS 的系统差异放在本包的不同文件
// 里，MCP 工具层只负责授权、取登记与渲染。
package sharemount

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"ssh-manager-mcp/internal/dialplan"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/probe"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
)

const (
	// DefaultTimeout 覆盖识别系统、探测地址、试挂与生效的整次调用。
	DefaultTimeout = 5 * time.Minute
	// MaxOutputBytes 限制固定命令输出，避免目标机异常输出占满内存。
	MaxOutputBytes = int64(1024 * 1024)
	// SMBPort 是共享存储提供方地址的固定探测端口。
	SMBPort = 445
)

var (
	ErrUnsupportedSystem   = errors.New("目标服务器操作系统不支持共享存储挂载")
	ErrNoSudo              = errors.New("服务器条目没有配置 sudo 凭据，请先由 owner 配置 sudo")
	ErrTargetWithoutDevice = errors.New("目标服务器未关联拓扑设备，请 owner 先把服务器归到设备上")
	ErrNoCandidate         = errors.New("目标服务器与提供方设备没有可连通的共同网络候选地址")
	ErrUnsafeShareName     = errors.New("共享名不能作为 Linux 挂载点使用")
	ErrUnsafeDarwinName    = errors.New("共享名不能作为 macOS 挂载点使用")
)

// RemoteExecRequest 是一次固定命令执行请求。StandardInput 只会写在 sudo
// 密码行之后，机密值绝不进入 Command，也绝不能进入工具结果、日志或审计。
type RemoteExecRequest struct {
	Command       string
	Privileged    bool
	StandardInput []byte
}

// RemoteExecResult 是固定命令执行的原始结论。
type RemoteExecResult struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	TimedOut  bool
	Truncated bool
}

// RemoteExecFunc 是在目标服务器上执行固定命令的可替换注入点。
type RemoteExecFunc func(
	ctx context.Context, server *models.Server, request RemoteExecRequest,
) (RemoteExecResult, error)

// Request 是一次挂载调用的进程内输入。Password 从机密项当前值取出后只在
// 进程内传递；它不能被序列化，也不能写入错误文本或审计。
type Request struct {
	Share    *models.SharedStorage
	Server   *models.Server
	Password []byte
}

// Candidate 是一个已按共同网络规则排好序的候选地址。Address 只在进程内
// 使用，输出与审计只带网络名和登记序号。
type Candidate struct {
	Address           string
	Network           string
	Sequence          int
	Physical          bool
	RegistrationOrder int64
}

// Selection 是一次候选地址探测的安全结论。Reachable 保持共同网络规划里
// 的连通顺序；Attempts 保留逐地址探测结论，供成功降级与整体失败呈现使
// 用。
type Selection struct {
	// Reachable 保持共同网络规划里的连通顺序。重复挂载要把全部连通地址
	// 交给远端固定命令，才能在锁内优先保留现有地址。
	Reachable []*Candidate
	Attempts  []ProbeAttempt
}

// Result 是挂载成功后的安全结论，不包含地址与协议版本。
type Result struct {
	MountPoint      string
	Network         string
	NetworkSequence int
	Action          string
	Notices         []string
	Physical        bool
	Attempts        []ProbeAttempt
}

// Service 是共享存储挂载服务。生产路径复用统一拨号入口与软件清单的 sudo
// 包装；固定命令不经过命令策略判定。
type Service struct {
	Store    *store.Store
	Instance string
	// HostKeys 是远程执行使用的主机密钥存储；空值使用存储本身。
	HostKeys sshbroker.HostKeyStore
	// RemoteExec 为空时使用生产实现。
	RemoteExec RemoteExecFunc
	// Timeout 与 MaxOutputBytes 主要供测试覆盖。
	Timeout        time.Duration
	MaxOutputBytes int64
	// CheckTimeout 是单台服务器现场检测的固定超时，主要供测试覆盖。
	CheckTimeout time.Duration
}

// Mount 在目标服务器上完成首次挂载、重复调用比较或按新快照更新。系统
// 特定的固定命令由不同实现文件生成。
func (s *Service) Mount(parent context.Context, request Request) (*Result, error) {
	if s == nil || s.Store == nil {
		return nil, fmt.Errorf("共享存储挂载服务没有绑定存储")
	}
	if request.Share == nil || request.Server == nil || len(request.Password) == 0 {
		return nil, fmt.Errorf("共享存储挂载参数不完整")
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	systemResult, err := s.exec(ctx, request.Server, RemoteExecRequest{Command: "uname -s"})
	if err != nil {
		return nil, mountFailure(MountClassSSHError, fmt.Errorf(
			"识别目标服务器操作系统失败：%w", sanitizedExecError(err)))
	}
	if request.Server.SudoCredentialID == "" {
		return nil, mountFailure(MountClassMissingSudo, ErrNoSudo)
	}
	switch strings.TrimSpace(systemResult.Stdout) {
	case "Linux":
		return s.mountLinux(ctx, request)
	case "Darwin":
		return s.mountDarwin(ctx, request)
	default:
		return nil, mountFailure(MountClassUnsupportedSystem, ErrUnsupportedSystem)
	}
}

func (s *Service) mountDarwin(ctx context.Context, request Request) (*Result, error) {
	return s.mountWithSystem(ctx, request, darwinMountFlow)
}

func (s *Service) mountLinux(ctx context.Context, request Request) (*Result, error) {
	return s.mountWithSystem(ctx, request, linuxMountFlow)
}

// systemMountFlow 汇总 Linux 与 macOS 挂载固定命令的系统差异：命令生
// 成、输出解析与退出码说明。其余流程（校验共享名、选地址、传密码、解
// 释输出）两个系统完全共用。
type systemMountFlow struct {
	systemName      string
	unsafeNameError error
	buildCommand    func(share *models.SharedStorage, candidates []*Candidate) string
	parseOutput     func(stdout string) (action string, selectedIndex int, reset bool, err error)
	mountPoint      func(shareName string) string
	exitCodeError   func(exitCode int) error
}

var darwinMountFlow = systemMountFlow{
	systemName:      "macOS",
	unsafeNameError: ErrUnsafeDarwinName,
	buildCommand:    buildDarwinMountCommand,
	parseOutput: func(stdout string) (string, int, bool, error) {
		return parseMountCommandOutput(stdout, darwinMountMarker, "macOS")
	},
	mountPoint:    darwinMountPoint,
	exitCodeError: darwinMountExitCodeError,
}

var linuxMountFlow = systemMountFlow{
	systemName:      "Linux",
	unsafeNameError: ErrUnsafeShareName,
	buildCommand:    buildLinuxMountCommand,
	parseOutput: func(stdout string) (string, int, bool, error) {
		return parseMountCommandOutput(stdout, linuxMountMarker, "Linux")
	},
	mountPoint:    linuxMountPoint,
	exitCodeError: linuxMountExitCodeError,
}

// mountWithSystem 是两个系统共用的挂载流程。挂载密码只作为提权会话标准
// 输入传递，绝不拼进命令文本；这里对 standardInput 的补写只补凭据文件
// 所需的行尾。
func (s *Service) mountWithSystem(
	ctx context.Context, request Request, flow systemMountFlow,
) (*Result, error) {
	if !safeMountShareName(request.Share.Name) {
		return nil, mountFailure(MountClassError, flow.unsafeNameError)
	}
	selection, err := s.selectCandidate(ctx, request.Server, request.Share)
	if err != nil {
		return nil, err
	}
	candidates := selection.Reachable
	command := flow.buildCommand(request.Share, candidates)
	standardInput := append([]byte(nil), request.Password...)
	standardInput = append(standardInput, '\n')
	result, execErr := s.exec(ctx, request.Server, RemoteExecRequest{
		Command: command, Privileged: true, StandardInput: standardInput,
	})
	if execErr != nil {
		return nil, mountFailure(MountClassSSHError, fmt.Errorf(
			"%s 固定挂载命令执行失败：%w", flow.systemName, sanitizedExecError(execErr)))
	}
	if result.ExitCode != 0 {
		if exitErr := flow.exitCodeError(result.ExitCode); exitErr != nil {
			return nil, exitErr
		}
		return nil, fmt.Errorf("%s 固定挂载命令失败（退出码 %d）", flow.systemName, result.ExitCode)
	}
	action, selectedIndex, reset, parseErr := flow.parseOutput(result.Stdout)
	if parseErr != nil {
		return nil, parseErr
	}
	if selectedIndex > len(candidates) {
		return nil, fmt.Errorf("%s 固定挂载命令返回了不存在的候选序号", flow.systemName)
	}
	switch action {
	case "first-mount-success", "update-success", "remount-success", "no-change":
	default:
		return nil, mountOutputUnknownAction(flow.systemName)
	}
	selected := candidates[selectedIndex-1]
	notices := []string{}
	if reset {
		notices = append(notices, "系统配置被重置，已重建")
	}
	for index := 0; index < selectedIndex-1; index++ {
		if candidates[index].Physical {
			notices = append(notices, "存在更靠前的物理局域网地址；挂载工具不会卸载现有挂载，如需切换请由 owner 手动切换")
			break
		}
	}
	return &Result{
		MountPoint: flow.mountPoint(request.Share.Name), Network: selected.Network,
		NetworkSequence: selected.Sequence, Action: action, Notices: notices,
		Physical: selected.Physical, Attempts: selection.Attempts,
	}, nil
}

func (s *Service) selectCandidate(
	ctx context.Context, server *models.Server, share *models.SharedStorage,
) (*Selection, error) {
	if server.DeviceID == "" {
		return nil, mountFailure(MountClassNoDevice, ErrTargetWithoutDevice)
	}
	targetDevice, err := s.Store.GetDeviceByID(server.DeviceID)
	if err != nil {
		return nil, err
	}
	if targetDevice == nil {
		return nil, &SelectionFailure{Reason: SelectionErrorNoCommonNetwork, Attempts: []ProbeAttempt{}}
	}
	targetMemberships, err := s.Store.ListNetworkMembershipsByRegistration(targetDevice.Name)
	if err != nil {
		return nil, err
	}
	providerMemberships, err := s.Store.ListNetworkMembershipsByRegistration(share.DeviceName)
	if err != nil {
		return nil, err
	}
	plan := sshbroker.PlanCommonNetworkAddresses(providerMemberships, targetMemberships)
	if len(plan.Candidates) == 0 {
		return nil, &SelectionFailure{
			Reason:           SelectionErrorNoCommonNetwork,
			ProviderNetworks: networkNames(providerMemberships),
			TargetNetworks:   networkNames(targetMemberships),
			Attempts:         []ProbeAttempt{},
		}
	}
	addressTimeout, err := sshbroker.ResolveAddressTimeout()
	if err != nil {
		return nil, err
	}
	items := make([]probe.ShellCommandItem, 0, len(plan.Candidates))
	for _, address := range plan.Candidates {
		items = append(items, probe.ShellCommandItem{
			Address: address.Address, Port: SMBPort, Timeout: addressTimeout,
		})
	}
	command, err := probe.BuildShellProbeCommand(models.ShellPOSIX, items)
	if err != nil {
		return nil, err
	}
	result, execErr := s.exec(ctx, server, RemoteExecRequest{Command: command})
	if execErr != nil {
		return nil, mountFailure(MountClassSSHError, fmt.Errorf(
			"候选地址探测失败：%w", sanitizedExecError(execErr)))
	}
	results := probe.ParseShellProbeOutput(result.Stdout, len(items))
	reachable := make([]*Candidate, 0, len(results))
	attempts := make([]ProbeAttempt, 0, len(results))
	for index, probeResult := range results {
		attempts = append(attempts, ProbeAttempt{
			NetworkName:     plan.Candidates[index].NetworkName,
			NetworkSequence: plan.Candidates[index].NetworkSequence,
			Physical:        plan.Candidates[index].Source == sshbroker.DialAddressPhysical,
			OK:              probeResult.OK,
			ErrorCategory:   probeResult.Category,
			DurationMilli:   probeResult.Duration.Milliseconds(),
		})
		if probeResult.OK {
			candidate := plan.Candidates[index]
			reachable = append(reachable, &Candidate{
				Address: candidate.Address, Network: candidate.NetworkName,
				Sequence: candidate.NetworkSequence, Physical: candidate.Source == sshbroker.DialAddressPhysical,
				RegistrationOrder: candidate.RegistrationOrder,
			})
		}
	}
	if len(reachable) == 0 {
		return nil, &SelectionFailure{Reason: SelectionErrorAllCandidatesFailed, Attempts: attempts}
	}
	return &Selection{Reachable: reachable, Attempts: attempts}, nil
}

func (s *Service) exec(
	ctx context.Context, server *models.Server, request RemoteExecRequest,
) (RemoteExecResult, error) {
	if s.RemoteExec != nil {
		return s.RemoteExec(ctx, server, request)
	}
	return s.defaultRemoteExec(ctx, server, request)
}

// Exec 在共享存储挂载领域服务之外执行同一个固定命令注入点。共享存储
// owner 管理模块复用这里的统一拨号与提权路径，不另建远程执行通道。
func (s *Service) Exec(
	ctx context.Context, server *models.Server, request RemoteExecRequest,
) (RemoteExecResult, error) {
	return s.exec(ctx, server, request)
}

// defaultRemoteExec 按服务器条目解析凭据、经统一入口拨号并执行固定命令。
// 提权路径与软件清单共用 ExecSudo 的标记行证实；标准输入只在 sudo 密码行
// 后追加。
func (s *Service) defaultRemoteExec(
	ctx context.Context, server *models.Server, request RemoteExecRequest,
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

	maxBytes := s.MaxOutputBytes
	if maxBytes <= 0 {
		maxBytes = MaxOutputBytes
	}
	return RemoteExecOverClient(client, s.Store, server, maxBytes)(ctx, server, request)
}

// RemoteExecOverClient 返回在一条已经建立的 SSH 连接上执行共享存储固定
// 命令的注入点；提权请求经 sudo 标准输入送入机密值。共享地址本地拷贝与
// 挂载工具共用这一条路径，不另建远程执行通道。
func RemoteExecOverClient(
	client *sshbroker.Client, st *store.Store, server *models.Server, maxOutputBytes int64,
) RemoteExecFunc {
	return func(ctx context.Context, _ *models.Server, request RemoteExecRequest) (RemoteExecResult, error) {
		if maxOutputBytes <= 0 {
			maxOutputBytes = MaxOutputBytes
		}
		deadline, hasDeadline := ctx.Deadline()
		if !hasDeadline {
			deadline = time.Now().Add(DefaultTimeout)
		}
		if !request.Privileged {
			result, err := client.Exec(ctx, request.Command, time.Until(deadline), maxOutputBytes)
			return remoteResultFrom(result), err
		}
		if server.SudoCredentialID == "" {
			return RemoteExecResult{}, ErrNoSudo
		}
		credential, err := st.GetCredential(server.SudoCredentialID)
		if err != nil {
			return RemoteExecResult{}, fmt.Errorf("读取 sudo 凭据失败：%w", err)
		}
		if credential == nil {
			return RemoteExecResult{}, fmt.Errorf("服务器条目 %s 的 sudo 凭据不存在", server.Name)
		}
		result, err := client.ExecSudo(
			ctx, request.Command, credential.Secret, time.Until(deadline), maxOutputBytes,
			sshbroker.WithSudoStandardInput(request.StandardInput),
		)
		if err == nil && result.Sudo != nil && result.Sudo.Outcome != sshbroker.SudoElevated {
			return remoteResultFrom(result), &RemoteSudoFailure{Diagnostic: fmt.Sprintf(
				"sudo 提权未完成（%s）：%s", result.Sudo.Outcome, result.Sudo.Diagnostic)}
		}
		return remoteResultFrom(result), err
	}
}

func remoteResultFrom(result sshbroker.ExecResult) RemoteExecResult {
	return RemoteExecResult{
		Stdout: result.Stdout, Stderr: result.Stderr, ExitCode: result.ExitCode,
		TimedOut: result.TimedOut, Truncated: result.Truncated,
	}
}

func sanitizedExecError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("执行超时")
	}
	if errors.Is(err, context.Canceled) {
		return errors.New("执行被取消")
	}
	return errors.New("SSH 会话执行失败")
}
