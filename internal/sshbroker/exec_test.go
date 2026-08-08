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

	res, err := c.Exec("hello", 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "out:hello\n" || res.ExitCode != 0 {
		t.Fatalf("unexpected %+v", res)
	}

	res2, _ := c.Exec("exit 7", 0)
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

	res, err := c.Exec("slow", 200*time.Millisecond)
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
