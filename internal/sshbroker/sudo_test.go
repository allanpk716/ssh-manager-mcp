package sshbroker

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/testsshd"
)

func TestExecSudoFeedsPasswordAndRunsInner(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password:     "pw",
		SudoPassword: "sudopw",
		Exec: func(cmd string, stdin io.Reader) (string, string, int) {
			// testsshd consumes the sudo pw line (SudoPassword set), then passes the inner cmd here.
			if cmd == "whoami" {
				return "root\n", "", 0
			}
			return "", "unknown", 1
		},
	})
	defer cleanup()
	c := connectTest(t, addr, hk)

	res, err := c.ExecSudo(context.Background(), "whoami", []byte("sudopw"), 0, 0)
	if err != nil {
		t.Fatalf("execSudo: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "root" {
		t.Fatalf("stdout = %q, want root", res.Stdout)
	}
}

func TestExecSudoTimeoutKillsAndFlags(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password:     "pw",
		SudoPassword: "sudopw",
		Exec: func(cmd string, _ io.Reader) (string, string, int) {
			time.Sleep(2 * time.Second)
			return "done\n", "", 0
		},
	})
	defer cleanup()
	c := connectTest(t, addr, hk)

	res, err := c.ExecSudo(context.Background(), "slow", []byte("sudopw"), 200*time.Millisecond, 0)
	if err != nil && !res.TimedOut {
		t.Fatalf("err: %v", err)
	}
	if !res.TimedOut {
		t.Fatal("expected TimedOut=true")
	}
	if strings.Contains(res.Stdout, "done") {
		t.Fatal("should not have completed")
	}
}

func TestExecSudoTruncatesLargeOutput(t *testing.T) {
	const cap int64 = 1 << 6
	big := strings.Repeat("x", int(cap)*4)
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password:     "pw",
		SudoPassword: "sudopw",
		Exec:         func(cmd string, _ io.Reader) (string, string, int) { return big, "", 0 },
	})
	defer cleanup()
	c := connectTest(t, addr, hk)

	res, err := c.ExecSudo(context.Background(), "big", []byte("sudopw"), 0, cap)
	if err != nil {
		t.Fatalf("execSudo: %v", err)
	}
	if int64(len(res.Stdout)) != cap {
		t.Fatalf("stdout len=%d want %d", len(res.Stdout), cap)
	}
	if res.StdoutBytes != int64(len(big)) {
		t.Fatalf("total=%d want %d", res.StdoutBytes, len(big))
	}
	if !res.Truncated {
		t.Fatal("want Truncated=true")
	}
}

func TestExecSudoCancelContext(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw", SudoPassword: "sudopw",
		Exec: func(cmd string, _ io.Reader) (string, string, int) {
			time.Sleep(30 * time.Second)
			return "done\n", "", 0
		},
	})
	defer cleanup()
	c := connectTest(t, addr, hk)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	res, err := c.ExecSudo(ctx, "slow", []byte("sudopw"), 0, 0)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if res.TimedOut {
		t.Fatal("TimedOut=true on cancel, want false")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("ExecSudo took %v on cancel, want < 2s", elapsed)
	}
}

// TestExecSudoPreCancelledCtx pins the ctxErrOr fix: a pre-cancelled ctx must
// surface as context.Canceled even when the cancellation bites during the narrow
// sess.Start → stdin.Write window (the watchdog closes the session on ctx.Done
// before the password write completes, so Start/Write would otherwise return a
// generic non-Canceled error). With ctxErrOr every race outcome returns Canceled.
func TestExecSudoPreCancelledCtx(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw", SudoPassword: "sudopw",
		Exec: func(cmd string, _ io.Reader) (string, string, int) { return "", "", 0 },
	})
	defer cleanup()
	c := connectTest(t, addr, hk)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled

	res, err := c.ExecSudo(ctx, "whoami", []byte("sudopw"), 0, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled (pre-cancel must surface as Canceled even in the Start/stdin.Write window)", err)
	}
	if res.TimedOut {
		t.Fatal("TimedOut=true on cancel, want false")
	}
}

