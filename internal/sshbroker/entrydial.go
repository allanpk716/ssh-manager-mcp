package sshbroker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// DialAddressSource 标明一个候选地址来自哪一级挑选优先级：物理局域
// 网、虚拟局域网或兜底主机。
type DialAddressSource string

const (
	// DialAddressPhysical 表示目标设备与拨号设备共同所在的物理局域网。
	DialAddressPhysical DialAddressSource = "physical"
	// DialAddressVirtual 表示目标设备与拨号设备共同所在的虚拟局域网。
	DialAddressVirtual DialAddressSource = "virtual"
	// DialAddressFallback 表示服务器条目登记的兜底主机。
	DialAddressFallback DialAddressSource = "fallback"
)

// FallbackOnlyReason 解释为什么拓扑规划后仍只剩兜底主机。
type FallbackOnlyReason string

const (
	// FallbackOnlyDialerUnknown 表示本机设备尚未确定或已从拓扑消失。
	FallbackOnlyDialerUnknown FallbackOnlyReason = "dialer-unknown"
	// FallbackOnlyNoCommonNetwork 表示两台设备没有任何共同网络。
	FallbackOnlyNoCommonNetwork FallbackOnlyReason = "no-common-network"
	// FallbackOnlyLocalDeviceDeleted 表示本机设备设定指向的设备已不存在。
	FallbackOnlyLocalDeviceDeleted FallbackOnlyReason = "local-device-deleted"
)

// DialAddress 是一个去重后的候选 SSH 地址。NetworkSequence 是同一网络内
// 按登记序号排列后的序号，供错误文本使用；兜底地址没有这个序号。
type DialAddress struct {
	Address           string
	NetworkName       string
	NetworkSequence   int
	Source            DialAddressSource
	RegistrationOrder int64
}

// DialAddressPlan 是候选地址规划结果。FallbackOnlyReason 非空时表示没有
// 可用的网络拓扑候选，只能尝试兜底主机。
type DialAddressPlan struct {
	Candidates []DialAddress
	// HasTargetTopology 表示目标设备至少登记了一个网络成员。
	HasTargetTopology  bool
	FallbackOnlyReason FallbackOnlyReason
}

// DialAttemptError 记录一个候选地址的失败结果，供逐地址错误汇总使用。
type DialAttemptError struct {
	Candidate DialAddress
	Err       error
}

// ServerDialError 汇总一次按服务器条目拨号的逐地址失败结果；Unwrap 回到第
// 一个底层错误，调用方既有的错误分类继续可用。
type ServerDialError struct {
	Failures []DialAttemptError
	Hint     string
	Port     int
}

func (e *ServerDialError) Error() string {
	lines := make([]string, 0, len(e.Failures)+2)
	lines = append(lines, "全部候选地址连接失败：")
	for _, failure := range e.Failures {
		lines = append(lines, candidateLabel(failure.Candidate)+"："+candidateFailureText(failure.Err, failure.Candidate.Address, e.Port))
	}
	if e.Hint != "" {
		lines = append(lines, e.Hint)
	}
	return strings.Join(lines, "\n")
}

func (e *ServerDialError) Unwrap() error {
	if len(e.Failures) == 0 {
		return nil
	}
	return e.Failures[0].Err
}

func candidateLabel(candidate DialAddress) string {
	if candidate.Source == DialAddressFallback {
		return "兜底地址"
	}
	return fmt.Sprintf("%s 第%d个地址", candidate.NetworkName, candidate.NetworkSequence)
}

func candidateFailureText(err error, host string, port int) string {
	switch {
	case errors.Is(err, ErrHostKeyMismatch):
		return "主机密钥不同，已跳过"
	case errors.Is(err, context.DeadlineExceeded):
		return "连接超时"
	case errors.Is(err, context.Canceled):
		return "连接被取消"
	}
	var redacted *addrRedactedError
	if errors.As(err, &redacted) {
		return err.Error()
	}
	return redactAddr(err, host, port).Error()
}

