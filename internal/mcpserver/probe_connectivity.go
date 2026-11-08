package mcpserver

import (
	"context"
	"time"

	"ssh-manager-mcp/internal/probe"
	"ssh-manager-mcp/internal/probeobserve"
	"ssh-manager-mcp/internal/proberesults"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
)

// ProbeConnectivityInput is the probe_connectivity tool input.
type ProbeConnectivityInput struct {
	From string `json:"from" jsonschema:"probe origin: \"self\" (this sshmgr instance) or a server id from list_servers"`
	// Targets 每项是服务器编号或设备名，可附「:端口」；最多 10 个（去重后）。
	Targets []string `json:"targets" jsonschema:"target list; each item is a server id or device name, optionally as name:port; at most 10 after deduplication"`
}

// ProbeConnectivityForProfile 运行连通探测并写审计。起点或目标里的服务器
// 编号必须在该授权范围内；只要工具正常返回，审计状态就是成功，单个目标
// 不通不算失败。
func ProbeConnectivityForProfile(ctx context.Context, st *store.Store, projectID, profileID, from string, targets []string, hk sshbroker.HostKeyStore) (text string, summary map[string]any, originServerID string, err error) {
	start := time.Now()
	status := "error"
	defer func() {
		if status == "" {
			status = "error"
		}
		_ = st.WriteAudit(store.AuditRow{
			TS: start, ProjectID: projectID, ServerID: originServerID,
			Action: "probe-connectivity", Status: status,
			Source:  store.AuditSourceAgent,
			Command: store.MarshalAuditSummary(summary),
		})
	}()

	service := &probe.Service{
		Store:      st,
		GateServer: func(serverID string) error { return gateServer(st, profileID, serverID) },
		HostKeys:   hk,
	}
	if path, ok, pathErr := probeobserve.PathFor(""); pathErr == nil && ok {
		service.Results = proberesults.NewFileProbeResults(path)
	}
	report, probeErr := service.Probe(ctx, probe.Request{From: from, Targets: targets})
	summary = map[string]any{"from": from, "targets": []string{}}
	if report != nil {
		summary["targets"] = report.TargetSummaries()
		summary["from_device"] = report.FromDevice
		if report.Remote {
			summary["note"] = "固定命令，不经命令策略"
			originServerID = from
		}
	}
	if probeErr != nil {
		summary["error"] = probeErr.Error()
		return "", summary, originServerID, probeErr
	}
	status = "ok"
	return report.Text(), summary, originServerID, nil
}
