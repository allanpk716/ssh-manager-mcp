package mcpserver

// Ticket 06 (2026-09-21 pilot three-gaps spec): the shared profile gate
// (profilegate.go) folds case when the requested server id misses the grant
// set and, on EXACTLY ONE case-fold hit, appends a neighbor hint naming the
// correct id. Zero or multiple hits keep the original text. The error must
// stay ErrNotInProfile-shaped (errors.Is holds on the hint path too — the
// audit status routing and the existing denied tests depend on it), and the
// text must keep the "server is not in your profile" prefix (the
// docs/agent-tools.md error table matches on it).

import (
	"context"
	"errors"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// caseVariant returns a case-different form of id, skipping the (measure-zero)
// degenerate id with no letters at all.
func caseVariant(t *testing.T, id string) string {
	t.Helper()
	v := strings.ToUpper(id)
	if v == id {
		v = strings.ToLower(id)
	}
	if v == id {
		t.Skipf("id %q has no letters; no case variant exists", id)
	}
	return v
}

// TestGateServerNotInProfileCaseFoldHint pins the gate's three deny outcomes
// and the allowed path at the logic level (fabricated grant set — the F6
// two-ids-differing-only-by-case fixture is not constructible through the
// public API because server ids are store-generated).
func TestGateServerNotInProfileCaseFoldHint(t *testing.T) {
	allowed := []string{"srv-alpha", "srv-beta"}

	// Exactly one case-fold hit → hint with the correct id, still
	// ErrNotInProfile-shaped, original text as prefix.
	err := gateServerIn(allowed, "SRV-ALPHA")
	if err == nil {
		t.Fatal("want error")
	}
	if !errors.Is(err, ErrNotInProfile) {
		t.Fatalf("hint error must satisfy errors.Is(err, ErrNotInProfile), got %v", err)
	}
	want := ErrNotInProfile.Error() + ` (did you mean "srv-alpha"? server ids are case-sensitive)`
	if err.Error() != want {
		t.Fatalf("hint text =\n%q\nwant\n%q", err.Error(), want)
	}

	// Completely unrelated id → the original text, byte-for-byte unchanged.
	err = gateServerIn(allowed, "totally-other")
	if err == nil {
		t.Fatal("want error")
	}
	if err.Error() != ErrNotInProfile.Error() {
		t.Fatalf("unrelated id text = %q, want the unchanged %q", err.Error(), ErrNotInProfile.Error())
	}
	if !errors.Is(err, ErrNotInProfile) {
		t.Fatalf("plain deny must be the sentinel, got %v", err)
	}

	// F6: two ids differing only by case → ambiguous, original text unchanged.
	err = gateServerIn([]string{"web", "WEB"}, "Web")
	if err == nil {
		t.Fatal("want error")
	}
	if err.Error() != ErrNotInProfile.Error() {
		t.Fatalf("ambiguous case-fold text = %q, want the unchanged %q", err.Error(), ErrNotInProfile.Error())
	}

	// Exact member → allowed (nil).
	if err := gateServerIn(allowed, "srv-beta"); err != nil {
		t.Fatalf("granted id must pass the gate, got %v", err)
	}
}

// TestGateServerNotInProfileHintViaToolPaths walks the shared gate through
// real *ForProfile entries (assertBranch precedent, core_test.go). All four
// paths deny BEFORE any server lookup or connect, so no sshd is needed. This
// is the "one fix covers every tool" half of the acceptance: exec,
// upload_content, exec_context, relay.
func TestGateServerNotInProfileHintViaToolPaths(t *testing.T) {
	st := newStore(t)
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	const vh = "vault.example.internal" // never dialed — the gate fires first
	granted, _ := st.AddServer(&models.Server{Name: "g", Host: vh, Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	pid, _ := st.AddProfile("p")
	if err := st.GrantServers(pid, []string{granted}); err != nil {
		t.Fatal(err)
	}
	variant := caseVariant(t, granted)

	// exec_command
	_, err := ExecCommandForProfile(context.Background(), st, "proj", pid, variant, "true", false, 0)
	assertBranch(t, err, `did you mean "`+granted+`"?`)
	assertBranch(t, err, "server ids are case-sensitive")
	if !errors.Is(err, ErrNotInProfile) {
		t.Fatalf("exec hint must stay ErrNotInProfile-shaped, got %v", err)
	}
	assertNoLeak(t, err, vh)

	// upload_content (param-level validation first, then the gate)
	_, err = UploadContentForProfile(context.Background(), st, "proj", pid, variant, "data", "/x", "", 1<<20)
	assertBranch(t, err, `did you mean "`+granted+`"?`)
	if !errors.Is(err, ErrNotInProfile) {
		t.Fatalf("upload_content hint must stay ErrNotInProfile-shaped, got %v", err)
	}
	assertNoLeak(t, err, vh)

	// exec_context
	_, err = ExecContextForProfile(context.Background(), st, "proj", pid, variant, false)
	assertBranch(t, err, `did you mean "`+granted+`"?`)
	if !errors.Is(err, ErrNotInProfile) {
		t.Fatalf("exec_context hint must stay ErrNotInProfile-shaped, got %v", err)
	}
	assertNoLeak(t, err, vh)

	// relay_file (dest endpoint): valid absolute paths so ① passes and the
	// gate at ② denies before any stat/connect. The audit row must still
	// land as denied with the offending endpoint.
	tm := newTestTM(t, 4)
	defer tm.CloseAll()
	_, err = RelayForProfile(context.Background(), st, tm, "proj", pid,
		RelayInput{FromServerID: granted, FromPath: "/data/s", ToServerID: variant, ToPath: "/data/d"}, 64<<10)
	assertBranch(t, err, `did you mean "`+granted+`"?`)
	if !errors.Is(err, ErrNotInProfile) {
		t.Fatalf("relay hint must stay ErrNotInProfile-shaped, got %v", err)
	}
	assertNoLeak(t, err, vh)

	// An unrelated id on the same path keeps the original text (the hint is
	// not a blanket rewrite).
	_, err = RelayForProfile(context.Background(), st, tm, "proj", pid,
		RelayInput{FromServerID: granted, FromPath: "/data/s", ToServerID: "bogus-dest", ToPath: "/data/d"}, 64<<10)
	assertBranch(t, err, "not in your profile")
	if err.Error() != ErrNotInProfile.Error() {
		t.Fatalf("unrelated dest text = %q, want the unchanged %q", err.Error(), ErrNotInProfile.Error())
	}
}
