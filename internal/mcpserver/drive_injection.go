package mcpserver

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/probe"
	"ssh-manager-mcp/internal/sharemount"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
)

// networkDriveStatusPrefix 是远端注入封装回传结果的状态行行首标记，后接
// 本次执行的随机标记、共享序号与结果。
const networkDriveStatusPrefix = "SSHMGR-SMBMAP:"

// networkDriveAddressCacheLifetime 是选址缓存的存活时间：连续命令在 10 分钟内
// 复用同一选中地址，不再重复探测 445 端口。
const networkDriveAddressCacheLifetime = 10 * time.Minute

const (
	networkDriveReasonListFailed       = "读取共享存储登记失败"
	networkDriveReasonVisibleFailed    = "读取当前 profile 的服务器可见范围失败"
	networkDriveReasonPasswordFailed   = "读取挂载密码机密项失败"
	networkDriveReasonPasswordEmpty    = "挂载密码为空"
	networkDriveReasonPasswordZero     = "挂载密码含空字节"
	networkDriveReasonPasswordUTF8     = "挂载密码在 Windows 上不是合法 UTF-8"
	networkDriveReasonUnsafeValue      = "共享登记的共享名、地址或挂载账号包含无法注入的字符"
	networkDriveReasonNoDevice         = "目标服务器未关联拓扑设备"
	networkDriveReasonNoCommonNetwork  = "目标服务器所属设备与共享存储提供方设备没有共同网络"
	networkDriveReasonProbeFailed      = "候选地址探测失败"
	networkDriveReasonStatusMissing    = "注入步骤没有返回状态行"
	networkDriveReasonStatusUnreadable = "注入步骤没有返回可识别的结果"
	networkDriveReasonAuthentication   = "共享密码认证失败，已暂停注入，请同步或检查机密项"
	networkDriveHintNetwork            = "提供方可能不在线，或提供方开启了「禁止同一 IP 多个连接」限制"
)

// networkDriveWarning 是一个共享没有注入成功的说明，只包含共享名、盘符
// 与原因，绝不包含地址、挂载账号或密码。
type networkDriveWarning struct {
	ShareName string
	Drive     string
	Reason    string
	// Notice 是不需要再套「网络盘符未注入」格式的完整说明，用于认证失败
	// 暂停期间的简短提示。
	Notice string
}

// networkDriveInjectionPlan 是一次执行前求出的网络盘符注入计划，与环境变
// 量注入计划并列。items 只包含已经拿到候选地址与密码的共享；warnings 说
// 明每个没有进入 items 的共享及原因。远端结果状态行在执行后由
// consumeStatusLines 解析并从输出中剔除。
type networkDriveInjectionPlan struct {
	items     []sshbroker.NetworkDriveInjection
	warnings  []networkDriveWarning
	notes     []string
	statuses  map[int]string
	cacheKeys []networkDriveAddressCacheKey
	// shareIDs 与 shareVersions 把状态行序号对应回本地共享登记，只在注入
	// 计划进程内使用，不进入远端命令文本。
	shareIDs      map[int]string
	shareVersions map[int]int64
	// extraTimeout 是选址与映射步骤的耗时预算；执行用户命令时把它加在
	// SSH 超时上，注入耗时就不占用用户命令的超时。
	extraTimeout time.Duration
}

// networkDriveAuthenticationPauseState 是进程内的「共享存储编号 → 暂停时的
// 共享存储版本号」。它不落盘，也不发送到任何目标服务器。
var networkDriveAuthenticationPauseState struct {
	sync.Mutex
	versions map[string]int64
}

// networkDriveAddressCacheKey 唯一确定一份选址缓存：同一服务器条目上的同
// 一个共享，且共享存储登记版本号一致。版本号变化后旧缓存自然失效。
type networkDriveAddressCacheKey struct {
	ServerID string
	Share    string
	Version  int64
}

// networkDriveAddressCandidate 是缓存里保留的候选地址元数据。address 只
// 在进程内用于生成注入命令；呈现给 agent 的说明只使用网络名与登记序号。
type networkDriveAddressCandidate struct {
	Address     string
	NetworkName string
	Sequence    int
	Physical    bool
}

