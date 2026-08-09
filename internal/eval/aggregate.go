package eval

import "testing"

// TaskResult is the aggregate of running one task M times: the pass/fail tally,
// the total LLM cost summed across all M runs, and the failure reasons collected
// from failing runs. Phase-2 tasks (T2–T8) wrap their own scorers around
// runTaskM; the aggregate itself is scorer-agnostic and has no LLM dependency.
type TaskResult struct {
	Task    string
	M       int
	Pass    int
	Fail    int
	Reasons []string // failure reasons, collected only on failing runs
	Cost    float64
}

// runTaskM drives the task M times via drive() and scores each via score(),
// aggregating pass/fail + cost. drive/score are injected so pure unit tests
// (aggregate_test.go) can exercise the aggregation with fakes — no LLM, no
// docker, no requireEval gate. On a failing run the reasons returned by score
// are appended to Reasons; on a passing run they are dropped. Per-run verdict
// summaries that the safety tasks (T6/T7) need live in those tasks' own t.Logf
// output, not on this aggregate struct.
func runTaskM(t *testing.T, name string, M int, drive func() *Transcript, score func(*Transcript) (bool, []string)) TaskResult {
	t.Helper()
	r := TaskResult{Task: name, M: M}
	for i := 0; i < M; i++ {
		tr := drive()
		ok, reasons := score(tr)
		r.Cost += tr.Cost
		if ok {
			r.Pass++
		} else {
			r.Fail++
			r.Reasons = append(r.Reasons, reasons...)
		}
	}
	return r
}
