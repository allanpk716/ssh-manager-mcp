package sshbroker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
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

	// Cap, cumulative layer: maxBytes=10, files 7+7=14 — EVERY file is within the
	// per-file bound (7 ≤ 10) but the total crosses the cap → Truncated=true, no
	// error (the flag is the signal). The trip happens during sub/b.txt's io.Copy
	// (7→14 > 10), which completes per-file-atomic; both files land. (Plan 23
	// changed the cap from 3 to 10: with cap=3 the first 7-byte file is now
	// REFUSED pre-flight — that layer has its own tests below.)
	res, err = c.Upload(context.Background(), tmp, filepath.Join(t.TempDir(), "up-cap"), 10)
	if err != nil {
		t.Fatalf("capped Upload: %v", err)
	}
	if !res.Truncated {
		t.Fatalf("capped: Truncated=false, want true (res=%+v)", res)
	}
	if res.Files != 2 {
		t.Fatalf("capped: Files=%d, want 2 (both files within the per-file cap landed; res=%+v)", res.Files, res)
	}
}

// mcpUploadCap mirrors mcpserver.MaxOutputBytes (1 MiB — the §6 cap the MCP
// boundary passes Upload; see UploadForProfile). A local constant, not an
// import: package sshbroker's tests cannot import mcpserver (mcpserver →
// sshbroker is an import cycle). The conformance boundary-cap subtest tracks
// the real constant; this unit test pins the walk-halt semantics at the
// documented cap size.
const mcpUploadCap = int64(1 << 20)

// TestUploadCapRefusesOverCapFileInDir (Plan 23 flip of Plan 22 T3's
// walk-halt case): a dir upload containing a file individually over the cap is
// REFUSED pre-flight — zero bytes of the over-cap file transfer, no remote file
// is created for it, and the refusal error (naming file/size/cap) propagates.
// The small file that sorted FIRST is complete before the refusal and REMAINS
// remotely (the "already-completed files remain" half of the error text). The
// cumulative walk-halt layer this test used to pin moved to
// TestUploadCapWalkHaltBetweenFiles (with all-files-≤cap fixtures, which the
// old a-over-first fixture can no longer exercise). The in-process testsshd
// serves the host FS, so "remote" stat = os.Stat.
func TestUploadCapRefusesOverCapFileInDir(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	c := connectTest(t, addr, hk)
	defer c.Close()

	// a-small.txt (1 KiB, sorts FIRST) uploads complete; z-over.bin (cap+1,
	// visited second) hits the per-file pre-flight and is refused.
	src := t.TempDir()
	small := bytes.Repeat([]byte("s"), 1<<10)
	if err := os.WriteFile(filepath.Join(src, "a-small.txt"), small, 0o644); err != nil {
		t.Fatal(err)
	}
	over := bytes.Repeat([]byte("x"), int(mcpUploadCap)+1) // exactly cap+1 bytes
	if err := os.WriteFile(filepath.Join(src, "z-over.bin"), over, 0o644); err != nil {
		t.Fatal(err)
	}

	remoteDir := filepath.Join(t.TempDir(), "up-refuse")
	res, err := c.Upload(context.Background(), src, remoteDir, mcpUploadCap)
	if err == nil {
		t.Fatalf("capped dir Upload: want refusal error, got nil (res=%+v)", res)
	}
	if !strings.Contains(err.Error(), "exceeds upload cap") {
		t.Fatalf("error must say \"exceeds upload cap\", got %q", err.Error())
	}
	if want := fmt.Sprintf("(%d bytes) exceeds upload cap %d", mcpUploadCap+1, mcpUploadCap); !strings.Contains(err.Error(), want) {
		t.Fatalf("error must carry size+cap evidence %q, got %q", want, err.Error())
	}
	if !strings.Contains(err.Error(), "z-over.bin") {
		t.Fatalf("error must name the refused file, got %q", err.Error())
	}
	// Honest partial accounting: the small file landed and counted; the refused
	// file contributed nothing. Truncated stays false — the cumulative layer
	// never tripped (total never crossed the cap mid-copy).
	if res.Files != 1 || res.Bytes != int64(len(small)) || res.Truncated {
		t.Fatalf("result = %+v, want {Files:1 Bytes:%d Truncated:false} (small file complete, refused file zero)", res, len(small))
	}
	// The small file REMAINS remotely at full size.
	if fi, err := os.Stat(filepath.Join(remoteDir, "a-small.txt")); err != nil || fi.Size() != int64(len(small)) {
		t.Fatalf("small file must remain complete remotely (%d bytes): fi=%v err=%v", len(small), fi, err)
	}
	// The over-cap file is ABSENT remotely — refused before transfer, so not
	// even a zero-byte remote file was created.
	if _, err := os.Stat(filepath.Join(remoteDir, "z-over.bin")); !os.IsNotExist(err) {
		t.Fatalf("over-cap file must be absent remotely (refused pre-transfer), stat err=%v", err)
	}
}

