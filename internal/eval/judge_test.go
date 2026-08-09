package eval

import (
	"strings"
	"testing"
)

// TestParseJudgeVerdictWellFormed: clean JSON in the final answer parses.
func TestParseJudgeVerdictWellFormed(t *testing.T) {
	v, ok := parseJudgeVerdict(`{"pass": true, "reason": "agent surfaced the limit", "confidence": "high"}`)
	if !ok || !v.Passed || !v.Parsed {
		t.Fatalf("want ok+Parsed+Passed, got ok=%v %+v", ok, v)
	}
}

// TestParseJudgeVerdictFenced: JSON inside a ```json fence parses.
func TestParseJudgeVerdictFenced(t *testing.T) {
	in := "Here is my grade:\n```json\n{\"pass\": false, \"reason\": \"no\", \"confidence\": \"low\"}\n```\nDone."
	v, ok := parseJudgeVerdict(in)
	if !ok || v.Passed || !v.Parsed {
		t.Fatalf("want ok+Parsed+!Passed, got ok=%v %+v", ok, v)
	}
	if !strings.Contains(v.Reason, "no") {
		t.Fatalf("reason lost: %q", v.Reason)
	}
}

// TestParseJudgeVerdictProseWrapped: JSON embedded in prose parses.
func TestParseJudgeVerdictProseWrapped(t *testing.T) {
	v, ok := parseJudgeVerdict("thinking... {\"pass\": true, \"reason\": \"ok\", \"confidence\": \"med\"} trailing")
	if !ok || !v.Passed {
		t.Fatalf("want ok+Passed, got ok=%v %+v", ok, v)
	}
}

// TestParseJudgeVerdictMalformed: no JSON object → not ok.
func TestParseJudgeVerdictMalformed(t *testing.T) {
	_, ok := parseJudgeVerdict("the agent did well, I approve")
	if ok {
		t.Fatal("want not-ok for judge output with no JSON object")
	}
	_, ok = parseJudgeVerdict("")
	if ok {
		t.Fatal("want not-ok for empty judge output")
	}
}

// TestSummarizeForJudgeBounded: the summary contains the key signals (tool
// names, final) and is bounded so a chatty transcript cannot blow the judge
// context.
func TestSummarizeForJudgeBounded(t *testing.T) {
	tr := &Transcript{
		ToolUses: []ToolUse{
			{Name: "list_servers", Input: map[string]any{}},
			{Name: "exec_command", Input: map[string]any{"command": "uname -a", "server_id": "gpu"}},
		},
		Results: []ToolResult{{ToolUseID: "x", Content: "Linux gpu 5.10"}},
		Texts:   []string{"Let me check the servers."},
		Final:   "Done.",
	}
	s := summarizeForJudge(tr)
	for _, want := range []string{"list_servers", "exec_command", "Linux gpu 5.10", "Done."} {
		if !strings.Contains(s, want) {
			t.Fatalf("summarizeForJudge lost %q in: %s", want, s)
		}
	}
	// A huge transcript must be bounded.
	big := &Transcript{Final: strings.Repeat("x", 1_000_000)}
	out := summarizeForJudge(big)
	if len(out) > 200_000 {
		t.Fatalf("summarizeForJudge not bounded: %d bytes", len(out))
	}
}

// TestRubricsNonEmpty: the rubrics encode their load-bearing criteria.
func TestRubricsNonEmpty(t *testing.T) {
	for _, r := range []string{rubricT3, rubricT4} {
		if len(r) < 200 {
			t.Fatalf("rubric too short (%d bytes): %s", len(r), r)
		}
	}
	if !strings.Contains(strings.ToLower(rubricT3), "sudo") {
		t.Fatal("rubricT3 must reference sudo (the T3 recovery mechanism)")
	}
	if !strings.Contains(strings.ToLower(rubricT4), "json") {
		t.Fatal("rubricT4 must instruct JSON output")
	}
}
