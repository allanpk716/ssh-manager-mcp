package testsshd

import (
	"io"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestStartExecutesCommand(t *testing.T) {
	addr, hostKey, cleanup := Start(t, Options{
		Password: "pw",
		Exec: func(cmd string, _ io.Reader) (string, string, int) {
			if cmd == "echo hi" {
				return "hi\n", "", 0
			}
			return "", "unknown cmd", 1
		},
	})
	defer cleanup()

	cfg := &ssh.ClientConfig{
		User:            "u",
		Auth:            []ssh.AuthMethod{ssh.Password("pw")},
		HostKeyCallback: ssh.FixedHostKey(hostKey),
	}
	cli, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cli.Close()
	sess, err := cli.NewSession()
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	out, err := sess.CombinedOutput("echo hi")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if strings.TrimSpace(string(out)) != "hi" {
		t.Fatalf("got %q", out)
	}
}