// --- Plan 41 rev3 §1.3 batch-1 matrix ---

// §1.3-2: shellQuote is complete — every special character survives a
// quote/decode round-trip byte-for-byte (decode = what any POSIX shell does:
// consume the outer quotes, collapse each '\” splice back into a quote).
func TestShellQuoteRoundTrip(t *testing.T) {
	cases := []string{
		`plain`,
		`it's`,
		`double " and ' mixed`,
		"$HOME `backtick` \\backslash",
		"a; b && c | d || e",
		"line1\nline2",
		`'; rm -rf /`,
		"中文 and spaces",
		"",
	}
	for _, s := range cases {
		q := shellQuote(s)
		if !strings.HasPrefix(q, `'`) || !strings.HasSuffix(q, `'`) {
			t.Fatalf("shellQuote(%q) = %q, want single-quoted", s, q)
		}
		decoded := strings.ReplaceAll(strings.TrimSuffix(strings.TrimPrefix(q, `'`), `'`), `'\''`, `'`)
		if decoded != s {
			t.Fatalf("round-trip mismatch:\n  in:  %q\n  out: %q", s, decoded)
		}
	}
}

// §1.3-1: the wrapper's shape — BASH_ENV=/LC_ALL=C prefix, --, bash -c, and
// the quoted command as a single argument (the sudo domain spans the whole
// command only because of this shape).
func TestBuildSudoWrapperShape(t *testing.T) {
	w := buildSudoWrapper(`id; id`)
	const prefix = `BASH_ENV= LC_ALL=C sudo -S -p '' -- bash -c `
	if !strings.HasPrefix(w, prefix) {
		t.Fatalf("wrapper = %q, want prefix %q", w, prefix)
	}
	arg := strings.TrimPrefix(w, prefix)
	if arg != `'id; id'` {
		t.Fatalf("wrapper arg = %q, want 'id; id' (quoted)", arg)
	}
	// A quoted embedded quote must splice, not terminate the argument.
	w2 := buildSudoWrapper(`echo 'hi'`)
	if !strings.Contains(w2, `'echo '\''hi'\'''`) {
		t.Fatalf("wrapper for quoted cmd = %q, want spliced quotes", w2)
	}
}

// §1.3-1/2 e2e: a composite command survives both simulated shell layers of
// the wrapper intact — the Exec handler (i.e. the elevated side) receives the
// caller's original string, proving the whole `id; id` travels INSIDE the
// sudo domain rather than being split at the separator (the v0.10.0 defect).
func TestExecSudoCompositeCommandStaysIntact(t *testing.T) {
	const composite = `id; id`
	var got string
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw", SudoPassword: "sudopw",
		Exec: func(cmd string, _ io.Reader) (string, string, int) {
			got = cmd
			return "uid=0(root) uid=0(root)\n", "", 0
		},
	})
	defer cleanup()
	c := connectTest(t, addr, hk)

	if _, err := c.ExecSudo(context.Background(), composite, []byte("sudopw"), 0, 0); err != nil {
		t.Fatalf("execSudo: %v", err)
	}
	if got != composite {
		t.Fatalf("elevated side received %q, want the whole composite %q (no separator split)", got, composite)
	}
}

// §1.3-2 e2e: specials round-trip through the wrapper byte-for-byte.
func TestExecSudoQuotingSpecialsRoundTrip(t *testing.T) {
	cmd := "echo \"a'b\" $VAR `bt` \\\\ ; && | 中文"
	var got string
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw", SudoPassword: "sudopw",
		Exec: func(c string, _ io.Reader) (string, string, int) {
			got = c
			return "", "", 0
		},
	})
	defer cleanup()
	c := connectTest(t, addr, hk)

	if _, err := c.ExecSudo(context.Background(), cmd, []byte("sudopw"), 0, 0); err != nil {
		t.Fatalf("execSudo: %v", err)
	}
	if got != cmd {
		t.Fatalf("round-trip mismatch:\n  sent: %q\n  recv: %q", cmd, got)
	}
}

