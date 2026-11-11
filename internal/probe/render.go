package probe

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"ssh-manager-mcp/internal/models"
)

// StaleObservationAge 是一次观察被判为「旧」的时间阈值。
const StaleObservationAge = 10 * time.Minute

// ObservationAgeLabel 是观察时间的相对呈现：先按分钟、小时、天向下取整，
// 超过 StaleObservationAge 时追加「旧」字样。连通探测工具返回的都是新测
// 结果，仍会显示为「刚刚」；这个约定供读取接口与后续查询工具共用。
func ObservationAgeLabel(now, observedAt time.Time) string {
	if observedAt.IsZero() {
		return "旧"
	}
	age := now.Sub(observedAt)
	if age < time.Minute {
		return "刚刚"
	}
	label := ""
	switch {
	case age < time.Hour:
		label = fmt.Sprintf("%d分钟前", int64(age/time.Minute))
	case age < 24*time.Hour:
		label = fmt.Sprintf("%d小时前", int64(age/time.Hour))
	default:
		label = fmt.Sprintf("%d天前", int64(age/(24*time.Hour)))
	}
	if age > StaleObservationAge {
		return label + " 旧"
	}
	return label
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
				detail := models.ProbeErrorCategoryLabel(line.Category)
				if line.OK {
					marker = "✓"
					detail = ""
					if line.Duration >= 0 {
						detail = fmt.Sprintf("%dms", line.Duration.Milliseconds())
					}
				}
				if line.Category == models.ProbeErrorIncomplete {
					// 被总时限打断的行没有观察时间，不显示「刚刚」或
					//「旧」。
					lines = append(lines, fmt.Sprintf("%s  %s 第%d个地址  %s%s", header,
						line.NetworkName, line.Sequence, marker, detail))
					continue
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

// TargetSummaries 返回「目标:端口」列表，只含服务器编号与设备名。
func (r *Report) TargetSummaries() []string {
	targets := make([]string, 0, len(r.Targets))
	for _, target := range r.Targets {
		targets = append(targets, fmt.Sprintf("%s:%d", target.Label, target.Port))
	}
	return targets
}
