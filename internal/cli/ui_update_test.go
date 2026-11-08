package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/ops"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

func TestUIUpdateAuditForStandaloneWritesVaultRow(t *testing.T) {
	dbPath, mk := withCliStoreEnv(t)

	source := ops.WebUISource("127.0.0.1")
	entry := ops.UpdateAuditEntry{
		Action: "update.apply", OldVersion: "0.1.0", NewVersion: "v0.2.0", Result: "ok",
	}
	if err := uiUpdateAudit(webui.RoleStandalone, "")(source, entry); err != nil {
		t.Fatalf("写升级审计失败：%v", err)
	}

	st, err := store.Open(dbPath, mk)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rows, err := st.AuditRows(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("应写一条审计，实际 %d 条", len(rows))
	}
	row := rows[0]
	if row.Action != "update.apply" || row.Source != store.AuditSourceWebUI ||
		row.SourceIP != "127.0.0.1" || row.Status != "ok" {
		t.Fatalf("单机升级审计行不符：%+v", row)
	}
	for _, want := range []string{`"old_version":"0.1.0"`, `"new_version":"v0.2.0"`, `"result":"ok"`} {
		if !strings.Contains(row.Command, want) {
			t.Fatalf("摘要缺少 %s：%s", want, row.Command)
		}
	}
}

func TestUIUpdateAuditForClientAppendsInstanceSidecar(t *testing.T) {
	userDir := t.TempDir()
	t.Setenv("SSHMGR_CONFIG_ROOT", userDir)
	t.Setenv("SSHMGR_CACHE_DIR", "")
	t.Setenv("SSHMGR_CACHE_DEK", "")

	source := ops.WebUISource("127.0.0.1")
	entry := ops.UpdateAuditEntry{
		Action: "update.apply", OldVersion: "0.1.0", NewVersion: "v0.2.0",
		Result: "error", Detail: "校验",
	}
	if err := uiUpdateAudit(webui.RoleClient, "laptop")(source, entry); err != nil {
		t.Fatalf("写客户端升级审计失败：%v", err)
	}

	_, _, _, auditPath, err := clientops.CachePathsFor("laptop")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err := json.Unmarshal(data, &row); err != nil {
		t.Fatalf("实例审计日志应是 JSON 每行一条：%s", data)
	}
	if row["action"] != "update.apply" || row["source"] != store.AuditSourceWebUI ||
		row["source_ip"] != "127.0.0.1" || row["status"] != "error" {
		t.Fatalf("客户端升级审计行不符：%s", data)
	}
	command, _ := row["command"].(string)
	for _, want := range []string{`"old_version":"0.1.0"`, `"new_version":"v0.2.0"`, `"result":"error"`} {
		if !strings.Contains(command, want) {
			t.Fatalf("客户端升级审计摘要缺少 %s：%s", want, data)
		}
	}
}
