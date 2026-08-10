package sshbroker

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/testsshd"

	"golang.org/x/crypto/ssh"
)

func connectTest(t *testing.T, addr string, hostKey ssh.PublicKey) *Client {
	t.Helper()
	cli, err := Connect(context.Background(), hostOf(addr), portOf(addr), "u", PasswordAuth("pw"), ssh.FixedHostKey(hostKey))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { cli.Close() })
	return cli
}

func TestExecStdoutAndExitCode(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec: func(cmd string, _ io.Reader) (string, string, int) {
			if cmd == "exit 7" {
				return "", "", 7
			}
			return "out:" + cmd + "\n", "", 0
		},
	})
	defer cleanup()
	c := connectTest(t, addr, hk)

	res, err := c.Exec(context.Background(), "hello", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "out:hello\n" || res.ExitCode != 0 {
		t.Fatalf("unexpected %+v", res)
	}

	res2, _ := c.Exec(context.Background(), "exit 7", 0, 0)
	if res2.ExitCode != 7 {
		t.Fatalf("exit code = %d, want 7", res2.ExitCode)
	}
}

func TestExecTimeoutKillsAndFlags(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec: func(cmd string, _ io.Reader) (string, string, int) {
			// simulate a long-running command that only returns after the timeout fires.
			time.Sleep(2 * time.Second)
			return "done\n", "", 0
		},
	})
	defer cleanup()
	c := connectTest(t, addr, hk)

	res, err := c.Exec(context.Background(), "slow", 200*time.Millisecond, 0)
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

func TestExecTruncatesLargeOutput(t *testing.T) {
	const cap int64 = 1 << 6 // 64 bytes — small so the test is fast
	big := strings.Repeat("x", int(cap)*4)
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec:     func(cmd string, _ io.Reader) (string, string, int) { return big, "", 0 },
	})
	defer cleanup()
	c := connectTest(t, addr, hk)

	res, err := c.Exec(context.Background(), "big", 0, cap)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if int64(len(res.Stdout)) != cap {
		t.Fatalf("stdout len=%d want %d", len(res.Stdout), cap)
	}
	if res.StdoutBytes != int64(len(big)) {
		t.Fatalf("stdout total=%d want %d", res.StdoutBytes, len(big))
	}
	if !res.Truncated {
		t.Fatal("want Truncated=true")
	}
}

func TestExecTruncatesStderrIndependently(t *testing.T) {
	const cap int64 = 1 << 6
	big := strings.Repeat("e", int(cap)*2)
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec:     func(cmd string, _ io.Reader) (string, string, int) { return "", big, 0 },
	})
	defer cleanup()
	c := connectTest(t, addr, hk)

	res, err := c.Exec(context.Background(), "bigerr", 0, cap)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if int64(len(res.Stderr)) != cap {
		t.Fatalf("stderr len=%d want %d", len(res.Stderr), cap)
	}
	if res.StderrBytes != int64(len(big)) {
		t.Fatalf("stderr total=%d want %d", res.StderrBytes, len(big))
	}
	if !res.Truncated {
		t.Fatal("want Truncated=true (stderr capped)")
	}
}

func TestExecAtCapNotTruncated(t *testing.T) {
	const cap int64 = 1 << 6
	exact := strings.Repeat("y", int(cap))
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec:     func(cmd string, _ io.Reader) (string, string, int) { return exact, "", 0 },
	})
	defer cleanup()
	c := connectTest(t, addr, hk)

	res, err := c.Exec(context.Background(), "exact", 0, cap)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if int64(len(res.Stdout)) != cap {
		t.Fatalf("stdout len=%d want %d", len(res.Stdout), cap)
	}
	if res.StdoutBytes != cap {
		t.Fatalf("total=%d want %d", res.StdoutBytes, cap)
	}
	if res.Truncated {
		t.Fatal("exactly at cap must NOT be truncated")
	}
}

func TestExecUnlimitedMaxBytesZero(t *testing.T) {
	const cap int64 = 1 << 6
	big := strings.Repeat("z", int(cap)*3)
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec:     func(cmd string, _ io.Reader) (string, string, int) { return big, "", 0 },
	})
	defer cleanup()
	c := connectTest(t, addr, hk)

	res, err := c.Exec(context.Background(), "big", 0, 0) // maxBytes=0 → unlimited
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.Stdout != big {
		t.Fatalf("stdout len=%d want %d (unlimited)", len(res.Stdout), len(big))
	}
	if res.StdoutBytes != int64(len(big)) {
		t.Fatalf("total=%d want %d", res.StdoutBytes, len(big))
	}
	if res.Truncated {
		t.Fatal("unlimited must never be truncated")
	}
}

// TestExecCancelContext proves a caller cancellation aborts an in-flight Exec
// promptly via the SIGKILL+Close path (the same one timeout uses) and surfaces as
// context.Canceled — NOT flagged as TimedOut. The testsshd Exec callback blocks on
// a fixed sleep so the command is reliably still running when we cancel at 100ms.
func TestExecCancelContext(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec: func(cmd string, _ io.Reader) (string, string, int) {
			time.Sleep(30 * time.Second) // in-flight; cancel must abort via sess.Close
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
	res, err := c.Exec(ctx, "slow", 0, 0) // timeout=0 → only ctx cancel can fire
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if res.TimedOut {
		t.Fatal("TimedOut=true on cancel, want false (cancel ≠ timeout)")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Exec took %v on cancel, want < 2s (sleep 30 should have been aborted)", elapsed)
	}
}
