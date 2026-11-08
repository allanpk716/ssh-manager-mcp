package probe

import (
	"errors"
	"testing"
)

// TestAuditSummaryFields: MCP 工具与命令行共用同一份审计摘要：经服务器
// 探测注明固定命令并归属起点服务器编号；失败路径写错误原因；报告缺失时
// 只写起点与空目标列表。
func TestAuditSummaryFields(t *testing.T) {
	local := &Report{From: "self", FromDevice: "笔记本", Targets: []TargetReport{
		{Label: "NAS", Port: 445},
	}}
	summary, serverID := AuditSummary("self", local, nil)
	if serverID != "" {
		t.Fatalf("local summary server id = %q, want empty", serverID)
	}
	if summary["from"] != "self" || summary["from_device"] != "笔记本" {
		t.Fatalf("local summary = %+v", summary)
	}
	targets, ok := summary["targets"].([]string)
	if !ok || len(targets) != 1 || targets[0] != "NAS:445" {
		t.Fatalf("local summary targets = %#v", summary["targets"])
	}
	if _, present := summary["note"]; present {
		t.Fatalf("local summary must not carry the fixed-command note: %+v", summary)
	}

	remote := &Report{From: "srv-1", Remote: true, Targets: []TargetReport{{Label: "NAS", Port: 22}}}
	summary, serverID = AuditSummary("srv-1", remote, nil)
	if serverID != "srv-1" || summary["note"] != "固定命令，不经命令策略" {
		t.Fatalf("remote summary = %+v server %q", summary, serverID)
	}

	summary, serverID = AuditSummary("self", nil, errors.New("目标不能为空"))
	if serverID != "" || summary["error"] != "目标不能为空" {
		t.Fatalf("failure summary = %+v server %q", summary, serverID)
	}
	if targets, ok := summary["targets"].([]string); !ok || len(targets) != 0 {
		t.Fatalf("failure summary targets = %#v", summary["targets"])
	}
}
