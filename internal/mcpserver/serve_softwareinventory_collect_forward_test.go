package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/softwareinventory"
	"ssh-manager-mcp/internal/store"
)

// TestSoftwareInventoryImmediateCollectionForwardEndpoint 覆盖客户端实例转
// 发的立即采集：只允许设备码可见设备，同步采集结果与本地工具一致，审计
// 来源为受审计转发写并记录设备码编号。
func TestSoftwareInventoryImmediateCollectionForwardEndpoint(t *testing.T) {
	fx := newSoftwareInventoryRunner(t)
	gpu, err := fx.st.GetServerByName("gpu")
	if err != nil || gpu == nil {
		t.Fatalf("取 gpu 条目失败：%v %+v", err, gpu)
	}
	previous := softwareInventoryCollectService
	softwareInventoryCollectService = func(st *store.Store) *softwareinventory.Service {
		return &softwareinventory.Service{Store: st, RemoteExec: func(
			_ context.Context, server *models.Server, _ models.ShellType, _ string, _ bool,
		) (softwareinventory.RemoteExecResult, error) {
			if server.ID != gpu.ID {
				t.Fatalf("立即采集使用了错误的服务器条目：%+v", server)
			}
			return softwareinventory.RemoteExecResult{
				Stdout: softwareInventoryTestFixture(t),
			}, nil
		}}
	}
	t.Cleanup(func() { softwareInventoryCollectService = previous })

	status, body := postCollectorEndpoint(
		t, fx.srv, fx.laptopToken, "/software-inventory/collect", `{"device":"mirror-设备"}`,
	)
	if status != http.StatusForbidden || !strings.Contains(body, "not visible") {
		t.Fatalf("范围外设备应返回 403：%d %s", status, body)
	}

	status, body = postCollectorEndpoint(
		t, fx.srv, fx.laptopToken, "/software-inventory/collect", `{"device":"gpu-设备"}`,
	)
	if status != http.StatusOK {
		t.Fatalf("可见设备立即采集状态码 = %d：%s", status, body)
	}
	var response struct {
		DeviceName string                              `json:"device_name"`
		Report     *softwareinventory.CollectionReport `json:"report"`
	}
	if err := json.Unmarshal([]byte(body), &response); err != nil {
		t.Fatal(err)
	}
	if response.DeviceName != "gpu-设备" || response.Report == nil ||
		response.Report.CommandSetVersion != softwareinventory.POSIXCommandSetVersion {
		t.Fatalf("立即采集响应 = %s", body)
	}
	audit, err := fx.st.QueryAudit(store.AuditFilter{Actions: []string{"software-collect"}})
	if err != nil || len(audit) == 0 {
		t.Fatalf("立即采集应写审计：%v %+v", err, audit)
	}
	if audit[0].Source != store.AuditSourceForward {
		t.Fatalf("立即采集审计来源 = %q", audit[0].Source)
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(audit[0].Command), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["device_code"] != fx.laptopID || summary["device"] != "gpu-设备" {
		t.Fatalf("立即采集审计摘要 = %v", summary)
	}
}
func softwareInventoryTestFixture(t *testing.T) string {
	t.Helper()
	output, err := os.ReadFile("../softwareinventory/testdata/posix/ubuntu2404-full.txt")
	if err != nil {
		t.Fatal(err)
	}
	return string(output)
}
