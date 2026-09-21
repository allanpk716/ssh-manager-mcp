package store

import (
	"encoding/json"
	"testing"
	"time"
)

// TestAuditOwnerRowFields 钉住 owner 身份审计行的构造形态:ProjectID 留空
// (owner 操作不挂任何项目)、Status 为 ok、TS 为当前时间、Command 为摘要
// 序列化出的 JSON 字符串。摘要里只有调用方传入的字段——辅助函数不补任何
// 调用方没给的字段(宁缺勿泄)。
func TestAuditOwnerRowFields(t *testing.T) {
	before := time.Now()
	row := OwnerAuditRow("cache-token.revoke", map[string]any{"name": "gpu1"})
	after := time.Now()

	if row.ProjectID != "" {
		t.Fatalf("ProjectID = %q, want empty string (owner action)", row.ProjectID)
	}
	if row.Action != "cache-token.revoke" {
		t.Fatalf("Action = %q, want passthrough of caller's action", row.Action)
	}
	if row.Status != "ok" {
		t.Fatalf("Status = %q, want \"ok\"", row.Status)
	}
	if row.TS.Before(before) || row.TS.After(after) {
		t.Fatalf("TS = %v, want a time between %v and %v", row.TS, before, after)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(row.Command), &got); err != nil {
		t.Fatalf("Command is not valid JSON: %v (Command=%q)", err, row.Command)
	}
	if len(got) != 1 || got["name"] != "gpu1" {
		t.Fatalf("Command summary = %v, want exactly {\"name\":\"gpu1\"}", got)
	}
}

// TestAuditOwnerRowEmptySummary:调用方没传任何摘要字段时,Command 为 "{}",
// 且行内不出现任何其他字段。
func TestAuditOwnerRowEmptySummary(t *testing.T) {
	row := OwnerAuditRow("server.add", nil)
	if row.Command != "{}" {
		t.Fatalf("Command = %q, want \"{}\" for nil summary", row.Command)
	}
}

// TestAuditOwnerMarshalSummary 钉住摘要序列化:nil 与空 map 都返回 "{}";正常
// map(字符串/整数/字符串切片)产出合法 JSON 且键序稳定(json.Marshal 对 map
// 按键排序);含无法序列化的值时返回 "{}" 而不是报错。
func TestAuditOwnerMarshalSummary(t *testing.T) {
	if got := MarshalAuditSummary(nil); got != "{}" {
		t.Fatalf("MarshalAuditSummary(nil) = %q, want \"{}\"", got)
	}
	if got := MarshalAuditSummary(map[string]any{}); got != "{}" {
		t.Fatalf("MarshalAuditSummary(empty) = %q, want \"{}\"", got)
	}

	// 插入顺序故意倒着放(zebra 在前),输出必须按 key 排序。
	got := MarshalAuditSummary(map[string]any{
		"zebra":   "z",
		"alpha":   2,
		"changed": []string{"host", "user"},
	})
	want := `{"alpha":2,"changed":["host","user"],"zebra":"z"}`
	if got != want {
		t.Fatalf("MarshalAuditSummary = %s, want %s", got, want)
	}
	var back map[string]any
	if err := json.Unmarshal([]byte(got), &back); err != nil {
		t.Fatalf("output is not valid JSON: %v (output=%s)", err, got)
	}

	// 不可序列化的值(通道)→ "{}",绝不把错误抛给调用方。
	if got := MarshalAuditSummary(map[string]any{"bad": make(chan int)}); got != "{}" {
		t.Fatalf("MarshalAuditSummary(unmarshalable) = %q, want \"{}\"", got)
	}
}

// TestAuditOwnerWriteRoundTrip:经便捷写入函数落库后,行可经既有 AuditRows
// 查询读回,且 OwnerOnly 过滤(空 project_id)能命中它。
func TestAuditOwnerWriteRoundTrip(t *testing.T) {
	s := newTestStore(t)
	err := s.WriteOwnerAudit("server.rm", map[string]any{
		"name": "edge1", "host": "192.0.2.10", "port": 22,
	})
	if err != nil {
		t.Fatal(err)
	}

	rows, err := s.AuditRows(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("AuditRows(1) returned %d rows, want 1", len(rows))
	}
	got := rows[0]
	if got.Action != "server.rm" || got.Status != "ok" || got.ProjectID != "" {
		t.Fatalf("row = %+v, want Action=server.rm Status=ok ProjectID=\"\"", got)
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(got.Command), &summary); err != nil {
		t.Fatalf("Command is not valid JSON: %v (Command=%q)", err, got.Command)
	}
	if summary["name"] != "edge1" || summary["host"] != "192.0.2.10" {
		t.Fatalf("summary = %v, want name=edge1 host=192.0.2.10", summary)
	}
	if port, ok := summary["port"].(float64); !ok || int(port) != 22 {
		t.Fatalf("summary[\"port\"] = %v, want 22", summary["port"])
	}

	ownerRows, err := s.QueryAudit(AuditFilter{OwnerOnly: true, Actions: []string{"server.rm"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(ownerRows) != 1 || ownerRows[0].Action != "server.rm" {
		t.Fatalf("OwnerOnly query returned %d rows, want the one owner row", len(ownerRows))
	}
}