// networkDriveAddressSelectionEntry 是一次选址留在进程内存里的缓存项：
// 全部候选、因网络类映射失败作废的地址、当前选中的地址，以及改走虚拟局
// 域网时的安全说明。缓存不落盘、不同步给权威端。
type networkDriveAddressSelectionEntry struct {
	candidates  []networkDriveAddressCandidate
	unavailable map[string]bool
	address     string
	note        string
	expiresAt   time.Time
}

// networkDriveAddressCacheView 是选址前从缓存读到的一次快照。ready 表示
// 可以直接复用；waiting 表示全部候选都已作废，应等缓存过期；probeList 是
// 这次仍要探测的候选。
type networkDriveAddressCacheView struct {
	ready       bool
	waiting     bool
	address     string
	note        string
	probeList   []networkDriveAddressCandidate
	unavailable map[string]bool
}

// networkDriveAddressSelectionCache 是网络盘符注入的进程内选址缓存。并发
// 调用 exec_command 时可能同时选址，全部读写都在互斥锁内完成。
type networkDriveAddressSelectionCache struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[networkDriveAddressCacheKey]*networkDriveAddressSelectionEntry
}

// networkDriveAddressCache 是进程级缓存：同一个 sshmgr 进程里的后续命令
// 共享选址结论。
var networkDriveAddressCache = newNetworkDriveAddressCache(time.Now)

func newNetworkDriveAddressCache(now func() time.Time) *networkDriveAddressSelectionCache {
	return &networkDriveAddressSelectionCache{
		now:     now,
		entries: map[networkDriveAddressCacheKey]*networkDriveAddressSelectionEntry{},
	}
}

// view 返回一份缓存快照。缓存过期或版本变化后视为没有缓存；选中地址已
// 被作废时给出待探测候选，全部候选都被作废时给出等待结论。
func (c *networkDriveAddressSelectionCache) view(key networkDriveAddressCacheKey) networkDriveAddressCacheView {
	c.mu.Lock()
	defer c.mu.Unlock()
	for cached := range c.entries {
		if cached.ServerID == key.ServerID && cached.Share == key.Share && cached.Version != key.Version {
			delete(c.entries, cached)
		}
	}
	entry, ok := c.entries[key]
	if !ok {
		return networkDriveAddressCacheView{}
	}
	if !c.now().Before(entry.expiresAt) {
		delete(c.entries, key)
		return networkDriveAddressCacheView{}
	}
	if entry.address != "" && !entry.unavailable[entry.address] {
		return networkDriveAddressCacheView{ready: true, address: entry.address, note: entry.note}
	}
	probeList := make([]networkDriveAddressCandidate, 0, len(entry.candidates))
	unavailable := make(map[string]bool, len(entry.unavailable))
	for address, marked := range entry.unavailable {
		unavailable[address] = marked
	}
	for _, candidate := range entry.candidates {
		if !entry.unavailable[candidate.Address] {
			probeList = append(probeList, candidate)
		}
	}
	return networkDriveAddressCacheView{
		waiting: len(probeList) == 0, probeList: probeList, unavailable: unavailable,
	}
}

// record 写入一次选址结论。如果是在作废地址后的重选，保留原来的过期时间
// 与作废标记；全新选址则按当前时间重新计算 10 分钟存活期。
func (c *networkDriveAddressSelectionCache) record(
	key networkDriveAddressCacheKey,
	candidates []networkDriveAddressCandidate, selected networkDriveAddressCandidate, note string,
) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for cached := range c.entries {
		if cached.ServerID == key.ServerID && cached.Share == key.Share && cached.Version != key.Version {
			delete(c.entries, cached)
		}
	}
	entry := c.entries[key]
	if entry == nil || !c.now().Before(entry.expiresAt) {
		entry = &networkDriveAddressSelectionEntry{
			candidates:  candidates,
			unavailable: map[string]bool{},
			expiresAt:   c.now().Add(networkDriveAddressCacheLifetime),
		}
		c.entries[key] = entry
	}
	entry.candidates = candidates
	entry.address = selected.Address
	entry.note = note
}