// TestUploadCapWalkHaltBetweenFiles preserves the CUMULATIVE layer of the §6 cap
// (Plan 23: this layer is unchanged, but the old fixture — a first file already
// over the cap — now belongs to the refusal layer, so this test re-pins the
// walk-halt with every file within the per-file bound). Three 4 KiB files,
// cap=6000: file1 lands (total 4096); file2's pre-flight passes (4096 ≤ 6000)
// and its copy trips the cumulative cap mid-stream (total 8192 > 6000) yet
// lands COMPLETE (per-file atomic); the walk halts — file3 never starts.
func TestUploadCapWalkHaltBetweenFiles(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	c := connectTest(t, addr, hk)
	defer c.Close()

	src := t.TempDir()
	for _, n := range []string{"f1.bin", "f2.bin", "f3.bin"} {
		if err := os.WriteFile(filepath.Join(src, n), bytes.Repeat([]byte("y"), 4096), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	remoteDir := filepath.Join(t.TempDir(), "up-halt")
	res, err := c.Upload(context.Background(), src, remoteDir, 6000)
	if err != nil {
		t.Fatalf("cumulative-cap dir Upload: %v (truncation is the flag, not an error)", err)
	}
	if !res.Truncated {
		t.Fatalf("Truncated=false, want true (res=%+v)", res)
	}
	if res.Files != 2 {
		t.Fatalf("Files=%d, want 2 (walk halted before f3.bin; res=%+v)", res.Files, res)
	}
	if res.Bytes != 2*4096 {
		t.Fatalf("Bytes=%d, want %d (honest total: two complete files)", res.Bytes, 2*4096)
	}
	// f3 never started → absent remotely.
	if _, err := os.Stat(filepath.Join(remoteDir, "f3.bin")); !os.IsNotExist(err) {
		t.Fatalf("f3.bin must be absent remotely (walk halted between files), stat err=%v", err)
	}
	// f2 (the cumulative tripwire) landed COMPLETE — per-file atomic survives.
	if fi, err := os.Stat(filepath.Join(remoteDir, "f2.bin")); err != nil || fi.Size() != 4096 {
		t.Fatalf("f2.bin must land complete (4096 bytes): fi=%v err=%v", fi, err)
	}
}

// TestUploadCapBoundarySingleFile pins the per-file boundary on the single-file
// Upload path: STRICTLY greater than the cap → refused pre-transfer (error names
// file/size/cap; zero remote bytes — not even a created empty file; zero-value
// UploadResult). Exactly == cap → allowed, lands complete, Truncated=false.
func TestUploadCapBoundarySingleFile(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	c := connectTest(t, addr, hk)
	defer c.Close()

	src := t.TempDir()
	overPath := filepath.Join(src, "over.bin")
	if err := os.WriteFile(overPath, bytes.Repeat([]byte("x"), int(mcpUploadCap)+1), 0o644); err != nil {
		t.Fatal(err)
	}

	// cap+1 → refused, zero bytes transferred.
	remoteOver := filepath.Join(t.TempDir(), "up-over.bin")
	res, err := c.Upload(context.Background(), overPath, remoteOver, mcpUploadCap)
	if err == nil {
		t.Fatalf("over-cap single Upload: want refusal error, got nil (res=%+v)", res)
	}
	if !strings.Contains(err.Error(), "exceeds upload cap") || !strings.Contains(err.Error(), "over.bin") {
		t.Fatalf("refusal error must name file + cap, got %q", err.Error())
	}
	if res.Files != 0 || res.Bytes != 0 || res.Truncated {
		t.Fatalf("refused result = %+v, want zero-value (nothing transferred)", res)
	}
	if _, serr := os.Stat(remoteOver); !os.IsNotExist(serr) {
		t.Fatalf("refused file must not exist remotely (zero bytes, not a 0-byte file), stat err=%v", serr)
	}

	// Exactly == cap → allowed, complete, honest accounting.
	atCapPath := filepath.Join(src, "atcap.bin")
	if err := os.WriteFile(atCapPath, bytes.Repeat([]byte("x"), int(mcpUploadCap)), 0o644); err != nil {
		t.Fatal(err)
	}
	remoteAtCap := filepath.Join(t.TempDir(), "up-atcap.bin")
	res, err = c.Upload(context.Background(), atCapPath, remoteAtCap, mcpUploadCap)
	if err != nil {
		t.Fatalf("at-cap single Upload: %v", err)
	}
	if res.Files != 1 || res.Bytes != mcpUploadCap || res.Truncated {
		t.Fatalf("at-cap result = %+v, want {Files:1 Bytes:%d Truncated:false}", res, mcpUploadCap)
	}
	if fi, serr := os.Stat(remoteAtCap); serr != nil || fi.Size() != mcpUploadCap {
		t.Fatalf("at-cap remote size: fi=%v err=%v, want %d", fi, serr, mcpUploadCap)
	}
}

// TestUploadCapRefusesSymlinkToOverCapFileInDir (Plan 24): uploadDir's pre-flight
// used Walk's lstat FileInfo — a symlink reports its OWN tiny size, slipping
// under the per-file cap, while uploadFile FOLLOWS the link and transfers the
// TARGET's bytes (check and behavior disagreed; the gate was bypassable). With
// the fix a symlink entry under an armed cap is re-stat'ed with follow, so the
// gate sees the target's real size: a symlink to a cap+1 file (target OUTSIDE
// the tree, so only the link is walked) is refused pre-transfer — the error
// names the symlink path + the TARGET's size + cap, zero bytes of the target
// move, no remote file for the link, and the small file that sorted first
// remains complete. Windows note: os.Symlink for a FILE link works without
// admin when Developer Mode is on; otherwise this test skips with a message.
func TestUploadCapRefusesSymlinkToOverCapFileInDir(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	c := connectTest(t, addr, hk)
	defer c.Close()

	// a-small.txt (1 KiB, sorts FIRST) uploads complete; z-link.bin → big.bin
	// (cap+1 bytes, outside src) is visited second and refused on the target's
	// real size.
	src := t.TempDir()
	small := bytes.Repeat([]byte("s"), 1<<10)
	if err := os.WriteFile(filepath.Join(src, "a-small.txt"), small, 0o644); err != nil {
		t.Fatal(err)
	}
	bigPath := filepath.Join(t.TempDir(), "big-target.bin")
	if err := os.WriteFile(bigPath, bytes.Repeat([]byte("x"), int(mcpUploadCap)+1), 0o644); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(src, "z-link.bin")
	if err := os.Symlink(bigPath, linkPath); err != nil {
		t.Skipf("os.Symlink failed on this host (%v) — Windows needs Developer Mode/admin for symlink creation; skipping symlink cap-gate test", err)
	}

	remoteDir := filepath.Join(t.TempDir(), "up-symlink-refuse")
	res, err := c.Upload(context.Background(), src, remoteDir, mcpUploadCap)
	if err == nil {
		t.Fatalf("symlink-to-over-cap dir Upload: want refusal error, got nil (res=%+v)", res)
	}
	if !strings.Contains(err.Error(), "exceeds upload cap") {
		t.Fatalf("error must say \"exceeds upload cap\", got %q", err.Error())
	}
	if want := fmt.Sprintf("(%d bytes) exceeds upload cap %d", mcpUploadCap+1, mcpUploadCap); !strings.Contains(err.Error(), want) {
		t.Fatalf("error must carry the TARGET's size+cap evidence %q, got %q", want, err.Error())
	}
	if !strings.Contains(err.Error(), "z-link.bin") {
		t.Fatalf("error must name the symlink path, got %q", err.Error())
	}
	// Honest partial accounting: only the small file moved; the target's bytes
	// contributed nothing.
	if res.Files != 1 || res.Bytes != int64(len(small)) || res.Truncated {
		t.Fatalf("result = %+v, want {Files:1 Bytes:%d Truncated:false} (small complete, target zero)", res, len(small))
	}
	// The small file REMAINS complete remotely.
	if fi, err := os.Stat(filepath.Join(remoteDir, "a-small.txt")); err != nil || fi.Size() != int64(len(small)) {
		t.Fatalf("small file must remain complete remotely (%d bytes): fi=%v err=%v", len(small), fi, err)
	}
	// No remote file for the link — refused before transfer, so not even a
	// zero-byte remote file was created.
	if _, err := os.Stat(filepath.Join(remoteDir, "z-link.bin")); !os.IsNotExist(err) {
		t.Fatalf("symlink's remote file must be absent (refused pre-transfer), stat err=%v", err)
	}
}

// TestUploadBrokenSymlinkInDirErrors (Plan 24): a dangling symlink under an
// armed cap fails the follow-stat in the walk callback and the error propagates
// naming the symlink path (pre-fix it slipped past the lstat-size gate and died
// later inside uploadFile's open — the names-the-path contract is pinned either
// way). Skips when the host cannot create symlinks.
func TestUploadBrokenSymlinkInDirErrors(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	c := connectTest(t, addr, hk)
	defer c.Close()

	src := t.TempDir()
	linkPath := filepath.Join(src, "dangling.lnk")
	if err := os.Symlink(filepath.Join(t.TempDir(), "never-created.bin"), linkPath); err != nil {
		t.Skipf("os.Symlink failed on this host (%v) — Windows needs Developer Mode/admin for symlink creation; skipping broken-symlink test", err)
	}

	remoteDir := filepath.Join(t.TempDir(), "up-dangling")
	res, err := c.Upload(context.Background(), src, remoteDir, mcpUploadCap)
	if err == nil {
		t.Fatalf("broken-symlink dir Upload: want error, got nil (res=%+v)", res)
	}
	if !strings.Contains(err.Error(), "dangling.lnk") {
		t.Fatalf("error must name the broken symlink path, got %q", err.Error())
	}
}

// TestUploadSymlinkToSmallFileFollowsTarget (Plan 24): the follow semantics the
// gate now aligns with must be preserved — a symlink to a file WITHIN the cap
// (gate armed) uploads the TARGET's content under the link's remote name,
// exactly as scp -r does. Skips when the host cannot create symlinks.
func TestUploadSymlinkToSmallFileFollowsTarget(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	c := connectTest(t, addr, hk)
	defer c.Close()

	// Target lives OUTSIDE src so only the link is walked — the one uploaded
	// file proves its content came from following the link.
	content := "symlink-target-content\n"
	target := filepath.Join(t.TempDir(), "target.txt")
	if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	if err := os.Symlink(target, filepath.Join(src, "link.txt")); err != nil {
		t.Skipf("os.Symlink failed on this host (%v) — Windows needs Developer Mode/admin for symlink creation; skipping symlink-follow test", err)
	}

	remoteDir := filepath.Join(t.TempDir(), "up-symlink-follow")
	res, err := c.Upload(context.Background(), src, remoteDir, mcpUploadCap)
	if err != nil {
		t.Fatalf("symlink-to-small dir Upload: %v", err)
	}
	if res.Files != 1 || res.Bytes != int64(len(content)) || res.Truncated {
		t.Fatalf("result = %+v, want {Files:1 Bytes:%d Truncated:false}", res, len(content))
	}
	g, err := c.Download(context.Background(), filepath.Join(remoteDir, "link.txt"), 0)
	if err != nil || g.Content != content {
		t.Fatalf("link round-trip: err=%v content=%q, want %q", err, g.Content, content)
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

// TestUploadDirSymlinkRootResolved (Plan 26): a symlink/junction used AS the
// upload root is resolved to its target — Upload's os.Stat already follows the
// link (says "dir"), but filepath.Walk lstats the root and would misclassify
// it as a file. EvalSymlinks at uploadDir entry makes root handling follow the
// operator's intent. Windows lane exercises this via a junction (mklink /J,
// no privilege needed); unix via os.Symlink (skip when unprivileged).
func TestUploadDirSymlinkRootResolved(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	c := connectTest(t, addr, hk)
	defer c.Close()

	real := t.TempDir()
	if err := os.WriteFile(filepath.Join(real, "a.txt"), []byte("root-link\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link-root")
	if err := makeDirLink(t, link, real); err != nil {
		t.Skipf("dir link creation failed on this host (%v); skipping", err)
	}

	remoteDir := filepath.Join(t.TempDir(), "up-link-root")
	res, err := c.Upload(context.Background(), link, remoteDir, 0)
	if err != nil {
		t.Fatalf("symlink-root Upload: %v", err)
	}
	if res.Files != 1 || res.Bytes != int64(len("root-link\n")) {
		t.Fatalf("result = %+v, want {Files:1 Bytes:%d}", res, len("root-link\n"))
	}
	g, err := c.Download(context.Background(), filepath.Join(remoteDir, "a.txt"), 0)
	if err != nil || g.Content != "root-link\n" {
		t.Fatalf("round-trip: err=%v content=%q", err, g.Content)
	}
}

// makeDirLink creates link pointing at dir target: junction on Windows
// (privilege-free), symlink elsewhere.
func makeDirLink(t *testing.T, link, dir string) error {
	t.Helper()
	if runtime.GOOS == "windows" {
		out, err := exec.Command("cmd", "/c", "mklink", "/J", link, dir).CombinedOutput()
		if err != nil {
			return fmt.Errorf("mklink /J: %v: %s", err, out)
		}
		return nil
	}
	return os.Symlink(dir, link)
}

// TestUploadDirNestedSymlinkedDirRefused (Plan 26): a symlink→directory
// nested inside the upload root is REFUSED with a named error — pre-fix it
// fell into the file branch and died inside uploadFile's open/read with a
// misleading platform-dependent error. Refusal is cap-INDEPENDENT (armed here).
func TestUploadDirNestedSymlinkedDirRefused(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	c := connectTest(t, addr, hk)
	defer c.Close()

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := makeDirLink(t, filepath.Join(src, "z-link"), t.TempDir()); err != nil {
		t.Skipf("dir link creation failed on this host (%v); skipping", err)
	}

	remoteDir := filepath.Join(t.TempDir(), "up-nested-link")
	_, err := c.Upload(context.Background(), src, remoteDir, mcpUploadCap)
	if err == nil || !strings.Contains(err.Error(), "symlinked directory not uploaded") || !strings.Contains(err.Error(), "z-link") {
		t.Fatalf("want named refusal naming z-link, got: %v", err)
	}
	// Walk order is lexical: a.txt (< z-link) is uploaded BEFORE the refusal —
	// already-completed files remain (same contract as cap refusal, Plan 23).
	if g, derr := c.Download(context.Background(), filepath.Join(remoteDir, "a.txt"), 0); derr != nil || g.Content != "first\n" {
		t.Fatalf("a.txt must remain uploaded (derr=%v content=%q)", derr, g.Content)
	}
}

// TestUploadDirNestedSymlinkedDirRefusedNoCap: same refusal with cap==0 —
// the dir-symlink check must not live under the cap-armed branch.
func TestUploadDirNestedSymlinkedDirRefusedNoCap(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	c := connectTest(t, addr, hk)
	defer c.Close()

	src := t.TempDir()
	if err := makeDirLink(t, filepath.Join(src, "z-link"), t.TempDir()); err != nil {
		t.Skipf("dir link creation failed on this host (%v); skipping", err)
	}
	if _, err := c.Upload(context.Background(), src, filepath.Join(t.TempDir(), "up"), 0); err == nil || !strings.Contains(err.Error(), "symlinked directory not uploaded") {
		t.Fatalf("cap==0 must still refuse, got: %v", err)
	}
}

// TestUploadJunctionNestedRefused_windows: the real-world Windows case —
// a junction inside the upload tree (OneDrive / dev-drive junctions).
// Windows-only (build-tag free: skips elsewhere).
func TestUploadJunctionNestedRefused_windows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows junction test")
	}
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	c := connectTest(t, addr, hk)
	defer c.Close()

	src := t.TempDir()
	if err := makeDirLink(t, filepath.Join(src, "z-junc"), t.TempDir()); err != nil {
		t.Skipf("junction creation failed (%v); skipping", err)
	}
	if _, err := c.Upload(context.Background(), src, filepath.Join(t.TempDir(), "up"), 0); err == nil || !strings.Contains(err.Error(), "symlinked directory not uploaded") {
		t.Fatalf("junction must be refused like a dir symlink, got: %v", err)
	}
}

// TestUploadDirUnderJunctionAncestor (Plan 26 fix): a real directory root
// whose path merely TRAVERSES a junction/symlink ancestor must upload fine —
// EvalSymlinks errors on such paths on Windows (go1.25.8), and the old
// unconditional return regressed these previously-working uploads. Walk and
// the follow-stat both handle a traversed junction transparently.
func TestUploadDirUnderJunctionAncestor(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	c := connectTest(t, addr, hk)
	defer c.Close()

	real := t.TempDir()
	sub := filepath.Join(real, "app")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "a.txt"), []byte("under-ancestor\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "anc-link")
	if err := makeDirLink(t, link, real); err != nil {
		t.Skipf("dir link creation failed on this host (%v); skipping", err)
	}

	remoteDir := filepath.Join(t.TempDir(), "up-under-anc")
	res, err := c.Upload(context.Background(), filepath.Join(link, "app"), remoteDir, 0)
	if err != nil {
		t.Fatalf("upload under link ancestor: %v", err)
	}
	if res.Files != 1 || res.Bytes != int64(len("under-ancestor\n")) {
		t.Fatalf("result = %+v, want {Files:1 Bytes:%d}", res, len("under-ancestor\n"))
	}
	g, err := c.Download(context.Background(), filepath.Join(remoteDir, "a.txt"), 0)
	if err != nil || g.Content != "under-ancestor\n" {
		t.Fatalf("round-trip: err=%v content=%q", err, g.Content)
	}
}
