package conformance

import (
	"testing"

	"ssh-manager-mcp/internal/sshbroker"

	"golang.org/x/crypto/ssh"
)

// TestHarnessSmoke proves the docker sshd + ssh-binary + broker client all reach the
// same real OpenSSH sshd and return matching output. Gates every later conformance test.
func TestHarnessSmoke(t *testing.T) {
	requireConformance(t)
	privPath, pub := generateKey(t, "ed25519", "")
	host, port, hostKey, _, cleanup := startOpenSSH(t, OpenSSHOpts{AuthorizedPubKey: pub})
	defer cleanup()

	// Broker path (Go SSH), trusting the known host key on first use via FixedHostKey.
	cb := ssh.FixedHostKey(hostKey)
	cli, err := sshbroker.Connect(host, port, "sshuser", mustPrivAuth(t, privPath, ""), cb)
	if err != nil {
		t.Fatalf("broker connect: %v", err)
	}
	defer cli.Close()
	res, err := cli.Exec("printf %s hi-broker", 0, 0)
	if err != nil {
		t.Fatalf("broker exec: %v", err)
	}
	if res.Stdout != "hi-broker" {
		t.Fatalf("broker stdout = %q, want hi-broker", res.Stdout)
	}

	// ssh-binary path.
	out, _, code := runSSHBinary(t, append(sshBinaryKeyAuthArgs(host, port, "sshuser", privPath), "printf %s hi-bin")...)
	if code != 0 || out != "hi-bin" {
		t.Fatalf("ssh binary: code=%d stdout=%q", code, out)
	}
}