// markUnavailable 在注入遇到网络类映射失败后作废缓存里的地址：下一条命
// 令改选下一个候选，全部候选作废后等缓存过期再重新探测。
func (c *networkDriveAddressSelectionCache) markUnavailable(key networkDriveAddressCacheKey, address string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || entry.unavailable[address] {
		return
	}
	entry.unavailable[address] = true
	if entry.address == address {
		entry.address, entry.note = "", ""
	}
}

// networkDriveProbeExecutor 是在目标服务器上执行固定探测命令的注入点；
// 生产路径传入已连接的 SSH 客户端，每次探测是一次独立的 SSH 执行，不经
// 过命令策略。
type networkDriveProbeExecutor interface {
	Exec(ctx context.Context, cmd string, timeout time.Duration, maxBytes int64, options ...sshbroker.ExecOption) (sshbroker.ExecResult, error)
}

// buildNetworkDriveInjectionPlan 按目标服务器求出要注入的共享。规则与
// mount_share 的授权判定一致：服务器在共享挂载密码机密项的可写入服务器列
// 表内、共享对当前 profile 可见、能读到密码，再经共同网络候选规划与目标
// 机上的 445 探测选出地址。PowerShell 服务器直接执行映射封装；cmd 服务器
// 由封装启动 PowerShell 子进程执行映射。非 Windows 服务器返回空计划，行
// 为不变。
func buildNetworkDriveInjectionPlan(
	ctx context.Context, st *store.Store, profileID string,
	server *models.Server, privileged bool, executor networkDriveProbeExecutor,
) networkDriveInjectionPlan {
	plan := networkDriveInjectionPlan{statuses: map[int]string{}}
	if st == nil || server == nil || privileged || executor == nil {
		return plan
	}
	shellType := models.NormalizeShellType(string(server.ShellType))
	if shellType != models.ShellPowerShell && shellType != models.ShellWindowsCmd {
		return plan
	}
	shares, err := st.ListSharedStorages(store.SharedStorageFilter{Server: server.ID})
	if err != nil {
		plan.warn(networkDriveWarning{Reason: networkDriveReasonListFailed})
		return plan
	}
	if len(shares) == 0 {
		return plan
	}
	visible, err := sessionVisibleServerIDs(st, profileID, "")
	if err != nil {
		plan.warn(networkDriveWarning{Reason: networkDriveReasonVisibleFailed})
		return plan
	}

	marker, err := newNetworkDriveStatusMarker()
	if err != nil {
		plan.warn(networkDriveWarning{Reason: networkDriveReasonStatusUnreadable})
		return plan
	}
	probeElapsed := time.Duration(0)
	shareIDsByName := make(map[string]string, len(shares))
	shareVersionsByName := make(map[string]int64, len(shares))
	for _, share := range shares {
		if share == nil || !networkDriveShareVisible(share, visible, server.ID) {
			continue
		}
		if networkDriveAuthenticationPaused(share.ID, share.Version) {
			plan.warn(networkDriveWarning{
				ShareName: share.Name, Drive: share.Drive,
				Notice: "共享 " + share.Name + " 因认证失败已暂停注入",
			})
			continue
		}
		password, reason := networkDriveMountPassword(st, share)
		if reason != "" {
			plan.warn(networkDriveWarning{ShareName: share.Name, Drive: share.Drive, Reason: reason})
			continue
		}
		if !networkDriveScriptValuesSafe(share) {
			plan.warn(networkDriveWarning{ShareName: share.Name, Drive: share.Drive, Reason: networkDriveReasonUnsafeValue})
			continue
		}
		probeBegin := time.Now()
		selection := networkDriveSelectAddress(ctx, st, server, share, executor)
		probeElapsed += time.Since(probeBegin)
		if selection.Reason != "" {
			plan.warn(networkDriveWarning{ShareName: share.Name, Drive: share.Drive, Reason: selection.Reason})
			continue
		}
		shareIDsByName[share.Name] = share.ID
		shareVersionsByName[share.Name] = share.Version
		if selection.Note != "" {
			plan.notes = append(plan.notes, fmt.Sprintf("共享 %s 的网络盘符 %s: %s",
				share.Name, share.Drive, selection.Note))
		}
		plan.cacheKeys = append(plan.cacheKeys, networkDriveAddressCacheKey{
			ServerID: server.ID, Share: share.Name, Version: share.Version,
		})
		plan.items = append(plan.items, sshbroker.NetworkDriveInjection{
			ShareName: share.Name, Drive: share.Drive, Address: selection.Address,
			UserName: share.MountUser, Password: password, StatusMarker: marker,
		})
	}
	sort.Slice(plan.items, func(i, j int) bool {
		return plan.items[i].ShareName < plan.items[j].ShareName
	})
	sort.Slice(plan.cacheKeys, func(i, j int) bool {
		return plan.cacheKeys[i].Share < plan.cacheKeys[j].Share
	})
	plan.shareIDs = make(map[int]string, len(plan.items))
	plan.shareVersions = make(map[int]int64, len(plan.items))
	for index, item := range plan.items {
		plan.shareIDs[index] = shareIDsByName[item.ShareName]
		plan.shareVersions[index] = shareVersionsByName[item.ShareName]
	}
	sort.Slice(plan.warnings, func(i, j int) bool {
		if plan.warnings[i].ShareName == plan.warnings[j].ShareName {
			return plan.warnings[i].Reason < plan.warnings[j].Reason
		}
		return plan.warnings[i].ShareName < plan.warnings[j].ShareName
	})
	plan.extraTimeout = probeElapsed + sshbroker.NetworkDriveMappingTimeout*time.Duration(len(plan.items))
	return plan
}

