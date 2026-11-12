package mcpserver

import (
	"context"
	"os"
	"time"

	"ssh-manager-mcp/internal/clientfeatures"
	"ssh-manager-mcp/internal/probe"
	"ssh-manager-mcp/internal/probeobserve"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
)

// ProbeConnectivityInput is the probe_connectivity tool input.
type ProbeConnectivityInput struct {
	From string `json:"from" jsonschema:"probe origin: \"self\" (this sshmgr instance) or a server id from list_servers"`
	// Targets 每项是服务器编号或设备名，可附「:端口」；最多 10 个（去重后）。
	Targets []string `json:"targets" jsonschema:"target list; each item is a server id or device name, optionally as name:port; at most 10 after deduplication"`
}

// bridgeCacheInstance 返回当前桥进程所属的缓存实例名。命令行启动
// mcp --cache 时把解析好的实例名固定在 SSHMGR_MCP_INSTANCE（与固定实例目
// 录的 SSHMGR_CACHE_DIR 同一处），桥内工具据此解析 self 设备与探测结果文
// 件；默认槽与权威端为空字符串。
func bridgeCacheInstance() string {
	return os.Getenv("SSHMGR_MCP_INSTANCE")
}

// ProbeConnectivityForProfile 运行连通探测并写审计。起点或目标里的服务器
// 编号必须在该授权范围内；只要工具正常返回，审计状态就是成功，单个目标
// 不通不算失败。
func ProbeConnectivityForProfile(ctx context.Context, st *store.Store, projectID, profileID, from string, targets []string, hk sshbroker.HostKeyStore) (string, error) {
	if st.CacheAuthorityTooOld() {
		return "", clientfeatures.ErrTopologyAndSharedStorageAuthorityTooOld
	}
	start := time.Now()
	status := "error"
	var originServerID string
	var summary map[string]any
	defer func() {
		_ = st.WriteAudit(store.AuditRow{
			TS: start, ProjectID: projectID, ServerID: originServerID,
			Action: "probe-connectivity", Status: status,
			Source:  store.AuditSourceAgent,
			Command: store.MarshalAuditSummary(summary),
		})
	}()

	instance := bridgeCacheInstance()
	service := &probe.Service{
		Store:      st,
		Instance:   instance,
		GateServer: func(serverID string) error { return gateServer(st, profileID, serverID) },
		HostKeys:   hk,
	}
	if results, ok, pathErr := probeobserve.ResultsFor(instance); pathErr == nil && ok {
		service.Results = results
	}
	report, probeErr := service.Probe(ctx, probe.Request{From: from, Targets: targets})
	summary, originServerID = probe.AuditSummary(from, report, probeErr)
	if probeErr != nil {
		return "", probeErr
	}
	status = "ok"
	return report.Text(), nil
}
