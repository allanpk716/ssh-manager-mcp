package tui

import (
	"strings"
	"testing"
)

// TestTokenIssuedMsgBody_ZeroFields: usage/recovery/snippet 全零值时 body()
// 输出 = 现状裸 token（设备码发射点 / 向导发射点的零值兼容锚）。
func TestTokenIssuedMsgBody_ZeroFields(t *testing.T) {
	m := tokenIssuedMsg{title: "设备码 — laptop", token: "DEV-x"}
	if got := m.body(); got != "DEV-x" {
		t.Fatalf("zero-field body must be the bare token, got %q", got)
	}
}

// TestTokenIssuedMsgBody_BlockOrder: 三字段齐备时按固定顺序分块
// token → 用途 → 丢失 → 片段；snippet 非空时含引导语首行与说明块（归属锚）。
func TestTokenIssuedMsgBody_BlockOrder(t *testing.T) {
	m := tokenIssuedMsg{
		title:    "项目 token",
		token:    "TOK-1",
		usage:    "填进 agent 的 .mcp.json",
		recovery: "Projects 页 [e] 轮换换发",
		snippet:  mcpConfigLines([]string{`"args": ["mcp"]`, stdioEnvLine("TOK-1")}, []string{"note-a"}),
	}
	b := m.body()
	iTok := strings.Index(b, "TOK-1")
	iUse := strings.Index(b, "用途：填进")
	iLost := strings.Index(b, "⚠ 仅此一次。丢失 → ")
	iSnip := strings.Index(b, "把下面的片段写进 agent 项目的 .mcp.json：")
	if !(iTok < iUse && iUse < iLost && iLost < iSnip) {
		t.Fatalf("block order must be token→用途→丢失→片段:\n%s", b)
	}
	for _, want := range []string{"说明：", "- note-a", `"SSHMGR_TOKEN": "TOK-1"`} {
		if !strings.Contains(b, want) {
			t.Fatalf("snippet block missing %q:\n%s", want, b)
		}
	}
}