// normalizeDialAddress 统一 IP 与域名的等价写法，供去重使用。IPv6 归一为
// net.IP 的标准文本；域名按大小写不敏感处理。
func normalizeDialAddress(address string) string {
	return store.NormalizeSSHHost(address)
}

// PlanCommonNetworkAddresses 是共享的候选地址规划纯函数：只取拨号设备也
// 是成员的网络里的目标地址，物理局域网优先于虚拟局域网，同级按网络成
// 员登记序号；不追加任何兜底主机。按服务器条目拨号与连通探测共用它，
// 保证两条路径的共同网络判定与排序不会漂移。
func PlanCommonNetworkAddresses(targetMemberships, dialerMemberships []*models.NetworkMembership) DialAddressPlan {
	dialerNetworks := make(map[string]bool, len(dialerMemberships))
	for _, membership := range dialerMemberships {
		if membership != nil {
			dialerNetworks[membership.NetworkID] = true
		}
	}

	type preparedAddress struct {
		item DialAddress
		key  string
		rank int
	}
	prepared := make([]preparedAddress, 0, len(targetMemberships)+1)
	sequenceByNetwork := networkSequencesByRegistration(targetMemberships)
	hasTopology := false
	hasCommonNetwork := false
	for _, membership := range targetMemberships {
		if membership == nil {
			continue
		}
		hasTopology = true
		if !dialerNetworks[membership.NetworkID] {
			continue
		}
		source := DialAddressVirtual
		switch membership.NetworkType {
		case models.NetworkPhysical:
			source = DialAddressPhysical
		case models.NetworkVirtual:
			source = DialAddressVirtual
		default:
			continue
		}
		hasCommonNetwork = true
		prepared = append(prepared, preparedAddress{
			item: DialAddress{
				Address: membership.Address, NetworkName: membership.NetworkName,
				NetworkSequence: sequenceByNetwork[membership.NetworkID],
				Source:          source, RegistrationOrder: membership.RegistrationOrder,
			},
			key: normalizeDialAddress(membership.Address), rank: dialAddressPriority(source),
		})
	}
	sort.SliceStable(prepared, func(i, j int) bool {
		if prepared[i].rank != prepared[j].rank {
			return prepared[i].rank < prepared[j].rank
		}
		if prepared[i].item.RegistrationOrder != prepared[j].item.RegistrationOrder {
			return prepared[i].item.RegistrationOrder < prepared[j].item.RegistrationOrder
		}
		if prepared[i].item.NetworkName != prepared[j].item.NetworkName {
			return prepared[i].item.NetworkName < prepared[j].item.NetworkName
		}
		return prepared[i].item.Address < prepared[j].item.Address
	})

	out := make([]DialAddress, 0, len(prepared))
	seen := make(map[string]bool, len(prepared))
	for _, item := range prepared {
		if seen[item.key] {
			continue
		}
		seen[item.key] = true
		out = append(out, item.item)
	}
	var reason FallbackOnlyReason
	if hasTopology && !hasCommonNetwork {
		reason = FallbackOnlyDialerUnknown
		for _, membership := range dialerMemberships {
			if membership != nil {
				reason = FallbackOnlyNoCommonNetwork
				break
			}
		}
	}
	return DialAddressPlan{Candidates: out, HasTargetTopology: hasTopology, FallbackOnlyReason: reason}
}