// §1.3-3: an injection-shaped command is delivered verbatim — quoting never
// lets payload text break out into a second command on the wire.
func TestExecSudoInjectionDoesNotEscape(t *testing.T) {
	cmd := `'; touch /tmp/pwn; echo `
	var got string
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw", SudoPassword: "sudopw",
		Exec: func(c string, _ io.Reader) (string, string, int) {
			got = c
			return "", "", 0
		},
	})
	defer cleanup()
	c := connectTest(t, addr, hk)

	if _, err := c.ExecSudo(context.Background(), cmd, []byte("sudopw"), 0, 0); err != nil {
		t.Fatalf("execSudo: %v", err)
	}
	if got != cmd {
		t.Fatalf("injection-shaped cmd altered in transit:\n  sent: %q\n  recv: %q", cmd, got)
	}
}

// §1.3-5: the inner exit code propagates through the bash -c layer.
func TestExecSudoExitCodePropagates(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw", SudoPassword: "sudopw",
		Exec: func(cmd string, _ io.Reader) (string, string, int) { return "", "", 7 },
	})
	defer cleanup()
	c := connectTest(t, addr, hk)

	res, err := c.ExecSudo(context.Background(), "exit 7", []byte("sudopw"), 0, 0)
	if err != nil {
		t.Fatalf("execSudo: %v", err)
	}
	if res.ExitCode != 7 {
		t.Fatalf("ExitCode = %d, want 7", res.ExitCode)
	}
}

// §1.3-7: a wrong password fails loud — exit 1 plus the sudo
// incorrect-password signature, and the command itself never runs.
func TestExecSudoWrongPasswordFailsLoud(t *testing.T) {
	ran := false
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw", SudoPassword: "sudopw",
		Exec: func(cmd string, _ io.Reader) (string, string, int) {
			ran = true
			return "", "", 0
		},
	})
	defer cleanup()
	c := connectTest(t, addr, hk)

	res, err := c.ExecSudo(context.Background(), "whoami", []byte("WRONG"), 0, 0)
	if err != nil {
		t.Fatalf("execSudo: %v", err)
	}
	if res.ExitCode != 1 {
		t.Fatalf("ExitCode = %d, want 1", res.ExitCode)
	}
	if !strings.Contains(res.Stderr, "sudo: 1 incorrect password attempt") {
		t.Fatalf("stderr = %q, want the incorrect-password signature", res.Stderr)
	}
	if ran {
		t.Fatal("the command must NOT run when sudo authentication fails")
	}
}

// --- Plan 41 rev3 §6 batch-2a matrix ---

