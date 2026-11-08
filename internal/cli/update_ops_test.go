package cli

import (
	"strings"
	"testing"

	"ssh-manager-mcp/internal/store"
)

// TestUpdateWritesAuditRow 验证命令行升级成功后写入 update.apply 审计行：
// 动作、来源、旧版本、新版本与结果都在摘要里，来源为命令行。
func TestUpdateWritesAuditRow(t *testing.T) {
	dbPath, mk := withCliStoreEnv(t)
	_, self, _ := writeSelfFixture(t)
	startFakeUpdateSource(t, "v0.2.0", testBinBytes(t))
	seamUpdateDefaults(t, self, "0.1.0")
	withStagedVersion(t, "0.2.0")

	out, err := runUpdate(t, "--yes")
	if err != nil {
		t.Fatalf("update --yes: %v\n%s", err, out)
	}

	st, err := store.Open(dbPath, mk)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rows, err := st.AuditRows(5)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("应写一条升级审计，实际 %d 条", len(rows))
	}
	row := rows[0]
	if row.Action != "update.apply" || row.Source != store.AuditSourceCLI || row.SourceIP != "" ||
		row.Status != "ok" || row.ProjectID != "" || row.ServerID != "" {
		t.Fatalf("审计行字段不符：%+v", row)
	}
	for _, want := range []string{`"old_version":"0.1.0"`, `"new_version":"v0.2.0"`, `"result":"ok"`} {
		if !strings.Contains(row.Command, want) {
			t.Fatalf("审计摘要缺少 %s：%s", want, row.Command)
		}
	}
}
