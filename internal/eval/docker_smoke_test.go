package eval

import (
	"context"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/sshbroker"

	"golang.org/x/crypto/ssh"
)

// TestEvalSSHDNvidiaSMI proves the eval image + helper work: start the
// container, connect as agent/testpw123 over SSH, run nvidia-smi, and assert
// the known memory figure. No LLM call — just docker + the broker's ssh client.
func TestEvalSSHDNvidiaSMI(t *testing.T) {
	requireEval(t)
	host, port, _, cleanup := startEvalSSHD(t)
	defer cleanup()

	// Smoke only: InsecureIgnoreHostKey. The eval agent path does TOFU via the
	// broker's host-key store; the per-container key regen in the Dockerfile CMD
	// makes a pinned key useless across runs anyway.
	cb := ssh.InsecureIgnoreHostKey()
	cli, err := sshbroker.Connect(context.Background(), host, port, "agent", sshbroker.PasswordAuth("testpw123"), cb)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cli.Close()
	res, err := cli.Exec(context.Background(), "nvidia-smi", 0, 0)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !strings.Contains(res.Stdout, "24576 MiB") {
		t.Fatalf("nvidia-smi output missing the known figure:\n%s", res.Stdout)
	}
}
