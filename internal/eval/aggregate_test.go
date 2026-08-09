package eval

import (
	"reflect"
	"strings"
	"testing"
)

// TestRunTaskM is a PURE UNIT TEST for runTaskM's aggregation logic. It injects
// a fake drive (hand-built *Transcript with a known Cost) and a fake score, so
// it runs ungated in the default fast-lane — no LLM, no docker, no requireEval.
// If the M-loop aggregation ever regresses (pass/fail tally, cost sum, or
// reasons accumulation), this fails before any expensive gated T2–T8 run.
//
// Fixture: M=3, two passes + one fail. Verifies:
//   - Pass/Fail tally correctly reflects the score outcomes.
//   - Cost is the sum of each driven transcript's Cost (0.25 + 0.50 + 0.25 = 1.00,
//     all exactly representable in float64 so there is no rounding noise).
//   - Reasons accumulate ONLY on the failing run, in order, and the passing
//     runs contribute nothing (score returns nil reasons on pass).
//   - drive is invoked exactly M times (idx lands at M).
func TestRunTaskM(t *testing.T) {
	transcripts := []*Transcript{
		{Cost: 0.25, Final: "pass-1"},
		{Cost: 0.50, Final: "fail-1"}, // scores fail
		{Cost: 0.25, Final: "pass-2"},
	}
	idx := 0
	drive := func() *Transcript {
		if idx >= len(transcripts) {
			t.Fatalf("drive called more than M=%d times", len(transcripts))
		}
		tr := transcripts[idx]
		idx++
		return tr
	}

	// score: pass when Final has the "pass" prefix; fail returns two reasons so
	// we also verify that ALL reasons from a single failing run are appended.
	score := func(tr *Transcript) (bool, []string) {
		if strings.HasPrefix(tr.Final, "pass") {
			return true, nil
		}
		return false, []string{"missing unit", "wrong answer: " + tr.Final}
	}

	r := runTaskM(t, "fake-task", 3, drive, score)

	if r.Task != "fake-task" {
		t.Errorf("Task = %q, want %q", r.Task, "fake-task")
	}
	if r.M != 3 {
		t.Errorf("M = %d, want 3", r.M)
	}
	if r.Pass != 2 {
		t.Errorf("Pass = %d, want 2 (2 passing runs)", r.Pass)
	}
	if r.Fail != 1 {
		t.Errorf("Fail = %d, want 1 (1 failing run)", r.Fail)
	}
	if r.Cost != 1.00 {
		t.Errorf("Cost = %v, want 1.00 (sum of 0.25+0.50+0.25)", r.Cost)
	}
	wantReasons := []string{"missing unit", "wrong answer: fail-1"}
	if !reflect.DeepEqual(r.Reasons, wantReasons) {
		t.Errorf("Reasons = %v, want %v (only the failing run's reasons, in order)", r.Reasons, wantReasons)
	}
	if idx != 3 {
		t.Errorf("drive invoked %d times, want exactly M=3", idx)
	}
}

// TestRunTaskMZeroM covers the M=0 edge: the loop body never runs, so drive and
// score are never invoked and the result is the zero value (with Task/M set).
// Guards against an off-by-one (`i <= M`) that would call drive once at M=0.
func TestRunTaskMZeroM(t *testing.T) {
	drive := func() *Transcript {
		t.Fatal("drive must not be called when M=0")
		return nil
	}
	score := func(*Transcript) (bool, []string) {
		t.Fatal("score must not be called when M=0")
		return false, nil
	}

	r := runTaskM(t, "zero-task", 0, drive, score)

	if r.Task != "zero-task" || r.M != 0 {
		t.Errorf("identity fields not set: got Task=%q M=%d", r.Task, r.M)
	}
	if r.Pass != 0 || r.Fail != 0 || r.Cost != 0 || len(r.Reasons) != 0 {
		t.Errorf("M=0 result should be zero-valued, got Pass=%d Fail=%d Cost=%v Reasons=%v",
			r.Pass, r.Fail, r.Cost, r.Reasons)
	}
}
