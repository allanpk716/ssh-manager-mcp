package sshbroker

import (
	"io"
	"testing"

	"ssh-manager-mcp/internal/testsshd"

	"golang.org/x/crypto/ssh"
)

func TestConnectPasswordAuth(t *testing.T) {
	addr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "secret",
		Exec:     func(string, io.Reader) (string, string, int) { return "ok\n", "", 0 },
	})
	defer cleanup()
	cb := ssh.FixedHostKey(hostKey)
	cli, err := Connect(hostOf(addr), portOf(addr), "u", PasswordAuth("secret"), cb)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cli.Close()
}

func TestConnectPasswordAuthRejected(t *testing.T) {
	addr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{Password: "secret"})
	defer cleanup()
	_, err := Connect(hostOf(addr), portOf(addr), "u", PasswordAuth("wrong"), ssh.FixedHostKey(hostKey))
	if err == nil {
		t.Fatal("connect with wrong password must fail")
	}
}