// networkDriveAuthenticationPaused 判断共享是否处于认证失败暂停；快照中的
// 版本号已经变化时先解除暂停，让新登记自动恢复注入。
func networkDriveAuthenticationPaused(shareID string, version int64) bool {
	networkDriveAuthenticationPauseState.Lock()
	defer networkDriveAuthenticationPauseState.Unlock()
	pausedVersion, exists := networkDriveAuthenticationPauseState.versions[shareID]
	if !exists {
		return false
	}
	if pausedVersion != version {
		delete(networkDriveAuthenticationPauseState.versions, shareID)
		return false
	}
	return true
}

// pauseNetworkDriveAuthentication 记录认证失败时的共享存储版本号。之后同
// 一进程对所有服务器都跳过该共享，直到快照带来不同版本号。
func pauseNetworkDriveAuthentication(shareID string, version int64) {
	networkDriveAuthenticationPauseState.Lock()
	defer networkDriveAuthenticationPauseState.Unlock()
	if networkDriveAuthenticationPauseState.versions == nil {
		networkDriveAuthenticationPauseState.versions = map[string]int64{}
	}
	networkDriveAuthenticationPauseState.versions[shareID] = version
}

func networkDriveAuthenticationFailed(status string) bool {
	return status == "error-86" || status == "error-1326"
}

func (p *networkDriveInjectionPlan) warn(warning networkDriveWarning) {
	p.warnings = append(p.warnings, warning)
}

// networkDriveShareVisible 与 mount_share 使用同一判定：共享的某个可挂载服
// 务器对当前 profile 可见，且目标服务器本身在可挂载范围内。
func networkDriveShareVisible(share *models.SharedStorage, visible map[string]bool, serverID string) bool {
	mountable := false
	shareVisible := false
	for _, candidate := range share.MountableServers {
		if candidate.ID == serverID {
			mountable = true
		}
		if visible[candidate.ID] {
			shareVisible = true
		}
	}
	return mountable && shareVisible
}

// networkDriveMountPassword 读取并校验挂载密码。密码只在进程内传递，绝不
// 进入错误文本。
func networkDriveMountPassword(st *store.Store, share *models.SharedStorage) ([]byte, string) {
	secretValue, err := st.SecretValueForSharedStorageMount(share.PasswordSecretID)
	if err != nil {
		return nil, networkDriveReasonPasswordFailed
	}
	if len(secretValue.Value) == 0 {
		return nil, networkDriveReasonPasswordEmpty
	}
	if strings.ContainsRune(string(secretValue.Value), 0) {
		return nil, networkDriveReasonPasswordZero
	}
	if !utf8.Valid(secretValue.Value) {
		return nil, networkDriveReasonPasswordUTF8
	}
	return append([]byte(nil), secretValue.Value...), ""
}

