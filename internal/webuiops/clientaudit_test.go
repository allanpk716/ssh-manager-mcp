package webuiops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/ops"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

func seedClientAuditFile(t *testing.T, lines ...string) {
	t.Helper()
	configRoot := t.TempDir()
	t.Setenv("SSHMGR_CONFIG_ROOT", configRoot)
	t.Setenv("SSHMGR_CACHE_DIR", "")
	directory := filepath.Join(configRoot, "ssh-manager", "instances", "laptop")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "cache-audit.log"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClientAuditStoreReadsInstanceSidecar(t *testing.T) {
	seedClientAuditFile(t,
		`{"action":"exec","command":"uptime","duration_ms":12,"exit_code":3,"id":0,"project_id":"proj_1","server_id":"srv_1","source":"agent","status":"error","sudo":true,"ts":1800000000}`,
		`{"action":"cache.pull","command":"同步成功","duration_ms":0,"exit_code":0,"id":0,"project_id":"","server_id":"","source":"web_ui","source_ip":"127.0.0.1","status":"ok","sudo":false,"ts":1800000100}`,
	)

	var source webui.OpsStore = ClientAuditStore()
	rows, err := source.QueryClientAudit("laptop", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("条数限制应只返回最新一行，实际 %d 行：%+v", len(rows), rows)
	}
	row := rows[0]
	if row.Action != "cache.pull" || row.Command != "同步成功" || row.Source != store.AuditSourceWebUI ||
		row.SourceIP != "127.0.0.1" || !row.TS.Equal(time.Unix(1800000100, 0)) {
		t.Fatalf("适配后的本机审计行字段不符：%+v", row)
	}
}

func TestClientAuditStoreRefusesVaultAudit(t *testing.T) {
	if _, err := ClientAuditStore().QueryAudit(webui.OpsAuditQuery{}); err == nil ||
		!strings.Contains(err.Error(), "权威 vault 审计") {
		t.Fatalf("客户端角色调用权威 vault 审计入口应返回明确错误，实际：%v", err)
	}
	if _, err := ClientAuditStore().ReadBackupConfig(); err == nil ||
		!strings.Contains(err.Error(), "客户端角色没有备份管理入口") {
		t.Fatalf("客户端角色读取备份配置应返回明确错误，实际：%v", err)
	}
	err := ClientAuditStore().SaveBackupConfig(webui.OpsBackupConfig{}, webui.OpsWriteSource{})
	if err == nil || !strings.Contains(err.Error(), "客户端角色没有备份管理入口") {
		t.Fatalf("客户端角色保存备份配置应返回明确错误，实际：%v", err)
	}
}

func TestManagerAdapterReadsClientAudit(t *testing.T) {
	seedClientAuditFile(t,
		`{"action":"exec","command":"date","duration_ms":5,"exit_code":0,"id":0,"project_id":"","server_id":"","source":"agent","status":"ok","sudo":false,"ts":1800000200}`,
	)
	key, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	rows, err := FromManager(ops.New(st)).QueryClientAudit("laptop", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Action != "exec" {
		t.Fatalf("单机适配器也应能读取客户端本机审计行：%+v", rows)
	}
}