// PlanServerDialAddresses 是按服务器条目拨号使用的候选地址规划纯函数：
// 先取共享规划给出的共同网络地址，再追加服务器条目的兜底主机；物理局
// 域网优先于虚拟局域网，兜底主机最后，同级按网络成员登记序号。
func PlanServerDialAddresses(targetMemberships, dialerMemberships []*models.NetworkMembership, fallbackHost string) DialAddressPlan {
	plan := PlanCommonNetworkAddresses(targetMemberships, dialerMemberships)
	prepared := make([]DialAddress, 0, len(plan.Candidates)+1)
	prepared = append(prepared, plan.Candidates...)
	prepared = append(prepared, DialAddress{
		Address: fallbackHost, Source: DialAddressFallback,
	})
	// 兜底主机排在共同网络地址之后；与拓扑地址重复时只保留优先级更高
	// 的那份，与共享规划使用同一条等价地址去重规则。
	out := make([]DialAddress, 0, len(prepared))
	seen := make(map[string]bool, len(prepared))
	for _, candidate := range prepared {
		key := normalizeDialAddress(candidate.Address)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, candidate)
	}
	plan.Candidates = out
	return plan
}

// OrderServerDialPlanWithSuccess 只按成功地址缓存重排已有候选计划的同级地
// 址。按服务器条目拨号的共用入口（ConnectServerEntry）在首次锚定过
// 滤等调整之后调用它，保证只读预览与真实拨
// 号使用同一条排序规则。
func OrderServerDialPlanWithSuccess(
	plan DialAddressPlan, records []DialSuccessRecord, now time.Time,
) DialAddressPlan {
	if fallbackOnlyDialPlan(plan) {
		return plan
	}
	return orderPlanWithDialSuccess(plan, records, now)
}

func networkSequencesByRegistration(memberships []*models.NetworkMembership) map[string]int {
	ordered := make([]*models.NetworkMembership, 0, len(memberships))
	for _, membership := range memberships {
		if membership != nil {
			ordered = append(ordered, membership)
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].RegistrationOrder != ordered[j].RegistrationOrder {
			return ordered[i].RegistrationOrder < ordered[j].RegistrationOrder
		}
		return ordered[i].NetworkID < ordered[j].NetworkID
	})
	out := make(map[string]int, len(ordered))
	for _, membership := range ordered {
		out[membership.NetworkID]++
	}
	return out
}

// DialTopology 是一次拨号可见的网络拓扑视图。
type DialTopology struct {
	TargetMemberships []*models.NetworkMembership
	DialerMemberships []*models.NetworkMembership
	DialerKnown       bool
	// DialerUnknownReason 保留拓扑来源识别出的本机设备未知原因。
	DialerUnknownReason FallbackOnlyReason
}

// DialTopologySource 在每次按服务器条目拨号时读取当前拓扑。生产实现从当前
// 缓存快照代或本机 vault 读取；测试注入固定视图。
type DialTopologySource interface {
	ServerDialTopology(server *models.Server) (DialTopology, error)
}

// DialSuccessCacheSource 是拓扑来源可以附加的成功地址缓存接口。统一拨号
// 入口在规划候选时读取它，在连接成功或主机密钥不一致时写回。
type DialSuccessCacheSource interface {
	ServerDialSuccess(server *models.Server) ([]DialSuccessRecord, bool, error)
	RecordServerDialSuccess(server *models.Server, record DialSuccessRecord) error
	DeleteServerDialSuccess(server *models.Server, address string) error
}

// DialSuccessCacheResolver lets a production source resolve its concrete cache
// once per dial instead of re-reading role state for each cache operation.
type DialSuccessCacheResolver interface {
	ResolveDialSuccessCache() (DialSuccessCacheSource, error)
}

// DialAttemptOutcome 是按服务器条目拨号的共用入口（ConnectServerEntry）
// 对一个候选地址的观察结论。Category 使
// 用连通探测的共用错误分类；连接成功时为空。Detail 附人可读说明（例如
// 主机密钥不同）。
type DialAttemptOutcome struct {
	OK       bool
	Category models.ProbeErrorCategory
	Detail   string
	Duration time.Duration
}

// DialAttemptObserver 接收按服务器条目拨号的共用入口（ConnectServerEntry）
// 实际试过的每个拓扑候选地址的观察。
// 兜底主机不属于任何登记网络，不会交给观察者；观察记录失败不影响连接。
type DialAttemptObserver interface {
	ObserveDialAttempt(server *models.Server, candidate DialAddress, outcome DialAttemptOutcome)
}

