package tui

import (
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// Plan 28 T3: the TUI's two metadata-carrying save paths — the servers-page
// add/edit form (submitServer) and the import flow's supplement submit
// (importFlow.submitSupplement) — surface advisory, non-blocking
// suspected-secret hints for the free-text metadata they persist (a pasted
// PEM key or API token in a note would ride into every list_servers LLM
// context verbatim). hintLines is the pure seam both call: unit-tested here
// without driving the bubbletea loop, then pinned at each save point by
// direct invocation (the same direct-call shape the neighboring form/import
// tests use).

// TestHintLinesHitAndNoEcho: hits return one ⚠ line per finding naming the
// field and the rule — and never any of the suspected value itself.
func TestHintLinesHitAndNoEcho(t *testing.T) {
	const tagSentinel = "SENTINEL-TUI-GHP-2MR"
	const pemSentinel = "SENTINEL-TUI-PEM-8QD"

	lines := hintLines(
		`["gpu","ghp_`+tagSentinel+`fake"]`, // tags — raw JSON form, as persisted
		"", "", "", "",
		"prod box",
		"-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEA"+pemSentinel+"\n-----END OPENSSH PRIVATE KEY-----",
	)
	if len(lines) != 2 {
		t.Fatalf("want 2 hint lines (tags prefix + caveats pem), got %d: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "field 'tags'") || !strings.Contains(lines[0], "prefix:ghp_") {
		t.Fatalf("tags line must name field+rule: %q", lines[0])
	}
	if !strings.Contains(lines[1], "field 'caveats'") || !strings.Contains(lines[1], "pem-private-key") {
		t.Fatalf("caveats line must name field+rule: %q", lines[1])
	}
	for _, l := range lines {
		if strings.Contains(l, tagSentinel) || strings.Contains(l, pemSentinel) {
			t.Fatalf("hint must never echo the suspected value: %q", l)
		}
	}
}

// TestHintLinesClean: legal metadata across all seven free-text fields
// produces zero lines — the hint is advisory-only and silent on clean input.
func TestHintLinesClean(t *testing.T) {
	lines := hintLines(
		tagsScanValue([]string{"prod", "gpu"}),
		"ml training box", "dc1 rack 4", "64c/512G/8xA100",
		"postgres primary", "prod pg primary", "do not reboot during business hours",
	)
	if len(lines) != 0 {
		t.Fatalf("clean metadata must produce zero lines, got: %v", lines)
	}
}

// TestSubmitServerWarnsOnSuspectedSecret: the add-form save path appends the
// ⚠ hint lines to its actionDoneMsg status (advisory only — the server still
// persists) without echoing the suspected value.
func TestSubmitServerWarnsOnSuspectedSecret(t *testing.T) {
	st := newStore(t)
	const sentinel = "SENTINEL-TUI-ADD-4HN"
	d := &serverDraft{Name: "leaky", Host: "h", User: "u", Port: 22,
		Description: "note: token AKIA" + sentinel + "fake"}

	msg := submitServer(st, nil, d)()
	done, ok := msg.(actionDoneMsg)
	if !ok {
		t.Fatalf("add must succeed (advisory only), got %#v", msg)
	}
	if !strings.Contains(done.desc, "已新增 leaky") {
		t.Fatalf("status must keep the success line: %q", done.desc)
	}
	if !strings.Contains(done.desc, "field 'description'") || !strings.Contains(done.desc, "prefix:AKIA") {
		t.Fatalf("status must carry the field+rule hint: %q", done.desc)
	}
	if strings.Contains(done.desc, sentinel) {
		t.Fatalf("hint must never echo the suspected value: %q", done.desc)
	}
	if srv, _ := st.GetServerByName("leaky"); srv == nil || srv.Description != d.Description {
		t.Fatalf("server must persist despite the hint: %+v", srv)
	}
}

// TestSubmitServerEditWarnsOnSuspectedSecret: the edit leg shares the same
// return wiring — a secret-shaped caveats edit reaches the status line while
// the update succeeds.
func TestSubmitServerEditWarnsOnSuspectedSecret(t *testing.T) {
	st := newStore(t)
	if _, err := st.AddServer(&models.Server{Name: "t", Host: "h", User: "u", Port: 22}); err != nil {
		t.Fatal(err)
	}
	cur, _ := st.GetServerByName("t")
	const sentinel = "SENTINEL-TUI-EDIT-6KP"
	d := prefill(cur)
	d.Caveats = "key: xoxb-" + sentinel + "-fake"

	msg := submitServer(st, cur, d)()
	done, ok := msg.(actionDoneMsg)
	if !ok {
		t.Fatalf("edit must succeed, got %#v", msg)
	}
	if !strings.Contains(done.desc, "已更新 t") {
		t.Fatalf("status must keep the success line: %q", done.desc)
	}
	if !strings.Contains(done.desc, "field 'caveats'") || !strings.Contains(done.desc, "prefix:xoxb-") {
		t.Fatalf("status must carry the field+rule hint: %q", done.desc)
	}
	if strings.Contains(done.desc, sentinel) {
		t.Fatalf("hint must never echo the suspected value: %q", done.desc)
	}
	if srv, _ := st.GetServerByName("t"); srv == nil || !strings.Contains(srv.Caveats, sentinel) {
		t.Fatalf("edit must persist despite the hint: %+v", srv)
	}
}

// TestImportFlowSupplementWarnsOnSuspectedSecret: the import supplement save
// path appends ⚠ lines to the flow's result report (the existing feedback
// surface — no blocking, no new confirm screen), the loop still advances to
// the result, and the supplement still persists.
func TestImportFlowSupplementWarnsOnSuspectedSecret(t *testing.T) {
	st := newStore(t)
	f := flowAtPick(t, st, writeImportConfig(t, ""))
	f.pick = []string{"bare"}
	runBatch(t, f)
	supplementTarget(t, f, "bare")

	const sentinel = "SENTINEL-TUI-SUPP-9XT"
	f.d.Caveats = "-----BEGIN OPENSSH PRIVATE KEY-----\n" + sentinel + "\n-----END OPENSSH PRIVATE KEY-----"
	f.submitSupplement()

	if f.state != stateResult {
		t.Fatalf("supplement must advance past the save (non-blocking), state=%v", f.state)
	}
	hintLine := ""
	for _, l := range f.report {
		if strings.Contains(l, "pem-private-key") {
			hintLine = l
		}
	}
	if hintLine == "" || !strings.Contains(hintLine, "bare") ||
		!strings.Contains(hintLine, "field 'caveats'") {
		t.Fatalf("report must carry a bare/caveats hint line, got: %v", f.report)
	}
	if strings.Contains(strings.Join(f.report, "\n"), sentinel) {
		t.Fatalf("hint must never echo the suspected value: %v", f.report)
	}
	bare, _ := st.GetServerByName("bare")
	if bare == nil || !strings.Contains(bare.Caveats, sentinel) {
		t.Fatalf("supplement must persist despite the hint: %+v", bare)
	}
}
