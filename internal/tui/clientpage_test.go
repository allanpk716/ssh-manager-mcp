package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/store"
)

func TestClientHeader(t *testing.T) {
	h := clientHeader(&clientops.CacheCred{URL: "https://192.0.2.5:7878", Pin: "sha256:" + strings.Repeat("a", 64)}, 3, 2*time.Minute)
	for _, want := range []string{"192.0.2.5", "sha256", "3 服务器", "2m"} {
		if !strings.Contains(h, want) {
			t.Fatalf("header missing %q:\n%s", want, h)
		}
	}
}

func TestClientServerList(t *testing.T) {
	snap := &store.Snapshot{Servers: []store.SnapshotServer{{Name: "gpu", Host: "192.0.2.10", User: "u"}}}
	rows := clientServerRows(snap)
	if len(rows) != 1 || !strings.Contains(rows[0], "gpu") || !strings.Contains(rows[0], "192.0.2.10") {
		t.Fatalf("rows: %v", rows)
	}
}

// TestSyncCmdRefusesEmptyPin pins the TUI's own no-plaintext invariant: with a
// stored cred lacking a pin, sync must fail fast instead of ever attempting a
// plaintext (AllowPlain=false would only fail later, after a network round
// trip) pull. (The syncCmd wrapper was deleted in Plan 20 T1; the panel pull
// is syncCmdMode(cred, false).)
func TestSyncCmdRefusesEmptyPin(t *testing.T) {
	cred := &clientops.CacheCred{URL: "https://x", Token: "t", Pin: ""}
	msg := syncCmdMode(cred, false)()
	done, ok := msg.(syncDoneMsg)
	if !ok {
		t.Fatalf("want syncDoneMsg, got %T", msg)
	}
	if done.err == nil {
		t.Fatal("want non-nil err for empty pin")
	}
	if !strings.Contains(done.err.Error(), "明文") && !strings.Contains(done.err.Error(), "pin") {
		t.Fatalf("err should mention pin/明文: %v", done.err)
	}
}

// TestClassifyPullError pins the wizard's four-state pull diagnosis (task-5
// brief, verbatim): dial/no-such-host → 地址不通, 401/authorization → 设备码无效,
// mismatch/fingerprint → 指纹失配, Timeout → 超时 — each category name must
// appear in the classified banner. The unknown-error default is checked too.
func TestClassifyPullError(t *testing.T) {
	cases := map[string]string{
		`Get "https://x": dial tcp: no route`:                                  "地址不通",
		`pull: server returned 401`:                                            "设备码无效",
		`server fingerprint mismatch (expected a, got b)`:                      "指纹失配",
		`Get "https://x": context deadline exceeded (Client.Timeout exceeded)`: "超时",
	}
	for raw, want := range cases {
		if got := classifyPullError(errors.New(raw)); !strings.Contains(got, want) {
			t.Fatalf("classify(%q) = %q, want contains %q", raw, got, want)
		}
	}
	if got := classifyPullError(errors.New("boom")); !strings.Contains(got, "boom") {
		t.Fatalf("default must keep the raw error text: %q", got)
	}
}

// TestClassifyPullError401 pins the 401 branch's PRECISION: only a genuine
// "server returned 401" (or an "authorization" error) classifies as 设备码无效.
// A bare "401" substring match false-triggers on fingerprint hex digits
// (aa4011…) or port numbers (1401) — those must land in a different class.
func TestClassifyPullError401(t *testing.T) {
	// 正例：真 401 — both the brief's generic shape and the real emitter's
	// shape (clientops: "pull: server returned %d (is the authorization code valid/active?)").
	for _, raw := range []string{
		`Get "https://x/snapshot": server returned 401 Unauthorized`,
		`pull: server returned 401 (is the authorization code valid/active?)`,
	} {
		if got := classifyPullError(errors.New(raw)); !strings.Contains(got, "设备码无效") {
			t.Fatalf("真 401 未分类: %q → %q", raw, got)
		}
	}
	// 负例：指纹 hex 含 "401" 子串 / 端口 1401 — must NOT classify as 设备码无效.
	for _, s := range []string{
		"dial tcp: 1401 connection refused",
		"pin sha256:aa4011... mismatch",
	} {
		if got := classifyPullError(errors.New(s)); strings.Contains(got, "设备码无效") {
			t.Fatalf("非 401 误分类: %q → %q", s, got)
		}
	}
}

// TestClientWizard_PullFailureReopensFormWithDraft pins the input-preservation
// invariant (task-5 brief): a failed first pull reopens the connection form
// with the previously submitted url/pin still in it (code stays empty — a
// masked secret is never re-echoed), under a classified error banner.
func TestClientWizard_PullFailureReopensFormWithDraft(t *testing.T) {
	m := newClientModel()
	m.wizard = true
	m.draft = &connDraft{URL: "https://192.0.2.9:7878", Pin: "sha256:" + strings.Repeat("b", 64)}

	nm, _ := m.Update(syncDoneMsg{err: errors.New("pull: server returned 401")})
	m2, ok := nm.(clientModel)
	if !ok {
		t.Fatalf("want clientModel, got %T", nm)
	}
	if m2.draft == nil || m2.draft.URL != "https://192.0.2.9:7878" || m2.draft.Pin != m.draft.Pin {
		t.Fatalf("draft must survive the failed pull: %+v", m2.draft)
	}
	if m2.overlay == nil {
		t.Fatal("failed wizard pull must reopen the connection form")
	}
	v := m2.View().Content
	if !strings.Contains(v, "设备码无效") {
		t.Fatalf("view must show the classified banner:\n%s", v)
	}
	if !strings.Contains(v, "192.0.2.9") {
		t.Fatalf("reopened form must prefill the previous url:\n%s", v)
	}
}

// TestEditConnFormRequiresCodeWhenNoToken: on a fresh machine (no stored cred)
// the connection form must refuse an empty 设备码 at submit — "留空=保持不变"
// has nothing to keep, and a token-less cred could never authorize a pull.
func TestEditConnFormRequiresCodeWhenNoToken(t *testing.T) {
	m := newClientModel()
	m.wizard = true
	fo, ok := m.editConnForm().(*formOverlay)
	if !ok {
		t.Fatal("editConnForm must return a formOverlay")
	}
	msg := fo.action()()
	if _, ok := msg.(errMsg); !ok {
		t.Fatalf("want errMsg for empty code with no stored token, got %T", msg)
	}
}

// TestClientFinishScreen: the client wizard's finish screen must show the
// --cache variant of the .mcp.json snippet — never the standalone plain-mcp
// shape (the two modes are mutually exclusive and a wrong snippet breaks the
// agent silently).
func TestClientFinishScreen(t *testing.T) {
	v := clientFinishScreen().View().Content
	for _, want := range []string{`"mcp", "--cache", "--token"`, "project token"} {
		if !strings.Contains(v, want) {
			t.Fatalf("finish screen missing %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, `["mcp", "--token"`) {
		t.Fatalf("client finish must not show the plain (non-cache) args:\n%s", v)
	}
}
