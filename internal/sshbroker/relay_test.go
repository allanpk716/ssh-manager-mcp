package sshbroker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/testsshd"

	"github.com/pkg/sftp"
)

// relayTestSC starts an in-process testsshd, connects a broker Client, and
// opens the relay SFTP client through it — the common fixture of every relay
// primitive test. The in-process server serves the host FS, so "remote"
// verification is plain os.ReadFile/os.Stat on the same paths (upload_test.go
// precedent). Cleanup order (LIFO via t.Cleanup): sftp client → ssh client →
// sshd listener (matches the defer-cleanup-before-cli-close LIFO rule).
func relayTestSC(t *testing.T) (*Client, *sftp.Client) {
	t.Helper()
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	t.Cleanup(cleanup)
	c := connectTest(t, addr, hk)
	sc, err := c.RelaySFTP()
	if err != nil {
		t.Fatalf("RelaySFTP: %v", err)
	}
	t.Cleanup(func() { sc.Close() })
	return c, sc
}

// relayTestChunk is the chunk grid of the relay primitive tests — small enough
// for a fast suite, large enough to span many sftp packets.
const relayTestChunk = 64 << 10

// relayFixture returns n bytes of deterministic pseudo-random data (fixed
// seed — every test run sees the same bytes, so hash expectations hold).
func relayFixture(n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(47)).Read(b) // deterministic fixture, not crypto
	return b
}

