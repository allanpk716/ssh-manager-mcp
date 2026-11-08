package probe

// AuditSummary 生成连通探测审计行共用的摘要字段与归属服务器编号。MCP
// 工具与命令行子命令都使用这一份拼接，保证两边字段一致：摘要只写起点、
// 「目标:端口」列表与起点设备名，不写地址；经服务器探测时额外注明固定
// 命令不经命令策略，并把审计行归属到起点服务器编号；探测失败时附错误原
// 因。report 为 nil（参数或授权错误）时只写起点与空目标列表。
func AuditSummary(from string, report *Report, probeErr error) (summary map[string]any, serverID string) {
	summary = map[string]any{"from": from, "targets": []string{}}
	if report != nil {
		summary["targets"] = report.TargetSummaries()
		if report.FromDevice != "" {
			summary["from_device"] = report.FromDevice
		}
		if report.Remote {
			summary["note"] = "固定命令，不经命令策略"
			serverID = from
		}
	}
	if probeErr != nil {
		summary["error"] = probeErr.Error()
	}
	return summary, serverID
}
