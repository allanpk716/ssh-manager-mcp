package cli

import (
	"testing"

	"ssh-manager-mcp/internal/models"
)

// TestRmSummaryEmptyHostKeepsEmptyHostString：server.rm 的 JSON 白名单摘
// 要对空主机条目保持 host 为空串（机器可读接口不改语义）。
func TestRmSummaryEmptyHostKeepsEmptyHostString(t *testing.T) {
	summary := rmSummary(&models.Server{Name: "bare", Host: "", Port: 22, User: "ops"})
	if summary["host"] != "" {
		t.Fatalf("空主机条目的 rm 摘要 host 应为空串，实际 %v", summary["host"])
	}
	if summary["name"] != "bare" || summary["port"] != 22 {
		t.Fatalf("摘要的其余字段应保持不变：%v", summary)
	}
	hosted := rmSummary(&models.Server{Name: "gpu", Host: "192.0.2.10", Port: 22, User: "ops"})
	if hosted["host"] != "192.0.2.10" {
		t.Fatalf("主机非空条目的摘要应照旧：%v", hosted)
	}
}