// networkDriveScriptValuesSafe 预检会进入远端封装脚本的登记值。sshbroker
// 仍有一层防线；这里失败只产生警告，不挡用户命令。
func networkDriveScriptValuesSafe(share *models.SharedStorage) bool {
	if len(share.Drive) != 1 || share.Drive < "D" || share.Drive > "Z" {
		return false
	}
	for _, value := range []string{share.Name, share.MountUser} {
		if value == "" || strings.ContainsAny(value, "\"\r\n") {
			return false
		}
		for _, symbol := range value {
			if symbol < 0x20 || symbol == 0x7f {
				return false
			}
		}
	}
	return true
}

// networkDriveNetworkNames 按登记顺序返回设备所在的网络名并去掉重复名
// 字。警告只用网络名呈现拓扑，不出现地址。
func networkDriveNetworkNames(memberships []*models.NetworkMembership) []string {
	names := make([]string, 0, len(memberships))
	seen := make(map[string]bool, len(memberships))
	for _, membership := range memberships {
		if membership == nil || membership.NetworkName == "" || seen[membership.NetworkName] {
			continue
		}
		seen[membership.NetworkName] = true
		names = append(names, membership.NetworkName)
	}
	return names
}

func networkDriveNetworkNamesLabel(names []string) string {
	if len(names) == 0 {
		return "（无）"
	}
	return strings.Join(names, "、")
}

// networkDriveNoDeviceReason 渲染目标服务器没有关联设备时的警告，列出提
// 供方与目标两侧的网络名，呈现约定与 mount_share 一致。
func networkDriveNoDeviceReason(
	providerMemberships []*models.NetworkMembership, providerErr error,
) string {
	reason := networkDriveReasonNoDevice + "，未探测"
	if providerErr != nil {
		return reason
	}
	return strings.Join([]string{
		reason,
		"共享存储提供方设备所在网络：" + networkDriveNetworkNamesLabel(networkDriveNetworkNames(providerMemberships)),
		"目标服务器所属设备所在网络：（无）",
		"请 owner 先补齐网络拓扑登记",
	}, "\n")
}

// networkDriveNoCommonNetworkReason 渲染没有共同网络时的警告，逐侧列出网
// 络名，说明没有做任何探测。
func networkDriveNoCommonNetworkReason(providerMemberships, targetMemberships []*models.NetworkMembership) string {
	return strings.Join([]string{
		networkDriveReasonNoCommonNetwork + "，未探测",
		"共享存储提供方设备所在网络：" + networkDriveNetworkNamesLabel(networkDriveNetworkNames(providerMemberships)),
		"目标服务器所属设备所在网络：" + networkDriveNetworkNamesLabel(networkDriveNetworkNames(targetMemberships)),
		"请 owner 先补齐网络拓扑登记",
	}, "\n")
}

// networkDriveAddressSelection 是一次选址的结果：成功时给出地址与可选的
// 虚拟局域网说明，失败时给出不含地址、挂载账号或密码的原因。
type networkDriveAddressSelection struct {
	Address string
	Reason  string
	Note    string
}

