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
	ErrDarwinNotImplemented = errors.New("macOS 共享存储挂载尚未支持，请等待后续版本")
	ErrUnsupportedSystem    = errors.New("目标服务器操作系统不支持共享存储挂载")
	ErrNoSudo               = errors.New("服务器条目没有配置 sudo 凭据，请先由 owner 配置 sudo")
	ErrTargetWithoutDevice  = errors.New("目标服务器未关联拓扑设备，请 owner 先把服务器归到设备上")
	ErrNoCandidate          = errors.New("目标服务器与提供方设备没有可连通的共同网络候选地址")
	ErrUnsafeShareName      = errors.New("共享名不能作为 Linux 挂载点使用")
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

// Selection 是一次候选地址探测的安全结论。Selected 只在存在连通地址时非
// 空；Attempts 保留逐地址探测结论，供成功降级与整体失败呈现使用。
type Selection struct {
	Selected *Candidate
	Attempts []ProbeAttempt
}

// Result 是挂载成功后的安全结论，不包含地址与协议版本。
type Result struct {
	MountPoint      string
	Network         string
	NetworkSequence int
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
}

// Mount 在目标服务器上完成一次首次挂载。重复调用的配置比较、文件锁和
// 版本保护属于后续票；本函数只实现当前票的 Linux 首次挂载路径。
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
		return nil, fmt.Errorf("识别目标服务器操作系统失败：%w", sanitizedExecError(err))
	}
	if request.Server.SudoCredentialID == "" {
		return nil, ErrNoSudo
	}
	switch strings.TrimSpace(systemResult.Stdout) {
	case "Linux":
		return s.mountLinux(ctx, request)
	case "Darwin":
		return nil, ErrDarwinNotImplemented
	default:
		return nil, ErrUnsupportedSystem
	}
}

func (s *Service) mountLinux(ctx context.Context, request Request) (*Result, error) {
	if !safeLinuxShareName(request.Share.Name) {
		return nil, ErrUnsafeShareName
	}
	selection, err := s.selectCandidate(ctx, request.Server, request.Share)
	if err != nil {
		return nil, err
	}
	candidate := selection.Selected
	command := buildLinuxMountCommand(request.Share, candidate.Address)
	// 护栏：挂载密码只作为提权会话标准输入传递。不要把它拼进 command、
	// 错误文本或审计；这里的 append 只补凭据文件所需的行尾。
	standardInput := append([]byte(nil), request.Password...)
	standardInput = append(standardInput, '\n')
	result, execErr := s.exec(ctx, request.Server, RemoteExecRequest{
		Command: command, Privileged: true, StandardInput: standardInput,
	})
	if execErr != nil {
		return nil, fmt.Errorf("Linux 固定挂载命令执行失败：%w", sanitizedExecError(execErr))
	}
	switch result.ExitCode {
	case 0:
		if !strings.Contains(result.Stdout, linuxMountMarker+" ok") {
			return nil, errors.New("Linux 固定挂载命令没有返回成功标记")
		}
		return &Result{
			MountPoint: linuxMountPoint(request.Share.Name), Network: candidate.Network,
			NetworkSequence: candidate.Sequence, Physical: candidate.Physical,
			Attempts: selection.Attempts,
		}, nil
	case linuxExitMissingCIFS:
		return nil, errors.New("目标服务器缺少 mount.cifs，请先由 owner 安装 cifs-utils；挂载工具不会自动安装")
	case linuxExitProtocolFailed:
		return nil, errors.New("SMB 协议版本 3.1.1、3.0、2.1、2.0 全部试挂失败，未写入 fstab 配置")
	default:
		return nil, fmt.Errorf("Linux 固定挂载命令失败（退出码 %d）", result.ExitCode)
	}
}

func (s *Service) selectCandidate(
	ctx context.Context, server *models.Server, share *models.SharedStorage,
) (*Selection, error) {
	if server.DeviceID == "" {
		return nil, ErrTargetWithoutDevice
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
		return nil, fmt.Errorf("候选地址探测失败：%w", sanitizedExecError(execErr))
	}
	results := probe.ParseShellProbeOutput(result.Stdout, len(items))
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
			return &Selection{
				Selected: &Candidate{
					Address: candidate.Address, Network: candidate.NetworkName,
					Sequence: candidate.NetworkSequence, Physical: candidate.Source == sshbroker.DialAddressPhysical,
					RegistrationOrder: candidate.RegistrationOrder,
				},
				Attempts: attempts,
			}, nil
		}
	}
	return nil, &SelectionFailure{Reason: SelectionErrorAllCandidatesFailed, Attempts: attempts}
}

func (s *Service) exec(
	ctx context.Context, server *models.Server, request RemoteExecRequest,
) (RemoteExecResult, error) {
	if s.RemoteExec != nil {
		return s.RemoteExec(ctx, server, request)
	}
	return s.defaultRemoteExec(ctx, server, request)
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

	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		deadline = time.Now().Add(DefaultTimeout)
	}
	maxBytes := s.MaxOutputBytes
	if maxBytes <= 0 {
		maxBytes = MaxOutputBytes
	}
	if !request.Privileged {
		result, err := client.Exec(ctx, request.Command, time.Until(deadline), maxBytes)
		return remoteResultFrom(result), err
	}
	if server.SudoCredentialID == "" {
		return RemoteExecResult{}, ErrNoSudo
	}
	credential, err := s.Store.GetCredential(server.SudoCredentialID)
	if err != nil {
		return RemoteExecResult{}, fmt.Errorf("读取 sudo 凭据失败：%w", err)
	}
	if credential == nil {
		return RemoteExecResult{}, fmt.Errorf("服务器条目 %s 的 sudo 凭据不存在", server.Name)
	}
	result, err := client.ExecSudo(
		ctx, request.Command, credential.Secret, time.Until(deadline), maxBytes,
		sshbroker.WithSudoStandardInput(request.StandardInput),
	)
	if err == nil && result.Sudo != nil && result.Sudo.Outcome != sshbroker.SudoElevated {
		return remoteResultFrom(result), fmt.Errorf(
			"sudo 提权未完成（%s）：%s", result.Sudo.Outcome, result.Sudo.Diagnostic)
	}
	return remoteResultFrom(result), err
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
