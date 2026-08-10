package sshbroker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	got, err := c.Download(context.Background(), remote, 0)
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
	got, _ = c.Download(context.Background(), remote, 5)
	if !got.Truncated || got.Content != want[:5] {
		t.Fatalf("capped: Truncated=%v content=%q, want true/%q", got.Truncated, got.Content, want[:5])
	}
	if got.Bytes != int64(len(want)) {
		t.Fatalf("capped Bytes=%d, want full size %d (Bytes reports true size even when capped)", got.Bytes, len(want))
	}
}

// TestDownloadCancelContext proves a cancelled ctx makes Download return
// context.Canceled promptly (the watchdog closes the sftp file so io.Copy
// aborts) with Truncated=false. We PRE-CANCEL rather than race a mid-transfer
// cancel: in-process sftp over loopback+SSH is fast enough that a 1 MiB file can
// finish inside a 100 ms cancel window, making a partial-bytes assertion flaky.
// Pre-cancellation deterministically exercises the abort path; the mid-op abort
// mechanism is covered by TestExecCancelContext (whose testsshd Exec callback
// blocks on a fixed sleep and is reliably in-flight at cancel time).
func TestDownloadCancelContext(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	c := connectTest(t, addr, hk)

	remote := filepath.Join(t.TempDir(), "cancel.bin")
	if err := os.WriteFile(remote, []byte(strings.Repeat("x", 1<<20)), 0644); err != nil {
		t.Fatalf("setup write: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled — Download must return Canceled without reading the whole file

	start := time.Now()
	got, err := c.Download(ctx, remote, 0)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got.Truncated {
		t.Fatal("Truncated=true on cancel, want false (cap not hit; we were cancelled)")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Download took %v on pre-cancelled ctx, want < 2s", elapsed)
	}
}