// networkDriveSelectAddress 复用 mount_share 的共同网络候选规划与连通探测
// 固定命令，在目标服务器上经一次独立 SSH 执行探测 445，取排序最靠前的连
// 通地址。选中地址按（服务器条目、共享、共享存储版本号）在进程内缓存
// 10 分钟；网络类映射失败作废的地址不再进入后续探测。
func networkDriveSelectAddress(
	ctx context.Context, st *store.Store, server *models.Server,
	share *models.SharedStorage, executor networkDriveProbeExecutor,
) networkDriveAddressSelection {
	providerMemberships, providerErr := st.ListNetworkMembershipsByRegistration(share.DeviceName)
	if server.DeviceID == "" {
		return networkDriveAddressSelection{Reason: networkDriveNoDeviceReason(providerMemberships, providerErr)}
	}
	targetDevice, err := st.GetDeviceByID(server.DeviceID)
	if err != nil || targetDevice == nil {
		return networkDriveAddressSelection{Reason: networkDriveNoDeviceReason(providerMemberships, providerErr)}
	}
	targetMemberships, err := st.ListNetworkMembershipsByRegistration(targetDevice.Name)
	if err != nil {
		return networkDriveAddressSelection{Reason: networkDriveReasonProbeFailed}
	}
	if providerErr != nil {
		return networkDriveAddressSelection{Reason: networkDriveReasonProbeFailed}
	}
	addressPlan := sshbroker.PlanCommonNetworkAddresses(providerMemberships, targetMemberships)
	if len(addressPlan.Candidates) == 0 {
		return networkDriveAddressSelection{Reason: networkDriveNoCommonNetworkReason(
			providerMemberships, targetMemberships)}
	}
	candidates := make([]networkDriveAddressCandidate, 0, len(addressPlan.Candidates))
	for _, candidate := range addressPlan.Candidates {
		candidates = append(candidates, networkDriveAddressCandidate{
			Address: candidate.Address, NetworkName: candidate.NetworkName,
			Sequence: candidate.NetworkSequence,
			Physical: candidate.Source == sshbroker.DialAddressPhysical,
		})
	}
	cacheKey := networkDriveAddressCacheKey{ServerID: server.ID, Share: share.Name, Version: share.Version}
	view := networkDriveAddressCache.view(cacheKey)
	if view.ready {
		return networkDriveAddressSelection{Address: view.address, Note: view.note}
	}
	if view.waiting {
		return networkDriveAddressSelection{Reason: networkDriveWaitingReason(candidates)}
	}
	probeList := candidates
	if view.probeList != nil {
		probeList = view.probeList
	}
	addressTimeout, err := sshbroker.ResolveAddressTimeout()
	if err != nil {
		return networkDriveAddressSelection{Reason: networkDriveReasonProbeFailed}
	}
	items := make([]probe.ShellCommandItem, 0, len(probeList))
	for _, candidate := range probeList {
		items = append(items, probe.ShellCommandItem{
			Address: candidate.Address, Port: sharemount.SMBPort, Timeout: addressTimeout,
		})
	}
	command, err := probe.BuildShellProbeCommand(
		models.NormalizeShellType(string(server.ShellType)), items)
	if err != nil {
		return networkDriveAddressSelection{Reason: networkDriveReasonProbeFailed}
	}
	result, err := executor.Exec(ctx, command,
		addressTimeout*time.Duration(len(items))+10*time.Second, 64*1024)
	if err != nil {
		return networkDriveAddressSelection{Reason: networkDriveReasonProbeFailed}
	}
	results := probe.ParseShellProbeOutput(result.Stdout, len(items))
	attempts := make([]sharemount.ProbeAttempt, 0, len(items))
	for index, probeResult := range results {
		attempts = append(attempts, sharemount.ProbeAttempt{
			NetworkName:     probeList[index].NetworkName,
			NetworkSequence: probeList[index].Sequence,
			Physical:        probeList[index].Physical, OK: probeResult.OK,
			ErrorCategory: probeResult.Category, DurationMilli: probeResult.Duration.Milliseconds(),
		})
		if probeResult.OK {
			selected := probeList[index]
			note := networkDriveVirtualNetworkNote(selected, candidates, attempts, view.unavailable)
			networkDriveAddressCache.record(cacheKey, candidates, selected, note)
			return networkDriveAddressSelection{Address: selected.Address, Note: note}
		}
	}
	return networkDriveAddressSelection{Reason: networkDriveAllUnreachableReason(attempts)}
}

// networkDriveWaitingReason 渲染全部候选都已作废时的等待说明，只使用网络
// 名与登记序号，不出现 IP 或域名。
func networkDriveWaitingReason(candidates []networkDriveAddressCandidate) string {
	entries := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		entries = append(entries, fmt.Sprintf("%s 第%d个地址", candidate.NetworkName, candidate.Sequence))
	}
	return "全部候选地址均已因网络类映射失败标记为不可用，等待缓存过期后重新探测：" +
		strings.Join(entries, "、")
}