// TestRelaySFTPClient pins the thin constructor (spec §2 connection-ownership
// discipline): RelaySFTP yields a working sftp client over the established
// connection — Stat sees the host FS with exact size, a missing path reports
// the os.ErrNotExist class (what RelayAvailable's ancestor walk and
// RelayReadManifest's state detection key on) — and lifetime (Close) sits with
// the caller, exercised here by relayTestSC's cleanup.
func TestRelaySFTPClient(t *testing.T) {
	_, sc := relayTestSC(t)

	p := filepath.Join(t.TempDir(), "stat.txt")
	if err := os.WriteFile(p, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := sc.Stat(p)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Size() != 10 {
		t.Fatalf("Stat size = %d, want 10", fi.Size())
	}
	if _, err := sc.Lstat(filepath.Join(t.TempDir(), "missing.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing Lstat err = %v, want os.ErrNotExist class", err)
	}
}

// TestRelayPosixRenameOK: testsshd's sftp server advertises
// posix-rename@openssh.com (pkg/sftp server constant), so the zero-IO probe
// returns true; a live rename then proves the extension actually works against
// this server class — INCLUDING the overwrite-of-existing semantics the atomic
// manifest write (§3) and the commit rename depend on.
func TestRelayPosixRenameOK(t *testing.T) {
	c, sc := relayTestSC(t)

	if !c.RelayPosixRenameOK(sc) {
		t.Fatal("RelayPosixRenameOK = false on testsshd, want true (server advertises posix-rename@openssh.com)")
	}
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(a, []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("OLD\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sc.PosixRename(a, b); err != nil {
		t.Fatalf("PosixRename: %v", err)
	}
	if _, err := os.Stat(a); !os.IsNotExist(err) {
		t.Fatalf("source must be gone after rename, stat err=%v", err)
	}
	if got, err := os.ReadFile(b); err != nil || string(got) != "new\n" {
		t.Fatalf("overwrite-rename readback: err=%v content=%q, want %q", err, got, "new\n")
	}
}

// TestRelayCopyChunkByteExactHoles: the §2 block pipeline is byte-exact — each
// chunk lands at ITS absolute offset (offset Seek + exact-length copy), and a
// SKIPPED chunk (the resume shape: an already-done index) leaves its region a
// zero hole while later chunks still land at their absolute offsets. Readback
// is via the host FS (the in-process server serves it).
func TestRelayCopyChunkByteExactHoles(t *testing.T) {
	_, sc := relayTestSC(t)

	const chunk = relayTestChunk
	srcData := relayFixture(chunk*3 + chunk/2) // 3.5 chunks
	srcPath := filepath.Join(t.TempDir(), "src.bin")
	if err := os.WriteFile(srcPath, srcData, 0o644); err != nil {
		t.Fatal(err)
	}
	dstPath := filepath.Join(t.TempDir(), "dst.bin")

	src, err := sc.Open(srcPath)
	if err != nil {
		t.Fatalf("open src: %v", err)
	}
	defer src.Close()
	dst, err := sc.OpenFile(dstPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		t.Fatalf("open dst: %v", err)
	}

	// Resume shape: chunks 0 and 1 need (re)transfer, chunk 2 is already done
	// (skipped — its region must stay untouched), chunk 3 is a PARTIAL final
	// chunk (half the grid — exact-length, not a grid round-up).
	for _, tc := range []struct {
		off, n int64
	}{{0, chunk}, {chunk, chunk}, {3 * chunk, chunk / 2}} {
		written, err := RelayCopyChunk(context.Background(), src, dst, tc.off, tc.n, nil)
		if err != nil {
			t.Fatalf("chunk at %d: %v", tc.off, err)
		}
		if written != tc.n {
			t.Fatalf("chunk at %d: written=%d, want %d", tc.off, written, tc.n)
		}
	}
	if err := dst.Close(); err != nil {
		t.Fatalf("close dst: %v", err)
	}

	got, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(got)) != int64(len(srcData)) {
		t.Fatalf("dst size = %d, want %d (3.5 chunks — no boundary overshoot)", len(got), len(srcData))
	}
	if !bytes.Equal(got[0:chunk], srcData[0:chunk]) {
		t.Fatal("chunk 0 mismatch")
	}
	if !bytes.Equal(got[chunk:2*chunk], srcData[chunk:2*chunk]) {
		t.Fatal("chunk 1 mismatch")
	}
	if !bytes.Equal(got[3*chunk:], srcData[3*chunk:]) {
		t.Fatal("chunk 3 (partial final) mismatch")
	}
	if !bytes.Equal(got[2*chunk:3*chunk], make([]byte, chunk)) {
		t.Fatal("skipped chunk 2 region must be a zero hole (resume: done and pending regions untouched)")
	}
}

// TestRelayCopyChunkShortReadErrors: the exact-length discipline (§2) — a
// source shorter than n (the real-world cause: source shrank/truncated
// mid-flight) must ERROR with the honest written count, not silently mark the
// chunk done. The partial bytes that DID move land in dst — the engine simply
// never records the chunk in the manifest.
func TestRelayCopyChunkShortReadErrors(t *testing.T) {
	_, sc := relayTestSC(t)

	srcData := []byte("0123456789") // 10 bytes
	srcPath := filepath.Join(t.TempDir(), "short.bin")
	if err := os.WriteFile(srcPath, srcData, 0o644); err != nil {
		t.Fatal(err)
	}
	dstPath := filepath.Join(t.TempDir(), "short-dst.bin")
	src, err := sc.Open(srcPath)
	if err != nil {
		t.Fatalf("open src: %v", err)
	}
	defer src.Close()
	dst, err := sc.OpenFile(dstPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		t.Fatalf("open dst: %v", err)
	}

	// Whole-file shape: ask for 100 of 10 → error wrapping io.EOF, written=10.
	written, err := RelayCopyChunk(context.Background(), src, dst, 0, 100, nil)
	if err == nil {
		t.Fatal("short source: want error, got nil")
	}
	if written != 10 {
		t.Fatalf("written=%d, want 10 (honest count)", written)
	}
	if !errors.Is(err, io.EOF) {
		t.Fatalf("short-read error must wrap io.EOF, got %v", err)
	}
	// Mid-file shape: offset 5, ask 10 → only 5 bytes remain after offset.
	written, err = RelayCopyChunk(context.Background(), src, dst, 5, 10, nil)
	if err == nil {
		t.Fatal("mid-file short source: want error, got nil")
	}
	if written != 5 {
		t.Fatalf("mid-file written=%d, want 5", written)
	}

	if err := dst.Close(); err != nil {
		t.Fatalf("close dst: %v", err)
	}
	got, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, srcData) {
		t.Fatalf("dst after short reads = %q, want the moved bytes at their offsets (%q)", got, srcData)
	}
}

