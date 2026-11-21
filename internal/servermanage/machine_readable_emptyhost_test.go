package servermanage

import (
	"encoding/json"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// auditSummaryJSON 解析一条审计行的 JSON 摘要。
func auditSummaryJSON(t *testing.T, command string) map[string]any {
	t.Helper()
	summary := map[string]any{}
	if err := json.Unmarshal([]byte(command), &summary); err != nil {
		t.Fatalf("审计摘要应为 JSON：%q (%v)", command, err)
	}
	return summary
}

// TestDeleteAuditSummaryEmptyHostKeepsEmptyHostString：删除空主机条目时
// server.delete 审计的 JSON 摘要 host 保持空串，机器可读接口不改语义。
func TestDeleteAuditSummaryEmptyHostKeepsEmptyHostString(t *testing.T) {
	m := newTestManager(t)
	preview := seedEmptyHostForDelete(t, m)
	if _, err := m.Delete("裸条目", preview.Digest, CLISource()); err != nil {
		t.Fatal(err)
	}
	rows, err := m.st.QueryAudit(store.AuditFilter{Actions: []string{"server.delete"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("应写一条 server.delete 审计，实际 %d", len(rows))
	}
	if summary := auditSummaryJSON(t, rows[0].Command); summary["host"] != "" {
		t.Fatalf("空主机条目的删除摘要 host 应为空串，实际 %v", summary["host"])
	}
}

// TestAddAuditSummaryEmptyHostKeepsEmptyHostString：新增条目的 JSON 摘要
// 照抄条目原始主机；主机为空就是空串，不推导、不补默认值（机器可读接口
// 不改语义）。正常新增路径会先推导兜底主机，这里直接锁定摘要函数本身。
func TestAddAuditSummaryEmptyHostKeepsEmptyHostString(t *testing.T) {
	empty := addAuditSummary(AddRequest{Server: models.Server{Name: "裸条目", Host: "", Port: 22, User: "ops"}})
	if empty["host"] != "" {
		t.Fatalf("空主机条目的新增摘要 host 应为空串，实际 %v", empty["host"])
	}
	if empty["name"] != "裸条目" || empty["user"] != "ops" || empty["port"] != 22 {
		t.Fatalf("摘要的其余字段应保持不变：%v", empty)
	}
	hosted := addAuditSummary(AddRequest{Server: models.Server{Name: "gpu", Host: "192.0.2.10", Port: 22, User: "ops"}})
	if hosted["host"] != "192.0.2.10" {
		t.Fatalf("主机非空条目的摘要应照旧：%v", hosted)
	}
}

// seedEmptyHostForDelete 建一台空主机条目并返回删除预览。
func seedEmptyHostForDelete(t *testing.T, m *Manager) *DeletePreview {
	t.Helper()
	if _, err := m.st.AddServerWithCredentials(&models.Server{
		Name: "裸条目", Host: "", Port: 22, User: "u", AuthMethod: models.AuthPassword,
	}, &models.Credential{Type: models.CredPassword, Secret: []byte("pw")}, nil); err != nil {
		t.Fatal(err)
	}
	preview, err := m.DeletePreview("裸条目")
	if err != nil {
		t.Fatal(err)
	}
	return preview
}