// networkDriveAllUnreachableReason 沿用 mount_share 选址失败的呈现约定：逐
// 个列出网络名、登记序号、错误分类与耗时，不出现 IP 或域名。
func networkDriveAllUnreachableReason(attempts []sharemount.ProbeAttempt) string {
	lines := make([]string, 0, len(attempts)+1)
	lines = append(lines, "全部候选地址在目标服务器上探测失败，登记与实际不一致：")
	for _, attempt := range attempts {
		line := fmt.Sprintf("%s 第%d个地址  ✗%s", attempt.NetworkName,
			attempt.NetworkSequence, models.ProbeErrorCategoryLabel(attempt.ErrorCategory))
		if attempt.DurationMilli >= 0 {
			line += fmt.Sprintf(" %dms", attempt.DurationMilli)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// networkDriveVirtualNetworkNote 在物理局域网候选不可用、改走虚拟局域网时
// 生成附在输出末尾的安全说明，呈现约定与 mount_share 一致。
func networkDriveVirtualNetworkNote(
	selected networkDriveAddressCandidate, candidates []networkDriveAddressCandidate,
	attempts []sharemount.ProbeAttempt, unavailable map[string]bool,
) string {
	if selected.Physical {
		return ""
	}
	physicalFailures := make([]string, 0)
	for _, candidate := range candidates {
		if !candidate.Physical || candidate.Address == selected.Address {
			continue
		}
		if unavailable[candidate.Address] {
			physicalFailures = append(physicalFailures,
				fmt.Sprintf("%s 第%d个地址（网络类映射失败）", candidate.NetworkName, candidate.Sequence))
			continue
		}
		for _, attempt := range attempts {
			if attempt.NetworkName == candidate.NetworkName &&
				attempt.NetworkSequence == candidate.Sequence && !attempt.OK {
				line := fmt.Sprintf("%s 第%d个地址  ✗%s", attempt.NetworkName,
					attempt.NetworkSequence, models.ProbeErrorCategoryLabel(attempt.ErrorCategory))
				if attempt.DurationMilli >= 0 {
					line += fmt.Sprintf(" %dms", attempt.DurationMilli)
				}
				physicalFailures = append(physicalFailures, line)
			}
		}
	}
	if len(physicalFailures) == 0 {
		return ""
	}
	return fmt.Sprintf("已使用虚拟局域网：%s 第%d个地址，性能可能下降；物理局域网地址不通：%s",
		selected.NetworkName, selected.Sequence, strings.Join(physicalFailures, "；"))
}

func newNetworkDriveStatusMarker() (string, error) {
	buffer := make([]byte, 16)
	if _, err := crand.Read(buffer); err != nil {
		return "", fmt.Errorf("generate network drive status marker: %w", err)
	}
	return hex.EncodeToString(buffer), nil
}

// consumeStatusLines 从执行输出里剔除带本次随机标记的状态行，解析每个共
// 享的映射结果；缺失或不可识别的结果转成警告。状态行必须在输出遮蔽前
// 剔除，内部标记绝不返回给 agent。
func (p *networkDriveInjectionPlan) consumeStatusLines(stdout, stderr string) (string, string) {
	if p == nil || len(p.items) == 0 {
		return stdout, stderr
	}
	if p.statuses == nil {
		p.statuses = map[int]string{}
	}
	stdout = p.stripStatusLines(stdout)
	stderr = p.stripStatusLines(stderr)
	for index, item := range p.items {
		status, ok := p.statuses[index]
		if !ok {
			p.warn(networkDriveWarning{
				ShareName: item.ShareName, Drive: item.Drive,
				Reason: networkDriveReasonStatusMissing,
			})
			continue
		}
		if status == "ok" {
			continue
		}
		if networkDriveAuthenticationFailed(status) {
			pauseNetworkDriveAuthentication(p.shareIDs[index], p.shareVersions[index])
		}
		// 网络类映射失败说明探测结论不可信（透明代理可能误报连通），把该
		// 地址在进程内缓存里作废，下一条命令改选下一个候选。
		if networkDriveStatusIsNetworkFailure(status) && index < len(p.cacheKeys) {
			networkDriveAddressCache.markUnavailable(p.cacheKeys[index], item.Address)
		}
		p.warn(networkDriveWarning{
			ShareName: item.ShareName, Drive: item.Drive,
			Reason: networkDriveStatusReason(status, item.Drive),
		})
	}
	return stdout, stderr
}

func (p *networkDriveInjectionPlan) stripStatusLines(output string) string {
	if !strings.Contains(output, networkDriveStatusPrefix) {
		return output
	}
	kept := make([]string, 0, 8)
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSuffix(line, "\r")
		status, ok := p.parseStatusLine(trimmed)
		if ok {
			p.statuses[status.index] = status.status
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

type networkDriveParsedStatus struct {
	index  int
	status string
}

func (p *networkDriveInjectionPlan) parseStatusLine(line string) (networkDriveParsedStatus, bool) {
	for index := range p.items {
		prefix := fmt.Sprintf("%s%s:%d:", networkDriveStatusPrefix, p.items[index].StatusMarker, index)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		status := strings.TrimPrefix(line, prefix)
		if status == "" || strings.ContainsAny(status, " \t\r\n") {
			continue
		}
		return networkDriveParsedStatus{index: index, status: status}, true
	}
	return networkDriveParsedStatus{}, false
}

// networkDriveStatusReason 把远端回传的映射结果转成面向 agent 的原因。分
// 类按 #206 真机结论：86 与 1326 是认证失败；85 是盘符被占用；53、64、67
// 与超时是网络类；1219 表示同一服务器已有不同凭据的连接；其余保留系统错
// 误编号。
func networkDriveStatusReason(status, drive string) string {
	switch status {
	case "timeout":
		return "映射超时（3 秒上限）：" + networkDriveHintNetwork
	case "error-85":
		return "盘符 " + drive + ": 在这台服务器上已被占用"
	case "error-86", "error-1326":
		return networkDriveReasonAuthentication
	case "error-53", "error-64", "error-67":
		code := strings.TrimPrefix(status, "error-")
		return "网络类失败（系统错误 " + code + "）：" + networkDriveHintNetwork
	case "error-1219":
		return "同一服务器已有不同凭据的连接（系统错误 1219）"
	case "error-wrapper":
		return "注入封装执行失败"
	}
	if code, ok := strings.CutPrefix(status, "error-"); ok {
		if _, err := strconv.Atoi(code); err == nil {
			return "其他错误（系统错误 " + code + "）"
		}
	}
	return networkDriveReasonStatusUnreadable
}

// injectedDrives 返回映射成功的共享与盘符；映射失败的共享只出现在警告里。
func (p networkDriveInjectionPlan) injectedDrives() []InjectedNetworkDrive {
	drives := make([]InjectedNetworkDrive, 0, len(p.items))
	for index, item := range p.items {
		if p.statuses[index] != "ok" {
			continue
		}
		drives = append(drives, InjectedNetworkDrive{Share: item.ShareName, Drive: item.Drive})
	}
	if len(drives) == 0 {
		return nil
	}
	return drives
}

func (p networkDriveInjectionPlan) injectedShareNames() []string {
	drives := p.injectedDrives()
	if len(drives) == 0 {
		return nil
	}
	names := make([]string, 0, len(drives))
	for _, drive := range drives {
		names = append(names, drive.Share)
	}
	return names
}

func (p networkDriveInjectionPlan) injectionItems() []sshbroker.ExecInjection {
	items := make([]sshbroker.ExecInjection, 0, len(p.items))
	for _, item := range p.items {
		items = append(items, item)
	}
	return items
}

// networkDriveStatusIsNetworkFailure 判断映射结果是否属于网络类失败。判
// 定只服务于选址缓存作废；完整的警告分类与认证失败暂停属于后续票。
func networkDriveStatusIsNetworkFailure(status string) bool {
	switch status {
	case "timeout", "error-53", "error-64", "error-67":
		return true
	}
	return false
}

// appendWarnings 沿用环境变量注入的警告机制：警告与说明附在标准输出末
// 尾，不影响退出码，也不计入输出截断。
func (p networkDriveInjectionPlan) appendWarnings(stdout string) string {
	if len(p.warnings) == 0 && len(p.notes) == 0 {
		return stdout
	}
	if stdout != "" && !strings.HasSuffix(stdout, "\n") {
		stdout += "\n"
	}
	for _, warning := range p.warnings {
		if warning.Notice != "" {
			stdout += warning.Notice + "\n"
			continue
		}
		line := "网络盘符未注入：" + warning.Reason
		if warning.ShareName != "" {
			line = fmt.Sprintf("共享 %s 的网络盘符 %s: 未注入：%s",
				warning.ShareName, warning.Drive, warning.Reason)
		}
		stdout += line + "\n"
	}
	for _, note := range p.notes {
		stdout += note + "\n"
	}
	return stdout
}
