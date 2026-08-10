package conformance

import (
	"context"
	"errors"
	"testing"
	"time"

	"ssh-manager-mcp/internal/sshbroker"

	"golang.org/x/crypto/ssh"
)

// TestCancellationAbortsRealExec proves ctx cancellation aborts an in-flight
// Exec against REAL openssh promptly (the broker's SIGKILL+Close path), mirroring
// what happens when a real ssh client disconnects mid-command. Guards the Item-1
// cancellation propagation end-to-end against the real server (the in-process
// testsshd unit tests cover the mechanism; this covers the wire).
func TestCancellationAbortsRealExec(t *testing.T) {
	requireConformance(t)

	privPath, pub := generateKey(t, "ed25519", "")
	host, port, hostKey, _, cleanup := startOpenSSH(t, OpenSSHOpts{AuthorizedPubKey: pub})
	defer cleanup()

	cli, err := sshbroker.Connect(context.Background(), host, port, "sshuser", mustPrivAuth(t, privPath, ""), ssh.FixedHostKey(hostKey))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cli.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(time.Second)
		cancel()
	}()

	start := time.Now()
	_, err = cli.Exec(ctx, "sleep 60", 0, 0)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("cancel took %v, want < 5s (sleep 60 should have been aborted)", elapsed)
	}
}
