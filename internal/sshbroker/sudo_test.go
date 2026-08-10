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
