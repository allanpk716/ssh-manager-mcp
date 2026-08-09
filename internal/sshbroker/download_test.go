package sshbroker

import (
	"os"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/testsshd"
)

// TestDownload verifies the broker downloads a remote file over SFTP and that
// maxBytes capping + Truncated work. Requires the testsshd sftp subsystem
// (enabled in testsshd by this task).
func TestDownload(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	c := connectTest(t, addr, hk)
	defer c.Close()

	// Write a known file to a temp path on the host FS. The in-process testsshd
	// runs in this same OS process, so its sftp server (sftp.NewServer) serves
	// the same host filesystem — the path is readable by Download. (We cannot
	// set up the fixture via broker Exec: testsshd's Exec is a registered
	// callback, not a real shell, so it would not create a file on disk.)
	const want = "hello-sftp\nline2\nlast line marker\n"
	remote := filepath.Join(t.TempDir(), "dl.bin")
	if err := os.WriteFile(remote, []byte(want), 0644); err != nil {
		t.Fatalf("setup write: %v", err)
	}

	// Full download.
	got, err := c.Download(remote, 0)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if got.Content != want {
		t.Fatalf("content = %q, want %q", got.Content, want)
	}
	if got.Bytes != int64(len(want)) || got.Truncated {
		t.Fatalf("Bytes=%d Truncated=%v, want %d/false", got.Bytes, got.Truncated, len(want))
	}

	// Capped download: maxBytes < len(want) → Truncated=true, content is the prefix.
	got, _ = c.Download(remote, 5)
	if !got.Truncated || got.Content != want[:5] {
		t.Fatalf("capped: Truncated=%v content=%q, want true/%q", got.Truncated, got.Content, want[:5])
	}
	if got.Bytes != int64(len(want)) {
		t.Fatalf("capped Bytes=%d, want full size %d (Bytes reports true size even when capped)", got.Bytes, len(want))
	}
}
