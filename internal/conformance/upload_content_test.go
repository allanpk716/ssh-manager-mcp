package conformance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/sshbroker"

	"golang.org/x/crypto/ssh"
)

// TestUploadContentRealSSH exercises the Plan-33 WriteFile primitive against a
// REAL OpenSSH container (the §13 conformance role — the mcpserver suite
// covers the same behavior against the in-process testsshd; THIS is the
// real-wire evidence): binary byte-exactness via sha256 readback through the
// server itself, deep parent creation, and overwrite truncation. Double-gated
// (requireConformance) like the other real-SSH suites.
func TestUploadContentRealSSH(t *testing.T) {
	requireConformance(t)
	privPath, pub := generateKey(t, "ed25519", "")
	host, port, hostKey, _, cleanup := startOpenSSH(t, OpenSSHOpts{AuthorizedPubKey: pub})
	defer cleanup()

	auth := mustPrivAuth(t, privPath, "")
	hkCb := ssh.FixedHostKey(hostKey)
	c, err := sshbroker.Connect(context.Background(), host, port, "sshuser", auth, hkCb)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()
	ctx := context.Background()

	// 1. binary byte-exact: write 64 KiB of pseudo-random-ish bytes, verify
	//    via the SERVER's own sha256sum (round trip through exec, not SFTP-get).
	payload := make([]byte, 64*1024)
	for i := range payload {
		payload[i] = byte(i*7 + i/251)
	}
	target := "/tmp/plan33-uc/blob/deep/data.bin"
	if err := c.WriteFile(ctx, target, bytes.NewReader(payload)); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	sum := sha256.Sum256(payload)
	wantSHA := hex.EncodeToString(sum[:])
	res, err := c.Exec(ctx, fmt.Sprintf("sha256sum %s", target), 30*time.Second, 0)
	if err != nil {
		t.Fatalf("sha256sum exec: %v", err)
	}
	if !strings.Contains(res.Stdout, wantSHA) {
		t.Fatalf("server sha256 mismatch: out=%q want %q", res.Stdout, wantSHA)
	}

	// 2. overwrite truncates: write a SHORTER payload over the same path.
	short := []byte("plan-33 overwrite")
	if err := c.WriteFile(ctx, target, bytes.NewReader(short)); err != nil {
		t.Fatalf("overwrite WriteFile: %v", err)
	}
	res, err = c.Exec(ctx, fmt.Sprintf("wc -c < %s", target), 30*time.Second, 0)
	if err != nil || strings.TrimSpace(res.Stdout) != fmt.Sprint(len(short)) {
		t.Fatalf("overwrite size: err=%v out=%q want %d", err, res.Stdout, len(short))
	}
}