// DialObserverResolver lets a production topology source resolve its concrete
// dial observer once per dial, mirroring DialSuccessCacheResolver.
type DialObserverResolver interface {
	ResolveDialObserver() (DialAttemptObserver, error)
}

// StaticDialTopology 是固定拓扑视图，供测试和调用方显式注入。
type StaticDialTopology DialTopology

// ServerDialTopology 返回同一个固定视图。
func (s StaticDialTopology) ServerDialTopology(*models.Server) (DialTopology, error) {
	return DialTopology(s), nil
}

// ServerDialRequest 是按服务器条目拨号统一入口的输入。Topology 在入口内
// 部按次读取；SuccessCache 显式携带成功地址缓存，空值表示不使用缓存。
type ServerDialRequest struct {
	// Server 是目标服务器条目；兜底主机与端口始终来自这里。
	Server *models.Server
	// Auth 是登录目标服务器使用的认证方式。
	Auth ssh.AuthMethod
	// HostKeys 是主机密钥存储。统一入口按目标服务器条目在这里构建
	// 首次信任（TOFU）回调，锚跟随条目；缓存模式下调用方传入锚定
	// 转发的包装存储即可。
	HostKeys HostKeyStore
	// KeepAlive 为 true 时等价于原先的 ConnectKeepAlive：连接挂周期保活
	// 循环（后台长任务与中继使用）。默认与原先的 Connect 相同，不挂循环。
	KeepAlive bool
	// AddressTimeout 显式覆盖每个候选地址的拨号超时；零值表示读取
	// SSHMGR_SSH_ADDRESS_TIMEOUT（默认 1.5 秒）。候选只有一个地址时不套用。
	AddressTimeout time.Duration
	// Topology 是本次拨号的网络拓扑来源；nil 表示旧部署没有拓扑登记，
	// 只尝试条目的兜底「主机:端口」。
	Topology DialTopologySource
	// SuccessCache 是本次拨号使用的成功地址缓存；nil 表示不使用。
	SuccessCache DialSuccessCacheSource
	// Observer 接收每个实际试过的拓扑候选地址的探测观察；nil 表示本次
	// 拨号不记录。生产拓扑来源可以实现 DialObserverResolver，让入口自动
	// 解析观察者。
	Observer DialAttemptObserver
}

// lowLevelDial 是统一入口的低层连接函数：按「主机、端口」拨一个地址并完成
// 认证。默认值是现有的 connectWith（Connect 与 ConnectKeepAlive 共用的底层
// 函数）；测试注入它，把候选地址映射到进程内 SSH 服务器。
var lowLevelDial = connectWith

// AddressTimeoutEnv 覆盖每个候选地址的拨号超时；默认值是规格定下的 1.5 秒。
const (
	AddressTimeoutEnv     = "SSHMGR_SSH_ADDRESS_TIMEOUT"
	AddressTimeoutDefault = 1500 * time.Millisecond
)

// ResolveAddressTimeout 读取每个候选地址的拨号超时。非法值与非正值在任何
// 地址开始拨号前即报错。
func ResolveAddressTimeout() (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(AddressTimeoutEnv))
	if value == "" {
		return AddressTimeoutDefault, nil
	}
	timeout, err := time.ParseDuration(value)
	if err != nil || timeout <= 0 {
		return 0, fmt.Errorf("%s: invalid duration %q (want a positive Go duration)", AddressTimeoutEnv, value)
	}
	return timeout, nil
}

// ServerDialPlanInput 是一次不建立连接的按服务器条目拨号候选规划输入。
type ServerDialPlanInput struct {
	Server *models.Server
	// HostKeys 读取服务器条目当前的主机密钥锚与虚拟局域网候选地址。
	HostKeys HostKeyStore
	// Topology 是目标设备与拨号设备的网络成员视图。
	Topology DialTopology
	// SuccessCache 用来把仍有效的成功地址排到同级前面；空值表示不读取。
	SuccessCache DialSuccessCacheSource
	// Now 用来判断成功地址记录是否仍有效。
	Now time.Time
}

