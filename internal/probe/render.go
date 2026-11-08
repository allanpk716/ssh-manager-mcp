package probe

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"ssh-manager-mcp/internal/models"
)

// CategoryLabel 把共用错误分类翻译成呈现用的中文短语。
func CategoryLabel(category models.ProbeErrorCategory) string {
	switch category {
	case models.ProbeErrorNone:
		return ""
	case models.ProbeErrorConnectionRefused:
		return "连接被拒绝"
	case models.ProbeErrorTimeout:
		return "连接超时"
	case models.ProbeErrorNoRoute:
		return "无路由"
	case models.ProbeErrorDNSFailure:
		return "名字解析失败"
	case models.ProbeErrorMissingTool:
		return "服务器缺少探测工具"
	case models.ProbeErrorNoResult:
		return "服务器未返回结果"
	case models.ProbeErrorIncomplete:
		return "探测未完成"
	default:
		return "其他"
	}
}

// ObservationAgeLabel 是观察时间的相对呈现：超过 10 分钟加「旧」字样。本工
// 具返回的都是新测结果；这个约定供读取接口与后续查询工具共用。
func ObservationAgeLabel(now, observedAt time.Time) string {
	if observedAt.IsZero() || now.Sub(observedAt) > 10*time.Minute {
		return "旧"
	}
	return "刚刚"
}

// Text 渲染紧凑文本：一个目标一组，每个共同网络的每个地址一行；地址用
// 网络名加第几个地址指称，不暴露 IP 或域名。
func (r *Report) Text() string {
	now := time.Now()
	lines := make([]string, 0, len(r.Targets)*2+2)
	if r.OriginError != "" {
		lines = append(lines, "无法连接起点服务器："+r.OriginError)
	}
	for _, target := range r.Targets {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		header := target.Label + ":" + strconv.Itoa(target.Port)
		switch {
		case target.NoCommonNetwork:
			lines = append(lines, header+"  无共同网络，未探测")
		case target.UnprobedNote != "":
			lines = append(lines, header+"  "+target.UnprobedNote)
		default:
			for _, line := range target.Lines {
				marker := "✗"
				detail := CategoryLabel(line.Category)
				if line.OK {
					marker = "✓"
					detail = ""
					if line.Duration >= 0 {
						detail = fmt.Sprintf("%dms", line.Duration.Milliseconds())
					}
				}
				age := ObservationAgeLabel(now, line.ObservedAt)
				lines = append(lines, fmt.Sprintf("%s  %s 第%d个地址  %s%s %s", header,
					line.NetworkName, line.Sequence, marker, detail, age))
			}
		}
		for _, note := range target.Notes {
			lines = append(lines, "  "+note)
		}
	}
	return strings.Join(lines, "\n")
}

// Summary 返回审计摘要：起点与「目标:端口」列表，只写服务器编号与设备
// 名，不写地址。经服务器探测时额外注明固定命令不经命令策略。
func (r *Report) Summary() string {
	targets := make([]string, 0, len(r.Targets))
	for _, target := range r.Targets {
		targets = append(targets, fmt.Sprintf("%s:%d", target.Label, target.Port))
	}
	from := r.From
	if r.FromDevice != "" {
		from = r.From + "（" + r.FromDevice + "）"
	}
	summary := fmt.Sprintf("from=%s targets=%s", from, strings.Join(targets, ","))
	if r.Remote {
		summary += " 固定命令，不经命令策略"
	}
	return summary
}
