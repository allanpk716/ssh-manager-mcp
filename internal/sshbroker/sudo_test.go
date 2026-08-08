package sshbroker

import (
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

	res, err := c.ExecSudo("whoami", []byte("sudopw"), 0)
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

	res, err := c.ExecSudo("slow", []byte("sudopw"), 200*time.Millisecond)
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