// ServerDialPlanResult 是候选计划，加上本次规划实际解析出的成功地址缓
// 存；连接成功或主机密钥不一致时，调用方写回这个缓存。
type ServerDialPlanResult struct {
	Plan         DialAddressPlan
	SuccessCache DialSuccessCacheSource
}

// PlanServerDialCandidates 计算按服务器条目拨号实际会尝试的最终候选地
// 址计划：共同网络地址与兜底主机、条目尚无主机密钥锚时只保留虚拟局域网
// 候选、仍有效的成功地址缓存重排。真实拨号入口与只读拓扑查询共用这一条
// 规划；本函数不建立连接、不写任何缓存，缓存读取失败时与真实拨号一致，
// 告警后继续按已排好的顺序规划。
func PlanServerDialCandidates(input ServerDialPlanInput) (ServerDialPlanResult, error) {
	dialerMemberships := input.Topology.DialerMemberships
	if !input.Topology.DialerKnown {
		dialerMemberships = nil
	}
	plan := PlanServerDialAddresses(input.Topology.TargetMemberships, dialerMemberships, input.Server.Host)
	if !input.Topology.DialerKnown && input.Topology.DialerUnknownReason != "" {
		plan.FallbackOnlyReason = input.Topology.DialerUnknownReason
	}
	pin, err := input.HostKeys.GetEntryPin(input.Server)
	if err != nil {
		return ServerDialPlanResult{}, err
	}
	if pin == nil {
		firstTrustNeedsVirtualLan := hasVirtualLanAddress(input.Topology.TargetMemberships)
		if !firstTrustNeedsVirtualLan {
			virtualLan, err := input.HostKeys.EntryVirtualLanAddresses(input.Server)
			if err != nil {
				return ServerDialPlanResult{}, err
			}
			firstTrustNeedsVirtualLan = len(virtualLan) > 0
		}
		if firstTrustNeedsVirtualLan {
			plan = keepVirtualLanCandidates(plan)
			if len(plan.Candidates) == 0 {
				return ServerDialPlanResult{}, errors.New("首次锚定只能经虚拟局域网；当前拨号设备与目标没有共同虚拟局域网。请用带外锚定，或先从虚拟局域网内的机器连接一次。")
			}
		}
	}
	var successCache DialSuccessCacheSource
	if input.SuccessCache != nil && !fallbackOnlyDialPlan(plan) {
		successCache = input.SuccessCache
		if resolver, ok := successCache.(DialSuccessCacheResolver); ok {
			resolved, err := resolver.ResolveDialSuccessCache()
			if err != nil {
				warnDialSuccessCacheFailure("解析", err)
				successCache = nil
			} else {
				successCache = resolved
			}
		}
	}
	if successCache != nil {
		records, present, err := successCache.ServerDialSuccess(input.Server)
		if err != nil {
			warnDialSuccessCacheFailure("读取", err)
		}
		if present {
			plan = OrderServerDialPlanWithSuccess(plan, records, input.Now)
		}
	}
	return ServerDialPlanResult{Plan: plan, SuccessCache: successCache}, nil
}

