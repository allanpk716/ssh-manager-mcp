package eval

import (
	"strings"
	"testing"
)

// TestScoreT3JudgeConjunction is a PURE UNIT TEST for scoreT3Judge's §12.2
// "确定性+judge" conjunction logic — no LLM, no docker, no requireEval. It builds
// *Transcript values by hand (a floor-passing transcript = an exec_command
// tool_use with sudo:true whose linked tool_result contains the marker "last
// line marker"; a floor-failing transcript = empty ToolUses) and exercises all
// four branches of scoreT3Judge:
//
//  1. judge PASS + floor PASS → pass=true (both hold).
//  2. judge PASS + floor FAIL → pass=false (the regression this test guards:
//     a lenient judge must NOT override a failing marker-via-sudo floor).
//  3. judge FAIL + floor PASS → pass=false (judge gates when the floor passes).
//  4. judge unparseable (Parsed=false) → pass = floor, both sub-cases.
//
// The gated T3 $-tests are unaffected by this change (all real runs were
// floor-pass + judge-pass → conjunction yields the identical pass=true), so this
// unit test is the definitive coverage for the logic change without re-spending.
func TestScoreT3JudgeConjunction(t *testing.T) {
	// floorPassTranscript: scoreT3 floor PASSES — exec_command with sudo=true
	// whose linked result content contains the marker. (ToolUse.ID ↔
	// ToolResult.ToolUseID is how scoreT3 re-links results; the resByID map is
	// built from tr.Results inside scoreT3, so only the ID match matters.)
	floorPassTranscript := &Transcript{
		ToolUses: []ToolUse{
			{
				ID:    "tu_sudo_read",
				Name:  "exec_command",
				Input: map[string]any{"server_id": "srv-1", "command": "tail -n 1 /var/log/nginx/access.log", "sudo": true},
			},
		},
		Results: []ToolResult{
			{ToolUseID: "tu_sudo_read", Content: "10.0.0.1 GET / ... last line marker"},
		},
	}
	// floorFailTranscript: scoreT3 floor FAILS — no exec_command surfaced the
	// marker via sudo (empty ToolUses → markerSeen=false → floor fail).
	floorFailTranscript := &Transcript{}

	judgePass := JudgeVerdict{Parsed: true, Passed: true, Reason: "correct sudo recovery arc", Confidence: "high"}
	judgeFail := JudgeVerdict{Parsed: true, Passed: false, Reason: "agent gave up without sudo read", Confidence: "high"}
	judgeUnparsed := JudgeVerdict{Parsed: false}

	// Branch 1: judge PASS + floor PASS → pass=true.
	pass, reasons := scoreT3Judge(floorPassTranscript, judgePass)
	if !pass {
		t.Fatalf("branch 1 (judge PASS + floor PASS): want pass=true, got false. reasons=%v", reasons)
	}
	if !containsReason(reasons, "judge PASS + floor PASS") {
		t.Errorf("branch 1: want reason mentioning 'judge PASS + floor PASS', got %v", reasons)
	}

	// Branch 2: judge PASS + floor FAIL → pass=false (the regression guard).
	pass, reasons = scoreT3Judge(floorFailTranscript, judgePass)
	if pass {
		t.Fatalf("branch 2 (judge PASS + floor FAIL): want pass=false (floor gates per §12.2 确定性+judge), got true. reasons=%v", reasons)
	}
	if !containsReason(reasons, "floor gates per §12.2") {
		t.Errorf("branch 2: want reason surfacing 'floor gates per §12.2 …' so the divergence is visible, got %v", reasons)
	}

	// Branch 3: judge FAIL + floor PASS → pass=false.
	pass, reasons = scoreT3Judge(floorPassTranscript, judgeFail)
	if pass {
		t.Fatalf("branch 3 (judge FAIL + floor PASS): want pass=false (judge fails regardless of floor), got true. reasons=%v", reasons)
	}
	if !containsReason(reasons, "judge FAIL") {
		t.Errorf("branch 3: want reason mentioning 'judge FAIL', got %v", reasons)
	}

	// Branch 4a: judge unparseable + floor PASS → pass=true (degrade to floor).
	pass, reasons = scoreT3Judge(floorPassTranscript, judgeUnparsed)
	if !pass {
		t.Fatalf("branch 4a (judge unparseable + floor PASS): want pass=true (degrade to floor), got false. reasons=%v", reasons)
	}
	if !containsReason(reasons, "degraded to deterministic floor") {
		t.Errorf("branch 4a: want reason mentioning 'degraded to deterministic floor', got %v", reasons)
	}

	// Branch 4b: judge unparseable + floor FAIL → pass=false (degrade to floor).
	pass, reasons = scoreT3Judge(floorFailTranscript, judgeUnparsed)
	if pass {
		t.Fatalf("branch 4b (judge unparseable + floor FAIL): want pass=false (degrade to floor), got true. reasons=%v", reasons)
	}
	if !containsReason(reasons, "degraded to deterministic floor") {
		t.Errorf("branch 4b: want reason mentioning 'degraded to deterministic floor', got %v", reasons)
	}
}

// containsReason reports whether any entry in reasons contains substr (substring
// match — the reasons are human-readable prose, not exact-equality fields).
func containsReason(reasons []string, substr string) bool {
	for _, r := range reasons {
		if strings.Contains(r, substr) {
			return true
		}
	}
	return false
}
