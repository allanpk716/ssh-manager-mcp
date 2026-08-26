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
	code, timedOut, err := c.ExecSudoWriters(context.Background(), composite, "sudopw", 0, &out, &errBuf)
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
}