// ConnectServerEntry 是 sshmgr 发起 SSH 连接的统一生产入口（所有按服务
// 器条目发起的连接都走这里）：按目标服务器条目规划候选地址、逐个试连，
// 返回已建立的客户端与连接说明。主机密钥与
// 条目锚不一致的地址会被跳过；说明只用网络名加序号或「兜底地址」指称，
// 不泄露 IP 或域名。连接成功的地址会写入请求显式携带的成功地址缓存。
func ConnectServerEntry(ctx context.Context, req ServerDialRequest) (*Client, []string, error) {
	addressTimeout := req.AddressTimeout
	if addressTimeout == 0 {
		resolved, err := ResolveAddressTimeout()
		if err != nil {
			return nil, nil, err
		}
		addressTimeout = resolved
	}
	var topology DialTopology
	if req.Topology != nil {
		resolved, err := req.Topology.ServerDialTopology(req.Server)
		if err != nil {
			return nil, nil, err
		}
		topology = resolved
	}
	planned, err := PlanServerDialCandidates(ServerDialPlanInput{
		Server: req.Server, HostKeys: req.HostKeys, Topology: topology,
		SuccessCache: req.SuccessCache, Now: time.Now(),
	})
	if err != nil {
		return nil, nil, err
	}
	plan := planned.Plan
	successCache := planned.SuccessCache
	observer := req.Observer
	if observer == nil {
		if resolver, ok := req.Topology.(DialObserverResolver); ok && resolver != nil {
			resolved, err := resolver.ResolveDialObserver()
			if err != nil {
				warnDialObserverFailure(err)
			} else {
				observer = resolved
			}
		}
	}
	hostKeyCallback, err := HostKeyTOFU(req.HostKeys, req.Server)
	if err != nil {
		return nil, nil, err
	}
	keepAlive := keepAliveSpec{}
	if req.KeepAlive {
		keepAlive = backgroundKeepAlive
	}
	failures := make([]DialAttemptError, 0, len(plan.Candidates))
	var notes []string
	for _, candidate := range plan.Candidates {
		dialCtx := ctx
		stopAddressTimeout := func() {}
		candidateHostKeyCallback := hostKeyCallback
		if len(plan.Candidates) > 1 {
			var releaseHostKeyVerification func()
			dialCtx, releaseHostKeyVerification, stopAddressTimeout = newHostKeyVerificationContext(ctx, addressTimeout)
			candidateHostKeyCallback = hostKeyCallbackReleasingVerification(hostKeyCallback, releaseHostKeyVerification)
		}
		attemptStarted := time.Now()
		cli, err := lowLevelDial(dialCtx, candidate.Address, req.Server.Port, req.Server.User, req.Auth, candidateHostKeyCallback, keepAlive)
		if contextErr := dialCtx.Err(); contextErr != nil {
			if cli != nil {
				cli.Close()
			}
			err = contextErr
			stopAddressTimeout()
			if parentErr := ctx.Err(); parentErr != nil {
				return nil, nil, parentErr
			}
		} else {
			stopAddressTimeout()
		}
		if observer != nil && candidate.Source != DialAddressFallback {
			outcome := DialAttemptOutcome{
				OK: err == nil, Category: ClassifyProbeDialError(err), Duration: time.Since(attemptStarted),
			}
			if errors.Is(err, ErrHostKeyMismatch) {
				outcome.Detail = "主机密钥不同"
			}
			observer.ObserveDialAttempt(req.Server, candidate, outcome)
		}
		if err == nil {
			if successCache != nil {
				record := DialSuccessRecord{
					Address: candidate.Address, SucceededAt: time.Now(), Duration: time.Since(attemptStarted),
				}
				if err := successCache.RecordServerDialSuccess(req.Server, record); err != nil {
					warnDialSuccessCacheFailure("写入", err)
				}
			}
			return cli, notes, nil
		}
		if errors.Is(err, ErrHostKeyMismatch) {
			if successCache != nil {
				if cacheErr := successCache.DeleteServerDialSuccess(req.Server, candidate.Address); cacheErr != nil {
					warnDialSuccessCacheFailure("清除", cacheErr)
				}
			}
			notes = append(notes, candidateLabel(candidate)+"呈现了不同的主机密钥，已跳过")
		}
		failures = append(failures, DialAttemptError{Candidate: candidate, Err: err})
	}
	if len(failures) == 1 && plan.FallbackOnlyReason == "" && !errors.Is(failures[0].Err, ErrHostKeyMismatch) {
		return nil, nil, failures[0].Err
	}
	dialErr := &ServerDialError{Failures: failures, Port: req.Server.Port}
	if plan.FallbackOnlyReason != "" {
		dialErr.Hint = "请补登记本机设备与目标设备的共同网络；单机部署与权威端也可先设定本机设备。"
		if plan.FallbackOnlyReason == FallbackOnlyLocalDeviceDeleted {
			dialErr.Hint = "本机设备设定指向的设备已不存在，请重新设定本机设备。"
		}
	}
	return nil, nil, dialErr
}