// §6-9/10/13/14: the stderr processor's unit matrix — every strip/skip/keep
// rule and all five outcomes, driven directly (streaming Write calls; chunk
// boundaries deliberately split one line across two Writes to pin the line
// assembly).
func TestSudoStderrProcessorMatrix(t *testing.T) {
	nonce := "abcd1234abcd1234"
	marker := func(uid string) string { return "__SSHMGR_SUDO_" + nonce + ":uid=" + uid }
	var b strings.Builder

	// Fused prompt+marker on ONE line (the no-blank-echo legacy shape the
	// tolerant regex must still match) — split across two Writes.
	p := newSudoStderrProcessor(&b, nonce)
	p.Write([]byte("[sudo] password for x: __SSHMGR_SUD"))
	p.Write([]byte("O_" + nonce + ":uid=0\n"))
	p.Write([]byte("cmd stderr keeps [sudo] password for y: literals\n"))
	p.flush()
	meta := p.classify()
	if meta.Outcome != SudoElevated || meta.UID == nil || *meta.UID != 0 {
		t.Fatalf("meta = %+v, want elevated/uid=0 (fused single line)", meta)
	}
	if got := b.String(); got != "cmd stderr keeps [sudo] password for y: literals\n" {
		t.Fatalf("cleaned = %q, want post-marker verbatim (grep-log literals preserved)", got)
	}

	// Double marker (forged second line via /proc/$PPID/cmdline): first wins,
	// the forged line is stripped, the command's own marker-shaped noise is not
	// mistaken for anything.
	b.Reset()
	p = newSudoStderrProcessor(&b, nonce)
	p.Write([]byte(marker("0") + "\ncmd out\n" + marker("1000") + "\nmore out\n"))
	p.flush()
	meta = p.classify()
	if meta.Outcome != SudoElevated || meta.UID == nil || *meta.UID != 0 {
		t.Fatalf("meta = %+v, want elevated/uid=0 (first marker only, forged uid=1000 ignored)", meta)
	}
	if got := b.String(); got != "cmd out\nmore out\n" {
		t.Fatalf("cleaned = %q, want forged marker line stripped", got)
	}

	// Injected blank echo (standalone blank directly before the marker — the
	// passwordless/cached-credential shape) is dropped; a blank NOT followed by
	// the marker (sudo lecture) is kept.
	b.Reset()
	p = newSudoStderrProcessor(&b, nonce)
	p.Write([]byte("\n" + marker("0") + "\n"))
	p.flush()
	if got := b.String(); got != "" {
		t.Fatalf("cleaned = %q, want injected blank dropped", got)
	}
	b.Reset()
	p = newSudoStderrProcessor(&b, nonce)
	p.Write([]byte("lecture line\n\nmore lecture\n"))
	p.Write([]byte(marker("0") + "\nreal out\n"))
	p.flush()
	if got := b.String(); got != "lecture line\n\nmore lecture\nreal out\n" {
		t.Fatalf("cleaned = %q, want lecture blanks kept", got)
	}

	// Prompt-only line dropped; unterminated trailing prompt stripped at flush.
	b.Reset()
	p = newSudoStderrProcessor(&b, nonce)
	p.Write([]byte("[sudo] password for x: \n"))
	p.Write([]byte(marker("0") + "\nout\n"))
	p.flush()
	if got := b.String(); got != "out\n" {
		t.Fatalf("cleaned = %q, want prompt-only line dropped", got)
	}

	// Failure classification, no marker (command never ran):
	cases := []struct {
		name, stderr, want string
	}{
		{"auth-failed fused", "[sudo] password for x: sudo: 1 incorrect password attempt\n", SudoAuthFailed},
		{"auth-failed plural", "sudo: 3 incorrect password attempts\n", SudoAuthFailed},
		{"start not-in-sudoers", "x is not in the sudoers file.  This incident will be reported.\n", SudoStartFailed},
		{"start sorry-user", "Sorry, user x is not allowed to execute /usr/bin/bash as root on host.\n", SudoStartFailed},
		{"wrap syntax error", "bash: -c: line 1: syntax error near unexpected token `)'\n", SudoWrapFailed},
		{"unknown", "something else entirely\n", SudoUnverified},
	}
	for _, tc := range cases {
		b.Reset()
		p = newSudoStderrProcessor(&b, nonce)
		p.Write([]byte(tc.stderr))
		p.flush()
		if got := p.classify().Outcome; got != tc.want {
			t.Fatalf("%s: outcome = %q, want %q", tc.name, got, tc.want)
		}
	}

	// Timing anomaly: marker present AND a failure signature BEFORE it — the
	// command DID run, so did-NOT-run outcomes contradict; pin to unverified.
	b.Reset()
	p = newSudoStderrProcessor(&b, nonce)
	p.Write([]byte("sudo: 1 incorrect password attempt\n" + marker("0") + "\nout\n"))
	p.flush()
	if got := p.classify().Outcome; got != SudoUnverified {
		t.Fatalf("timing anomaly: outcome = %q, want unverified", got)
	}
}

