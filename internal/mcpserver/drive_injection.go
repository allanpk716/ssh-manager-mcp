package mcpserver

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
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
	networkDriveReasonAllUnreachable   = "全部候选地址在目标服务器上探测失败"
	networkDriveReasonStatusMissing    = "注入步骤没有返回状态行"
	networkDriveReasonStatusUnreadable = "注入步骤没有返回可识别的结果"
)

// networkDriveWarning 是一个共享没有注入成功的说明，只包含共享名、盘符
// 与原因，绝不包含地址、挂载账号或密码。
type networkDriveWarning struct {
	ShareName string
	Drive     string
	Reason    string
}

// networkDriveInjectionPlan 是一次执行前求出的网络盘符注入计划，与环境变
// 量注入计划并列。items 只包含已经拿到候选地址与密码的共享；warnings 说
// 明每个没有进入 items 的共享及原因。远端结果状态行在执行后由
// consumeStatusLines 解析并从输出中剔除。
type networkDriveInjectionPlan struct {
	items    []sshbroker.NetworkDriveInjection
	warnings []networkDriveWarning
	statuses map[int]string
	// extraTimeout 是选址与映射步骤的耗时预算；执行用户命令时把它加在
	// SSH 超时上，注入耗时就不占用用户命令的超时。
	extraTimeout time.Duration
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
// 机上的 445 探测选出地址。本票只支持 PowerShell 服务器；cmd 与非
// Windows 服务器返回空计划，行为不变。
func buildNetworkDriveInjectionPlan(
	ctx context.Context, st *store.Store, profileID string,
	server *models.Server, privileged bool, executor networkDriveProbeExecutor,
) networkDriveInjectionPlan {
	plan := networkDriveInjectionPlan{statuses: map[int]string{}}
	if st == nil || server == nil || privileged || executor == nil {
		return plan
	}
	if models.NormalizeShellType(string(server.ShellType)) != models.ShellPowerShell {
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
	for _, share := range shares {
		if share == nil || !networkDriveShareVisible(share, visible, server.ID) {
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
		address, reason := networkDriveSelectAddress(ctx, st, server, share, executor)
		probeElapsed += time.Since(probeBegin)
		if reason != "" {
			plan.warn(networkDriveWarning{ShareName: share.Name, Drive: share.Drive, Reason: reason})
			continue
		}
		plan.items = append(plan.items, sshbroker.NetworkDriveInjection{
			ShareName: share.Name, Drive: share.Drive, Address: address,
			UserName: share.MountUser, Password: password, StatusMarker: marker,
		})
	}
	sort.Slice(plan.items, func(i, j int) bool {
		return plan.items[i].ShareName < plan.items[j].ShareName
	})
	sort.Slice(plan.warnings, func(i, j int) bool {
		if plan.warnings[i].ShareName == plan.warnings[j].ShareName {
			return plan.warnings[i].Reason < plan.warnings[j].Reason
		}
		return plan.warnings[i].ShareName < plan.warnings[j].ShareName
	})
	plan.extraTimeout = probeElapsed + sshbroker.NetworkDriveMappingTimeout*time.Duration(len(plan.items))
	return plan
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

// networkDriveSelectAddress 复用 mount_share 的共同网络候选规划与连通探测
// 固定命令，在目标服务器上经一次独立 SSH 执行探测 445，取排序最靠前的连
// 通地址。本票不做缓存。
func networkDriveSelectAddress(
	ctx context.Context, st *store.Store, server *models.Server,
	share *models.SharedStorage, executor networkDriveProbeExecutor,
) (string, string) {
	if server.DeviceID == "" {
		return "", networkDriveReasonNoDevice
	}
	targetDevice, err := st.GetDeviceByID(server.DeviceID)
	if err != nil || targetDevice == nil {
		return "", networkDriveReasonNoDevice
	}
	targetMemberships, err := st.ListNetworkMembershipsByRegistration(targetDevice.Name)
	if err != nil {
		return "", networkDriveReasonProbeFailed
	}
	providerMemberships, err := st.ListNetworkMembershipsByRegistration(share.DeviceName)
	if err != nil {
		return "", networkDriveReasonProbeFailed
	}
	addressPlan := sshbroker.PlanCommonNetworkAddresses(providerMemberships, targetMemberships)
	if len(addressPlan.Candidates) == 0 {
		return "", networkDriveReasonNoCommonNetwork
	}
	addressTimeout, err := sshbroker.ResolveAddressTimeout()
	if err != nil {
		return "", networkDriveReasonProbeFailed
	}
	items := make([]probe.ShellCommandItem, 0, len(addressPlan.Candidates))
	for _, candidate := range addressPlan.Candidates {
		items = append(items, probe.ShellCommandItem{
			Address: candidate.Address, Port: sharemount.SMBPort, Timeout: addressTimeout,
		})
	}
	command, err := probe.BuildShellProbeCommand(models.ShellPowerShell, items)
	if err != nil {
		return "", networkDriveReasonProbeFailed
	}
	result, err := executor.Exec(ctx, command,
		addressTimeout*time.Duration(len(items))+10*time.Second, 64*1024)
	if err != nil {
		return "", networkDriveReasonProbeFailed
	}
	results := probe.ParseShellProbeOutput(result.Stdout, len(items))
	for index, probeResult := range results {
		if probeResult.OK {
			return addressPlan.Candidates[index].Address, ""
		}
	}
	return "", networkDriveReasonAllUnreachable
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
// 类按 #206 真机结论：85 是盘符被占用；53、64、67 与超时是网络类；其余
// 保留系统错误编号。
func networkDriveStatusReason(status, drive string) string {
	switch status {
	case "timeout":
		return "映射超时（3 秒上限），按网络类失败处理"
	case "error-85":
		return "盘符 " + drive + ": 在这台服务器上已被占用"
	case "error-53", "error-64", "error-67":
		code := strings.TrimPrefix(status, "error-")
		return "网络类失败（系统错误 " + code + "）"
	case "error-wrapper":
		return "注入封装执行失败"
	}
	if code, ok := strings.CutPrefix(status, "error-"); ok {
		if _, err := strconv.Atoi(code); err == nil {
			return "系统错误 " + code
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

// appendWarnings 沿用环境变量注入的警告机制：警告附在标准输出末尾，不影
// 响退出码，也不计入输出截断。
func (p networkDriveInjectionPlan) appendWarnings(stdout string) string {
	if len(p.warnings) == 0 {
		return stdout
	}
	if stdout != "" && !strings.HasSuffix(stdout, "\n") {
		stdout += "\n"
	}
	for _, warning := range p.warnings {
		line := "网络盘符未注入：" + warning.Reason
		if warning.ShareName != "" {
			line = fmt.Sprintf("共享 %s 的网络盘符 %s: 未注入：%s",
				warning.ShareName, warning.Drive, warning.Reason)
		}
		stdout += line + "\n"
	}
	return stdout
}
