package sshbroker

import (
	"io"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/testsshd"

	"golang.org/x/crypto/ssh"
)

func connectTest(t *testing.T, addr string, hostKey ssh.PublicKey) *Client {
	t.Helper()
	cli, err := Connect(hostOf(addr), portOf(addr), "u", PasswordAuth("pw"), ssh.FixedHostKey(hostKey))
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

	res, err := c.Exec("hello", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "out:hello\n" || res.ExitCode != 0 {
		t.Fatalf("unexpected %+v", res)
	}

	res2, _ := c.Exec("exit 7", 0, 0)
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

	res, err := c.Exec("slow", 200*time.Millisecond, 0)
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

	res, err := c.Exec("big", 0, cap)
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

	res, err := c.Exec("bigerr", 0, cap)
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

	res, err := c.Exec("exact", 0, cap)
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

	res, err := c.Exec("big", 0, 0) // maxBytes=0 → unlimited
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
