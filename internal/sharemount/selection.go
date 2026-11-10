package sharemount

import (
	"errors"
	"fmt"
	"strings"

	"ssh-manager-mcp/internal/models"
)

// 选地址失败原因保存在审计摘要里；值只使用稳定的英文代号，呈现文案仍由
// Error 统一生成，避免调用方各自拼出不同说法。
const (
	SelectionErrorNoCommonNetwork     = "no-common-network"
	SelectionErrorAllCandidatesFailed = "all-candidates-failed"
)

// ProbeAttempt 是一个候选地址在目标服务器上的安全探测结论。它只保留网
// 络名、登记序号、错误分类与耗时，不保留 IP 或域名。
type ProbeAttempt struct {
	NetworkName     string                    `json:"network_name"`
	NetworkSequence int                       `json:"network_sequence"`
	Physical        bool                      `json:"physical"`
	OK              bool                      `json:"ok"`
	ErrorCategory   models.ProbeErrorCategory `json:"error_category"`
	DurationMilli   int64                     `json:"duration_ms"`
}

// SelectionFailure 是一次选地址失败的安全元数据。它不携带 IP、域名、挂载
// 账号或密码；MCP 层用它渲染工具错误与审计摘要。
type SelectionFailure struct {
	Reason           string
	ProviderNetworks []string
	TargetNetworks   []string
	Attempts         []ProbeAttempt
}

// FormatProbeAttemptFailure 渲染一个候选地址的探测失败行。挂载、注入与
// 错误呈现共用这一份格式，避免同一行文案在各处漂移。
func FormatProbeAttemptFailure(attempt ProbeAttempt) string {
	line := fmt.Sprintf("%s 第%d个地址  ✗%s", attempt.NetworkName,
		attempt.NetworkSequence, models.ProbeErrorCategoryLabel(attempt.ErrorCategory))
	if attempt.DurationMilli >= 0 {
		line += fmt.Sprintf(" %dms", attempt.DurationMilli)
	}
	return line
}

func (e *SelectionFailure) Error() string {
	switch e.Reason {
	case SelectionErrorNoCommonNetwork:
		return strings.Join([]string{
			"目标服务器所属设备与共享存储提供方设备没有共同网络，未探测",
			"共享存储提供方设备所在网络：" + strings.Join(e.ProviderNetworks, "、"),
			"目标服务器所属设备所在网络：" + strings.Join(e.TargetNetworks, "、"),
			"请 owner 先补齐网络拓扑登记",
		}, "\n")
	case SelectionErrorAllCandidatesFailed:
		lines := []string{"全部候选地址在目标服务器上探测失败，登记与实际不一致："}
		for _, attempt := range e.Attempts {
			lines = append(lines, FormatProbeAttemptFailure(attempt))
		}
		lines = append(lines, "未写入任何挂载配置")
		return strings.Join(lines, "\n")
	default:
		return "目标服务器与提供方设备没有可连通的共同网络候选地址"
	}
}

// Is 让调用方可以按原因比较选地址失败。
func (e *SelectionFailure) Is(target error) bool {
	var other *SelectionFailure
	return errors.As(target, &other) && other.Reason == e.Reason
}

// networkNames 按登记顺序返回设备所在的网络名，并去掉重复网络成员带来的
// 重复名字。网络名是安全呈现的一部分，这里不返回地址。
func networkNames(memberships []*models.NetworkMembership) []string {
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
