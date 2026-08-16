package sshbroker

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/testsshd"
)

// TestUpload verifies single-file + recursive-dir upload over SFTP, plus the §6
// total-byte cap. The in-process testsshd (with its sftp subsystem, enabled in
// Plan 5e T1) serves the host FS, so Upload's local-read + SFTP-put and the
// verify Download's SFTP-get all hit the same t.TempDir() paths. Mirror of
// download_test.go's helper pattern.
func TestUpload(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	c := connectTest(t, addr, hk)
	defer c.Close()

	// Build a local dir tree on the host FS: tmp/a.txt + tmp/sub/b.txt.
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "a.txt"), []byte("file-a\n"), 0644); err != nil {
		t.Fatalf("setup a.txt: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(tmp, "sub"), 0755); err != nil {
		t.Fatalf("setup sub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "sub", "b.txt"), []byte("file-b\n"), 0644); err != nil {
		t.Fatalf("setup b.txt: %v", err)
	}

	// Single-file upload: round-trip through Download to verify content landed,
	// and check UploadResult accounting (Files=1, Bytes=len, Truncated=false).
	remoteFile := filepath.Join(t.TempDir(), "up-single.txt")
	res, err := c.Upload(context.Background(), filepath.Join(tmp, "a.txt"), remoteFile, 0)
	if err != nil {
		t.Fatalf("single Upload: %v", err)
	}
	if res.Files != 1 || res.Bytes != int64(len("file-a\n")) || res.Truncated {
		t.Fatalf("single result = %+v, want {Files:1 Bytes:%d Truncated:false}", res, len("file-a\n"))
	}
	got, err := c.Download(context.Background(), remoteFile, 0)
	if err != nil {
		t.Fatalf("verify Download: %v", err)
	}
	if got.Content != "file-a\n" {
		t.Fatalf("single round-trip content = %q, want %q", got.Content, "file-a\n")
	}

	// Dir upload (recursive) — remote root under a fresh temp dir.
	remoteDir := filepath.Join(t.TempDir(), "up-dir")
	res, err = c.Upload(context.Background(), tmp, remoteDir, 0)
	if err != nil {
		t.Fatalf("dir Upload: %v", err)
	}
	if res.Files != 2 { // a.txt + sub/b.txt
		t.Fatalf("dir Files=%d, want 2 (res=%+v)", res.Files, res)
	}
	if res.Truncated {
		t.Fatalf("dir Truncated=true, want false (res=%+v)", res)
	}
	// Verify both files landed at their preserved relative paths.
	if g, err := c.Download(context.Background(), filepath.Join(remoteDir, "a.txt"), 0); err != nil || g.Content != "file-a\n" {
		t.Fatalf("dir a.txt: err=%v content=%q", err, g.Content)
	}
	if g, err := c.Download(context.Background(), filepath.Join(remoteDir, "sub", "b.txt"), 0); err != nil || g.Content != "file-b\n" {
		t.Fatalf("dir sub/b.txt: err=%v content=%q", err, g.Content)
	}
	// Cross-platform regression-guard (Plan 6 T2-review fix): the remote SFTP
	// target must be POSIX (forward-slash) regardless of broker host OS, so a
	// Windows broker host uploading to a Linux server preserves the dir tree
	// (the primary deployment). Assert the nested file is reachable at the
	// POSIX path — path.Join, NOT filepath.Join — so the assertion is
	// OS-independent. On a Linux server this fails if uploadDir ever regresses
	// to filepath.Join (backslash targets collapse into one weird nested name,
	// not the expected dir tree, so the forward-slash Download misses).
	posixB := path.Join(remoteDir, "sub", "b.txt")
	if g, err := c.Download(context.Background(), posixB, 0); err != nil || g.Content != "file-b\n" {
		t.Fatalf("POSIX-path regression-guard sub/b.txt (%q): err=%v content=%q", posixB, err, g.Content)
	}

	// Cap: maxBytes=3 < total=14 → Truncated=true, no error returned (the flag is
	// the signal). The walk halts after the first file exceeds the cap, so Files
	// is bounded (1 here: a.txt is uploaded fully, then the walk stops).
	res, err = c.Upload(context.Background(), tmp, filepath.Join(t.TempDir(), "up-cap"), 3)
	if err != nil {
		t.Fatalf("capped Upload: %v", err)
	}
	if !res.Truncated {
		t.Fatalf("capped: Truncated=false, want true (res=%+v)", res)
	}
	if res.Files == 0 {
		t.Fatalf("capped: Files=0, want at least the file that tripped the cap (res=%+v)", res)
	}
}

// mcpUploadCap mirrors mcpserver.MaxOutputBytes (1 MiB — the §6 cap the MCP
// boundary passes Upload; see UploadForProfile). A local constant, not an
// import: package sshbroker's tests cannot import mcpserver (mcpserver →
// sshbroker is an import cycle). The conformance boundary-cap subtest tracks
// the real constant; this unit test pins the walk-halt semantics at the
// documented cap size.
const mcpUploadCap = int64(1 << 20)

// TestUploadCapWalkHaltTwoFiles (Plan 22 T3 hygiene): the §6 cap halts the
// walk BETWEEN files. A dir whose FIRST file (lexical walk order) already
// exceeds the cap lands that file COMPLETE (per-file atomic — res.Files counts
// it), and the second file is never started: absent remotely, not counted.
// The in-process testsshd serves the host FS, so "remote" stat = os.Stat.
func TestUploadCapWalkHaltTwoFiles(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	c := connectTest(t, addr, hk)
	defer c.Close()

	// a-over.bin sorts FIRST in filepath.Walk's lexical order → trips the cap;
	// z-small.txt (1 KiB) is visited next and must never start.
	src := t.TempDir()
	over := bytes.Repeat([]byte("x"), int(mcpUploadCap)+1) // exactly cap+1 bytes
	if err := os.WriteFile(filepath.Join(src, "a-over.bin"), over, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "z-small.txt"), bytes.Repeat([]byte("s"), 1<<10), 0o644); err != nil {
		t.Fatal(err)
	}

	remoteDir := filepath.Join(t.TempDir(), "up-halt")
	res, err := c.Upload(context.Background(), src, remoteDir, mcpUploadCap)
	if err != nil {
		t.Fatalf("capped dir Upload: %v", err)
	}
	if res.Files != 1 || !res.Truncated {
		t.Fatalf("result = %+v, want {Files:1 Truncated:true} (the over-cap file landed + counted, the walk halted)", res)
	}
	if res.Bytes != mcpUploadCap+1 {
		t.Fatalf("Bytes = %d, want %d (honest total: the over-cap file landed complete, nothing else ran)", res.Bytes, mcpUploadCap+1)
	}
	// Second file ABSENT remotely (walk-halt — no new file started post-cap).
	if _, err := os.Stat(filepath.Join(remoteDir, "z-small.txt")); !os.IsNotExist(err) {
		t.Fatalf("second file must be absent remotely (walk halted between files), stat err=%v", err)
	}
	// The cap tripper landed COMPLETE (the cap never truncates a stream mid-file).
	if fi, err := os.Stat(filepath.Join(remoteDir, "a-over.bin")); err != nil || fi.Size() != int64(len(over)) {
		t.Fatalf("over-cap file must land complete (%d bytes): fi=%v err=%v", len(over), fi, err)
	}
}