// TestRelayCopyChunkLastChunkEOF: after the FINAL chunk's exact copy the
// source sits precisely at EOF — the engine's §2 trailing confirm ("read one
// more byte, expect EOF") keys on exactly this stream position, so the
// primitive must leave src at offset+n with zero bytes leaked past the limit.
func TestRelayCopyChunkLastChunkEOF(t *testing.T) {
	_, sc := relayTestSC(t)

	const chunk = 16 << 10
	srcData := relayFixture(chunk * 3)
	srcPath := filepath.Join(t.TempDir(), "last.bin")
	if err := os.WriteFile(srcPath, srcData, 0o644); err != nil {
		t.Fatal(err)
	}
	dstPath := filepath.Join(t.TempDir(), "last-dst.bin")
	src, err := sc.Open(srcPath)
	if err != nil {
		t.Fatalf("open src: %v", err)
	}
	defer src.Close()
	dst, err := sc.OpenFile(dstPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		t.Fatalf("open dst: %v", err)
	}

	for i := 0; i < 3; i++ {
		off := int64(i) * chunk
		written, err := RelayCopyChunk(context.Background(), src, dst, off, chunk, nil)
		if err != nil || written != chunk {
			t.Fatalf("chunk %d: written=%d err=%v", i, written, err)
		}
	}
	// Trailing confirm: one more byte from src must be EOF.
	one := make([]byte, 1)
	if _, err := src.Read(one); !errors.Is(err, io.EOF) {
		t.Fatalf("post-final-chunk read: err=%v, want io.EOF", err)
	}
	if err := dst.Close(); err != nil {
		t.Fatalf("close dst: %v", err)
	}
	if fi, err := os.Stat(dstPath); err != nil || fi.Size() != int64(len(srcData)) {
		t.Fatalf("dst size: fi=%v err=%v, want %d", fi, err, len(srcData))
	}
}

// TestRelayCopyChunkDualTee: the two hash states the engine streams (§2) —
// the chunk hash RESET per chunk (a fresh digest per RelayCopyChunk call) and
// the file hash NEVER reset (one digest across every call) — must each observe
// exactly the bytes that moved, in order, via the tee fan-out.
func TestRelayCopyChunkDualTee(t *testing.T) {
	_, sc := relayTestSC(t)

	const chunk = relayTestChunk
	srcData := relayFixture(chunk * 3)
	srcPath := filepath.Join(t.TempDir(), "tee.bin")
	if err := os.WriteFile(srcPath, srcData, 0o644); err != nil {
		t.Fatal(err)
	}
	dstPath := filepath.Join(t.TempDir(), "tee-dst.bin")
	src, err := sc.Open(srcPath)
	if err != nil {
		t.Fatalf("open src: %v", err)
	}
	defer src.Close()
	dst, err := sc.OpenFile(dstPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		t.Fatalf("open dst: %v", err)
	}

	fileSum := sha256.New() // never reset — the whole-stream digest
	chunkSums := make([][]byte, 0, 3)
	for i := 0; i < 3; i++ {
		chunkSum := sha256.New() // reset per chunk — the per-block digest
		off := int64(i) * chunk
		written, err := RelayCopyChunk(context.Background(), src, dst, off, chunk, []io.Writer{chunkSum, fileSum})
		if err != nil || written != chunk {
			t.Fatalf("chunk %d: written=%d err=%v", i, written, err)
		}
		chunkSums = append(chunkSums, chunkSum.Sum(nil))
	}
	if err := dst.Close(); err != nil {
		t.Fatalf("close dst: %v", err)
	}

	wantFile := sha256.Sum256(srcData)
	if !bytes.Equal(fileSum.Sum(nil), wantFile[:]) {
		t.Fatal("file hash (never-reset state) must equal sha256 of the whole streamed source")
	}
	for i, sum := range chunkSums {
		want := sha256.Sum256(srcData[i*chunk : (i+1)*chunk])
		if !bytes.Equal(sum, want[:]) {
			t.Fatalf("chunk %d hash mismatch (per-chunk reset state)", i)
		}
	}
}

