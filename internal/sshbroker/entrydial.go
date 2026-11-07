package sshbroker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/models"
)

// DialAddressSource 标明一个候选地址来自哪一级选址优先级。
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

// DialAttemptError 记录一个候选地址的失败结果。后续主机密钥跳过与成功地址
// 缓存票可以在这一层补充结果分类，而不改变统一拨号入口的调用方。
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
	address = strings.TrimSpace(address)
	if ip := net.ParseIP(address); ip != nil {
		return ip.String()
	}
	return strings.ToLower(address)
}

// PlanServerDialAddresses 是按网络拓扑规划候选 SSH 地址的纯函数：只取拨号
// 设备也是成员的网络里的目标地址，加上服务器条目的兜底主机；物理局域网
// 优先于虚拟局域网，兜底主机最后，同级按网络成员登记序号。
func PlanServerDialAddresses(targetMemberships, dialerMemberships []*models.NetworkMembership, fallbackHost string) DialAddressPlan {
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
		rank := 1
		switch membership.NetworkType {
		case models.NetworkPhysical:
			source, rank = DialAddressPhysical, 0
		case models.NetworkVirtual:
			source, rank = DialAddressVirtual, 1
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
			key: normalizeDialAddress(membership.Address), rank: rank,
		})
	}
	prepared = append(prepared, preparedAddress{
		item: DialAddress{Address: fallbackHost, Source: DialAddressFallback},
		key:  normalizeDialAddress(fallbackHost), rank: 2,
	})
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
}

// DialTopologySource 在每次按服务器条目拨号时读取当前拓扑。生产实现从当前
// 缓存快照代或本机 vault 读取；测试注入固定视图。
type DialTopologySource interface {
	ServerDialTopology(server *models.Server) (DialTopology, error)
}

// StaticDialTopology 是固定拓扑视图，供测试和调用方显式注入。
type StaticDialTopology DialTopology

// ServerDialTopology 返回同一个固定视图。
func (s StaticDialTopology) ServerDialTopology(*models.Server) (DialTopology, error) {
	return DialTopology(s), nil
}

// ServerDialRequest 是按服务器条目拨号统一入口的输入。Topology 在入口内
// 部按次读取；成功地址缓存由后续票作为同一来源的附加排序信息接入。
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

// ConnectServerEntry 是 sshmgr 发起 SSH 连接的统一生产入口：按目标服务器
// 条目规划候选地址、逐个试连，返回已建立的客户端与连接说明。主机密钥不
// 一致时的跳过说明与成功地址缓存由后续票在同一个循环内接入。
func ConnectServerEntry(ctx context.Context, req ServerDialRequest) (*Client, []string, error) {
	addressTimeout := req.AddressTimeout
	if addressTimeout == 0 {
		resolved, err := ResolveAddressTimeout()
		if err != nil {
			return nil, nil, err
		}
		addressTimeout = resolved
	}
	plan := PlanServerDialAddresses(nil, nil, req.Server.Host)
	var targetMemberships []*models.NetworkMembership
	if req.Topology != nil {
		topology, err := req.Topology.ServerDialTopology(req.Server)
		if err != nil {
			return nil, nil, err
		}
		dialerMemberships := topology.DialerMemberships
		if !topology.DialerKnown {
			dialerMemberships = nil
		}
		targetMemberships = topology.TargetMemberships
		plan = PlanServerDialAddresses(targetMemberships, dialerMemberships, req.Server.Host)
	}
	pin, err := req.HostKeys.GetEntryPin(req.Server)
	if err != nil {
		return nil, nil, err
	}
	if pin == nil {
		firstTrustNeedsVirtualLan := hasVirtualLanAddress(targetMemberships)
		if !firstTrustNeedsVirtualLan {
			virtualLan, err := req.HostKeys.EntryVirtualLanAddresses(req.Server)
			if err != nil {
				return nil, nil, err
			}
			firstTrustNeedsVirtualLan = len(virtualLan) > 0
		}
		if firstTrustNeedsVirtualLan {
			plan = keepVirtualLanCandidates(plan)
			if len(plan.Candidates) == 0 {
				return nil, nil, errors.New("首次锚定只能经虚拟局域网；当前拨号设备与目标没有共同虚拟局域网。请用带外锚定，或先从虚拟局域网内的机器连接一次。")
			}
		}
	}
	hkCb, err := HostKeyTOFU(req.HostKeys, req.Server)
	if err != nil {
		return nil, nil, err
	}
	ka := keepAliveSpec{}
	if req.KeepAlive {
		ka = backgroundKeepAlive
	}
	failures := make([]DialAttemptError, 0, len(plan.Candidates))
	for _, candidate := range plan.Candidates {
		dialCtx := ctx
		cancel := func() {}
		if len(plan.Candidates) > 1 {
			dialCtx, cancel = context.WithTimeout(ctx, addressTimeout)
		}
		cli, err := lowLevelDial(dialCtx, candidate.Address, req.Server.Port, req.Server.User, req.Auth, hkCb, ka)
		if contextErr := dialCtx.Err(); contextErr != nil {
			if cli != nil {
				cli.Close()
			}
			err = contextErr
			cancel()
			if parentErr := ctx.Err(); parentErr != nil {
				return nil, nil, parentErr
			}
		} else {
			cancel()
		}
		if err == nil {
			return cli, nil, nil
		}
		failures = append(failures, DialAttemptError{Candidate: candidate, Err: err})
	}
	if len(failures) == 1 && plan.FallbackOnlyReason == "" {
		return nil, nil, failures[0].Err
	}
	dialErr := &ServerDialError{Failures: failures, Port: req.Server.Port}
	if plan.FallbackOnlyReason != "" {
		dialErr.Hint = "请补登记本机设备与目标设备的共同网络；单机部署与权威端也可先设定本机设备。"
	}
	return nil, nil, dialErr
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