// TestClientMkdirAll verifies the broker MkdirAll helper creates nested parents
// over SFTP. It is the primitive UploadForProfile uses to ensure remotePath's
// parent exists before a transfer (T1 carry: Client.Upload uses sftp.Mkdir +
// sftp.Create, both of which require the parent to pre-exist). Covers: nested
// multi-level creation, idempotency on an existing dir, the conflict case
// (path occupied by a regular file → error), and a composition check
// (MkdirAll-the-parent then Upload-into-it then Download-verify round-trip).
func TestClientMkdirAll(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	c := connectTest(t, addr, hk)
	defer c.Close()

	base := t.TempDir()

	// (1) Nested multi-level creation: base/a/b/c — none exist yet.
	nested := filepath.Join(base, "a", "b", "c")
	if err := c.MkdirAll(nested); err != nil {
		t.Fatalf("MkdirAll nested: %v", err)
	}
	// (2) Idempotency: re-MkdirAll on the now-existing dir is a no-op.
	if err := c.MkdirAll(nested); err != nil {
		t.Fatalf("MkdirAll on existing dir: %v", err)
	}
	// (3) Composition: Upload a file INTO the freshly-created dir, then verify
	// via Download — proves MkdirAll unblocks a subsequent Client.Upload (the
	// exact composition UploadForProfile performs at the MCP boundary).
	file := filepath.Join(nested, "marker.txt")
	local := filepath.Join(t.TempDir(), "src.txt")
	if err := os.WriteFile(local, []byte("mkdir-ok\n"), 0644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	if _, err := c.Upload(context.Background(), local, file, 0); err != nil {
		t.Fatalf("Upload into MkdirAll'd dir: %v", err)
	}
	g, err := c.Download(context.Background(), file, 0)
	if err != nil {
		t.Fatalf("verify Download: %v", err)
	}
	if g.Content != "mkdir-ok\n" {
		t.Fatalf("round-trip = %q, want %q", g.Content, "mkdir-ok\n")
	}
	// (4) Conflict: MkdirAll on a path occupied by a regular file → error.
	if err := c.MkdirAll(file); err == nil {
		t.Fatal("MkdirAll on a regular file: want error, got nil")
	}
}

// TestUploadCancelContext proves a cancelled ctx makes Upload return
// context.Canceled promptly (the watchdog closes the sftp client so the in-flight
// sftp op errors and the walk propagates). Pre-cancelled for determinism (see
// TestDownloadCancelContext's rationale); the half-written remote file is left
// as-is (mirrors scp -r interrupted).
func TestUploadCancelContext(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	c := connectTest(t, addr, hk)

	tmp := t.TempDir()
	src := filepath.Join(tmp, "big.bin")
	if err := os.WriteFile(src, []byte(strings.Repeat("x", 1<<20)), 0644); err != nil {
		t.Fatalf("setup write: %v", err)
	}
	remote := filepath.Join(t.TempDir(), "up-cancel.bin")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	res, err := c.Upload(ctx, src, remote, 0)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if res.Truncated {
		t.Fatal("Truncated=true on cancel, want false (cap not hit)")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Upload took %v on pre-cancelled ctx, want < 2s", elapsed)
	}
}