func warnDialSuccessCacheFailure(action string, err error) {
	fmt.Fprintf(os.Stderr, "sshmgr: 成功地址缓存%s失败（继续拨号）: %v\n", action, err)
}

func warnDialObserverFailure(err error) {
	fmt.Fprintf(os.Stderr, "sshmgr: 解析探测结果记录器失败（本次拨号不记录）: %v\n", err)
}

func fallbackOnlyDialPlan(plan DialAddressPlan) bool {
	return len(plan.Candidates) == 1 && plan.Candidates[0].Source == DialAddressFallback
}

func hasVirtualLanAddress(memberships []*models.NetworkMembership) bool {
	for _, membership := range memberships {
		if membership != nil && membership.NetworkType == models.NetworkVirtual {
			return true
		}
	}
	return false
}

func keepVirtualLanCandidates(plan DialAddressPlan) DialAddressPlan {
	candidates := make([]DialAddress, 0, len(plan.Candidates))
	for _, candidate := range plan.Candidates {
		if candidate.Source == DialAddressVirtual {
			candidates = append(candidates, candidate)
		}
	}
	plan.Candidates = candidates
	return plan
}

// orderPlanWithDialSuccess 只重排同一优先级级别（物理局域网、虚拟局域
// 网、兜底主机）内部的地址：仍有效且仍在候选
// 里的成功地址按记录耗时从短到长排在前面，其余地址保持原登记顺序。
func orderPlanWithDialSuccess(plan DialAddressPlan, records []DialSuccessRecord, now time.Time) DialAddressPlan {
	durationByAddress := make(map[string]time.Duration, len(records))
	for _, record := range records {
		if !dialSuccessRecordValid(record, now) {
			continue
		}
		durationByAddress[normalizeDialAddress(record.Address)] = record.Duration
	}
	out := make([]DialAddress, 0, len(plan.Candidates))
	for _, source := range dialAddressSourcesInPriorityOrder() {
		var cached, uncached []DialAddress
		for _, candidate := range plan.Candidates {
			if candidate.Source != source {
				continue
			}
			if _, present := durationByAddress[normalizeDialAddress(candidate.Address)]; present {
				cached = append(cached, candidate)
				continue
			}
			uncached = append(uncached, candidate)
		}
		sort.SliceStable(cached, func(i, j int) bool {
			return durationByAddress[normalizeDialAddress(cached[i].Address)] <
				durationByAddress[normalizeDialAddress(cached[j].Address)]
		})
		out = append(out, cached...)
		out = append(out, uncached...)
	}
	plan.Candidates = out
	return plan
}

func dialAddressSourcesInPriorityOrder() []DialAddressSource {
	return []DialAddressSource{DialAddressPhysical, DialAddressVirtual, DialAddressFallback}
}

func dialAddressPriority(source DialAddressSource) int {
	for priority, candidate := range dialAddressSourcesInPriorityOrder() {
		if candidate == source {
			return priority
		}
	}
	return len(dialAddressSourcesInPriorityOrder())
}

func dialSuccessRecordValid(record DialSuccessRecord, now time.Time) bool {
	return record.Address != "" && record.Duration >= 0 &&
		!record.SucceededAt.IsZero() &&
		!record.SucceededAt.After(now) &&
		now.Sub(record.SucceededAt) <= DialSuccessCacheLifetime
}