// §6-8: elevated ExecResult carries the meta with the marker-attested uid.
func TestExecSudoElevatedMeta(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw", SudoPassword: "sudopw",
		Exec: func(cmd string, _ io.Reader) (string, string, int) { return "ok\n", "", 0 },
	})
	defer cleanup()
	c := connectTest(t, addr, hk)

	res, err := c.ExecSudo(context.Background(), "true", []byte("sudopw"), 0, 0)
	if err != nil {
		t.Fatalf("execSudo: %v", err)
	}
	if res.Sudo == nil || res.Sudo.Outcome != SudoElevated || res.Sudo.UID == nil || *res.Sudo.UID != 0 {
		t.Fatalf("Sudo = %+v, want elevated/uid=0", res.Sudo)
	}
	// The fake server emits the prompt + marker on stderr; the cleaned stream
	// the caller sees must contain neither.
	if strings.Contains(res.Stderr, "[sudo] password for") || strings.Contains(res.Stderr, "__SSHMGR_SUDO_") {
		t.Fatalf("Stderr = %q, want prompt and marker stripped", res.Stderr)
	}
}

// §6-11: wrong password → auth-failed meta + signature in the raw stream.
func TestExecSudoAuthFailedMeta(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw", SudoPassword: "sudopw",
		Exec: func(cmd string, _ io.Reader) (string, string, int) { return "RAN\n", "", 0 },
	})
	defer cleanup()
	c := connectTest(t, addr, hk)

	res, err := c.ExecSudo(context.Background(), "true", []byte("WRONG"), 0, 0)
	if err != nil {
		t.Fatalf("execSudo: %v", err)
	}
	if res.Sudo == nil || res.Sudo.Outcome != SudoAuthFailed {
		t.Fatalf("Sudo = %+v, want auth-failed", res.Sudo)
	}
	if strings.Contains(res.Stdout, "RAN") {
		t.Fatal("the command must NOT run on auth failure")
	}
	if !strings.Contains(res.Sudo.Diagnostic, "incorrect password attempt") {
		t.Fatalf("Diagnostic = %q, want the signature", res.Sudo.Diagnostic)
	}
}

// §6-11: sudoers-style start refusal (SudoStartFailure hook) → sudo-start-failed.
func TestExecSudoStartFailedMeta(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw", SudoPassword: "sudopw",
		SudoStartFailure: "Sorry, user u is not allowed to execute /usr/bin/bash as root on h.",
		Exec:             func(cmd string, _ io.Reader) (string, string, int) { return "RAN\n", "", 0 },
	})
	defer cleanup()
	c := connectTest(t, addr, hk)

	res, err := c.ExecSudo(context.Background(), "true", []byte("sudopw"), 0, 0)
	if err != nil {
		t.Fatalf("execSudo: %v", err)
	}
	if res.Sudo == nil || res.Sudo.Outcome != SudoStartFailed {
		t.Fatalf("Sudo = %+v, want sudo-start-failed", res.Sudo)
	}
	if strings.Contains(res.Stdout, "RAN") {
		t.Fatal("the command must NOT run on start failure")
	}
}

// §1.3-6: the background engine's privileged variant rides the same kernel —
// the composite round-trip must hold there too.
func TestExecSudoWritersCompositeRoundTrip(t *testing.T) {
	const composite = `id; id`
	var got string
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw", SudoPassword: "sudopw",
		Exec: func(cmd string, _ io.Reader) (string, string, int) {
			got = cmd
			return "uid=0(root) uid=0(root)\n", "", 0
		},
	})
	defer cleanup()
	c := connectTest(t, addr, hk)

	var out, errBuf strings.Builder
	code, timedOut, err, meta := c.ExecSudoWriters(context.Background(), composite, "sudopw", 0, &out, &errBuf)
	if err != nil {
		t.Fatalf("ExecSudoWriters: %v", err)
	}
	if timedOut || code != 0 {
		t.Fatalf("code=%d timedOut=%v, want 0/false", code, timedOut)
	}
	if got != composite {
		t.Fatalf("elevated side received %q, want %q", got, composite)
	}
	if out.String() != "uid=0(root) uid=0(root)\n" {
		t.Fatalf("stdout = %q", out.String())
	}
	if meta.Outcome != SudoElevated || meta.UID == nil || *meta.UID != 0 {
		t.Fatalf("meta = %+v, want elevated/uid=0 (background rides the same marker kernel)", meta)
	}
}