// TestRelayCopyChunkCancelledCtx: an already-cancelled ctx fails the chunk
// BEFORE any seek/copy with written=0 (fast-fail). Aborting an in-flight
// chunk is the caller's watchdog closing the sftp client (Upload pattern) —
// the engine owns that lifetime, not this primitive.
func TestRelayCopyChunkCancelledCtx(t *testing.T) {
	_, sc := relayTestSC(t)

	srcPath := filepath.Join(t.TempDir(), "ctx.bin")
	if err := os.WriteFile(srcPath, relayFixture(1024), 0o644); err != nil {
		t.Fatal(err)
	}
	src, err := sc.Open(srcPath)
	if err != nil {
		t.Fatalf("open src: %v", err)
	}
	defer src.Close()
	dst, err := sc.OpenFile(filepath.Join(t.TempDir(), "ctx-dst.bin"), os.O_RDWR|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		t.Fatalf("open dst: %v", err)
	}
	defer dst.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	written, err := RelayCopyChunk(ctx, src, dst, 0, 1024, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
	if written != 0 {
		t.Fatalf("written=%d, want 0 (entry check, nothing moved)", written)
	}
}

// TestRelayManifestRoundTripAtomic pins the §3 on-disk protocol end to end:
// (1) the wire field names verbatim (version/chunk_bytes/source_size/
// source_mtime_unix/chunks, entries i+sha256) asserted on the RAW bytes — a
// Go-struct round-trip alone could mask a json tag typo, and these names are
// the cross-version protocol (ADR 0001); (2) read-back equality; (3) the
// first-run empty manifest (chunks=[]) survives the round trip; (4) atomic
// overwrite in place, with NO .tmp left behind (PosixRename moves it); (5) a
// stale .tmp from a crashed run is ignored/overwritten harmlessly; (6) a
// missing manifest reads back the os.ErrNotExist class (§2⑥ state
// discrimination keys on exactly that via errors.Is).
func TestRelayManifestRoundTripAtomic(t *testing.T) {
	_, sc := relayTestSC(t)

	mp := filepath.Join(t.TempDir(), "task.sshmgr-manifest.json")
	m1 := &RelayManifest{
		Version:         1,
		ChunkBytes:      268435456,
		SourceSize:      53687091200,
		SourceMtimeUnix: 1757126400,
		Chunks: []RelayChunkDone{
			{I: 0, SHA256: strings.Repeat("ab", 32)},
			{I: 3, SHA256: strings.Repeat("cd", 32)}, // hole shape: 1,2 pending
		},
	}
	if err := RelayWriteManifestAtomic(sc, mp, m1); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	// (1) protocol field names on the raw on-disk bytes.
	raw, err := os.ReadFile(mp)
	if err != nil {
		t.Fatal(err)
	}
	for _, frag := range []string{
		`"version":1`,
		`"chunk_bytes":268435456`,
		`"source_size":53687091200`,
		`"source_mtime_unix":1757126400`,
		`"chunks":[`,
		`{"i":0,"sha256":"abab`,
		`{"i":3,"sha256":"cdcd`,
	} {
		if !strings.Contains(string(raw), frag) {
			t.Fatalf("manifest wire bytes missing %s — §3 field names are protocol:\n%s", frag, raw)
		}
	}

	// (2) round-trip equality.
	got, err := RelayReadManifest(sc, mp)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if !reflect.DeepEqual(m1, got) {
		t.Fatalf("round-trip = %+v, want %+v", got, m1)
	}

	// (3) the first-run empty manifest.
	mEmpty := &RelayManifest{Version: 1, ChunkBytes: relayTestChunk, SourceSize: 0, SourceMtimeUnix: 1, Chunks: []RelayChunkDone{}}
	if err := RelayWriteManifestAtomic(sc, mp, mEmpty); err != nil {
		t.Fatalf("write empty manifest: %v", err)
	}
	got, err = RelayReadManifest(sc, mp)
	if err != nil {
		t.Fatalf("read empty manifest: %v", err)
	}
	if len(got.Chunks) != 0 {
		t.Fatalf("empty-chunks round-trip len=%d, want 0", len(got.Chunks))
	}

	// (4) overwrite in place + no .tmp residue.
	m2 := &RelayManifest{Version: 1, ChunkBytes: 268435456, SourceSize: 268435456 * 10, SourceMtimeUnix: 1757126401}
	for i := 0; i < 5; i++ {
		m2.Chunks = append(m2.Chunks, RelayChunkDone{I: i * 2, SHA256: fmt.Sprintf("%064x", i)})
	}
	if err := RelayWriteManifestAtomic(sc, mp, m2); err != nil {
		t.Fatalf("overwrite manifest: %v", err)
	}
	got, err = RelayReadManifest(sc, mp)
	if err != nil {
		t.Fatalf("read after overwrite: %v", err)
	}
	if !reflect.DeepEqual(m2, got) {
		t.Fatalf("after overwrite = %+v, want %+v", got, m2)
	}
	if _, err := os.Stat(mp + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf(".tmp must not survive a successful write (PosixRename moves it), stat err=%v", err)
	}

	// (5) stale .tmp from a crashed run is overwritten harmlessly. Written via
	// the host FS directly — the in-process server shares it.
	if err := os.WriteFile(mp+".tmp", []byte("stale junk from a crashed run"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RelayWriteManifestAtomic(sc, mp, m1); err != nil {
		t.Fatalf("write over stale .tmp: %v", err)
	}
	if got, err = RelayReadManifest(sc, mp); err != nil || !reflect.DeepEqual(m1, got) {
		t.Fatalf("write over stale .tmp: err=%v got=%+v, want m1", err, got)
	}

	// (6) missing manifest → not-exist class.
	if _, err := RelayReadManifest(sc, filepath.Join(t.TempDir(), "nope.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing manifest err = %v, want os.ErrNotExist class", err)
	}
}

// TestRelayAvailableNormalAndAncestorWalk (spec §2⑦, rev4 kimi#7): an existing
// dir gauges real available bytes (Bavail×Frsize — >0 on any live FS); a NOT
// yet existing deep dir (the first-run destination parent — MkdirAll moved
// into the engine's stage 0) walks UP to the nearest existing ancestor and
// gauges that, same figure as gauging the ancestor directly. Windows dev hosts
// cannot run this lane: pkg/sftp's server stubs statvfs@openssh.com with
// ENOTSUP there — the CI linux lane carries it (mirror of upload_test.go's
// symlink skips).
func TestRelayAvailableNormalAndAncestorWalk(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pkg/sftp server statvfs stub returns ENOTSUP on windows hosts — linux CI lane carries the real-statvfs cases")
	}
	c, sc := relayTestSC(t)

	base := filepath.ToSlash(t.TempDir())
	before, ok, err := c.RelayAvailable(sc, base)
	if err != nil || !ok || before <= 0 {
		t.Fatalf("existing dir: avail=%d ok=%v err=%v, want ok=true avail>0", before, ok, err)
	}

	deep := base + "/deep/never-created"
	avail2, ok2, err2 := c.RelayAvailable(sc, deep)
	if err2 != nil || !ok2 {
		t.Fatalf("missing deep dir: avail=%d ok=%v err=%v, want ancestor-walk ok=true err=nil", avail2, ok2, err2)
	}
	// The walk must gauge the same filesystem as the ancestor. Compare against
	// a window [before,after] of direct gauges rather than one fixed sample —
	// an unrelated write on the shared CI filesystem between calls must not
	// flake the equality.
	after, ok3, err3 := c.RelayAvailable(sc, base)
	if err3 != nil || !ok3 {
		t.Fatalf("existing dir re-gauge: ok=%v err=%v", ok3, err3)
	}
	lo, hi := before, after
	if lo > hi {
		lo, hi = hi, lo
	}
	if avail2 < lo || avail2 > hi {
		t.Fatalf("walk-up avail=%d outside the ancestor window [%d,%d] (same filesystem expected)", avail2, lo, hi)
	}
}

// TestRelayAvailableHardErrorPropagates: a StatVFS failure that is NOT
// not-exist-class (unsupported extension / permission) must propagate as err
// with ok=false and avail=0 — the caller fail-opens ANY error to
// space_check="unavailable" — and must NOT be swallowed into the ancestor walk
// (the walk is only for the not-exist class). Two platform triggers: the
// Windows dev lane gets pkg/sftp's ENOTSUP stub for ANY path; the linux CI
// lane gets a permission-denied walk through a chmod-000 parent (skipped for
// root, where the chmod wouldn't block).
func TestRelayAvailableHardErrorPropagates(t *testing.T) {
	c, sc := relayTestSC(t)

	dir := filepath.ToSlash(t.TempDir())
	if runtime.GOOS != "windows" {
		if os.Geteuid() == 0 {
			t.Skip("running as root — a chmod-000 parent does not block statvfs; the windows ENOTSUP lane carries this branch")
		}
		noaccess := filepath.Join(dir, "noaccess")
		if err := os.MkdirAll(filepath.Join(noaccess, "child"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(noaccess, 0o000); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(noaccess, 0o755) // restore so TempDir cleanup stays trivial
		dir = filepath.ToSlash(filepath.Join(noaccess, "child"))
	}

	avail, ok, err := c.RelayAvailable(sc, dir)
	if ok || err == nil || avail != 0 {
		t.Fatalf("hard StatVFS error must propagate: avail=%d ok=%v err=%v, want (0,false,err)", avail, ok, err)
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hard error must not surface as the not-exist class (that would mis-trigger the walk): %v", err)
	}
}
