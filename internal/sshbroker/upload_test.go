package sshbroker

import (
	"os"
	"path/filepath"
	"testing"

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
	res, err := c.Upload(filepath.Join(tmp, "a.txt"), remoteFile, 0)
	if err != nil {
		t.Fatalf("single Upload: %v", err)
	}
	if res.Files != 1 || res.Bytes != int64(len("file-a\n")) || res.Truncated {
		t.Fatalf("single result = %+v, want {Files:1 Bytes:%d Truncated:false}", res, len("file-a\n"))
	}
	got, err := c.Download(remoteFile, 0)
	if err != nil {
		t.Fatalf("verify Download: %v", err)
	}
	if got.Content != "file-a\n" {
		t.Fatalf("single round-trip content = %q, want %q", got.Content, "file-a\n")
	}

	// Dir upload (recursive) — remote root under a fresh temp dir.
	remoteDir := filepath.Join(t.TempDir(), "up-dir")
	res, err = c.Upload(tmp, remoteDir, 0)
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
	if g, err := c.Download(filepath.Join(remoteDir, "a.txt"), 0); err != nil || g.Content != "file-a\n" {
		t.Fatalf("dir a.txt: err=%v content=%q", err, g.Content)
	}
	if g, err := c.Download(filepath.Join(remoteDir, "sub", "b.txt"), 0); err != nil || g.Content != "file-b\n" {
		t.Fatalf("dir sub/b.txt: err=%v content=%q", err, g.Content)
	}

	// Cap: maxBytes=3 < total=14 → Truncated=true, no error returned (the flag is
	// the signal). The walk halts after the first file exceeds the cap, so Files
	// is bounded (1 here: a.txt is uploaded fully, then the walk stops).
	res, err = c.Upload(tmp, filepath.Join(t.TempDir(), "up-cap"), 3)
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
	if _, err := c.Upload(local, file, 0); err != nil {
		t.Fatalf("Upload into MkdirAll'd dir: %v", err)
	}
	g, err := c.Download(file, 0)
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
