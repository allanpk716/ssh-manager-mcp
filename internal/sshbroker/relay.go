package sshbroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"

	"github.com/pkg/sftp"
)

// RelaySFTP opens a dedicated SFTP subsystem client over the established SSH
// connection — the constructor every relay primitive's *sftp.Client comes from.
// THIN by contract (spec §2 connection-ownership discipline): it only wraps
// sftp.NewClient — no watchdog, no lifetime management. The CALLER owns the
// client's lifetime: a relay task arms the Upload-style watchdog (ctx cancel →
// sc.Close() unblocks an in-flight sftp op) around its WHOLE engine loop and
// closes the client when the task leaves the running set — the primitives
// below (RelayCopyChunk / RelayReadManifest / RelayWriteManifestAtomic /
// RelayAvailable) all take the *sftp.Client as a parameter, so the one
// caller-armed watchdog covers every op they issue.
func (c *Client) RelaySFTP() (*sftp.Client, error) {
	sc, err := sftp.NewClient(c.c)
	if err != nil {
		return nil, fmt.Errorf("sftp client: %w", err)
	}
	return sc, nil
}

// RelayPosixRenameOK reports whether the SFTP server advertised
// posix-rename@openssh.com — relay's hard dependency for the atomic manifest
// write and the commit rename (spec §2 stage 0 / §3; no fallback exists by
// design, the Remove+Rename fallback matrix was cut). HasExtension is a pure
// in-memory lookup against the SSH_FXP_VERSION handshake — ZERO IO — so the
// probe is safe to run unconditionally on every start AND resume before any
// byte moves. Absence is a failure distinct from an IO error: the engine
// reports "server lacks posix-rename" guidance instead of a generic failure.
func (c *Client) RelayPosixRenameOK(sc *sftp.Client) bool {
	_, ok := sc.HasExtension("posix-rename@openssh.com")
	return ok
}

// RelayAvailable gauges the free space (Bavail×Frsize — bytes available to a
// non-root writer) of the filesystem holding dir, the §2⑦ space pre-check's
// single figure. dir is a POSIX path (the remote's convention). dir itself may
// NOT exist yet — MkdirAll moved into the engine's stage 0, so a first run's
// fresh destination parent is missing at preflight: on a not-exist-class error
// the walk climbs via path.Dir to the nearest EXISTING ancestor and gauges
// that (rev4 kimi#7 — the pre-check must not degenerate to unavailable on
// every first-run); if even the root ("/" self-parents, so the loop tries it
// last) yields nothing, the result is (0, false, nil) — soft unavailable, not
// an error. Errors that are NOT not-exist-class (unsupported statvfs
// extension, permission, transport) propagate as err untouched — the caller
// fail-opens ANY error to space_check="unavailable" and proceeds (grilling
// decision, §2⑦), so ok is true only when a real byte figure was obtained.
// avail is int64-safe: a uint64 Bavail×Frsize product that would exceed int64
// (pathological Frsize) saturates at MaxInt64 instead of wrapping negative.
func (c *Client) RelayAvailable(sc *sftp.Client, dir string) (avail int64, ok bool, err error) {
	for {
		vfs, serr := sc.StatVFS(dir)
		if serr == nil {
			u := vfs.Bavail * vfs.Frsize
			if vfs.Frsize != 0 && vfs.Bavail > math.MaxInt64/vfs.Frsize {
				u = math.MaxInt64 // product exceeds int64 — saturate, never wrap
			}
			return int64(u), true, nil
		}
		if !errors.Is(serr, os.ErrNotExist) {
			return 0, false, serr // hard error — caller fail-opens to unavailable
		}
		parent := path.Dir(dir)
		if parent == dir {
			return 0, false, nil // climbed past the root, nothing exists — soft unavailable
		}
		dir = parent
	}
}

// RelayCopyChunk moves exactly n bytes of src at offset into dst at offset —
// the §2 per-chunk pipeline primitive. The copy shape is PINNED (rev3 kimi#5):
// src.Seek + dst.Seek, then io.CopyN over io.TeeReader(io.LimitReader(src, n),
// multi(tee…)). LimitReader+CopyN is what makes the length EXACT — a bare
// io.Copy would run to EOF and flood everything past the chunk boundary into
// the partial, corrupting sibling chunks on any resume. Every byte read
// mirrors into the tee writers (the engine's chunk hash — reset per chunk —
// and file hash — never reset); a nil/empty tee is legal (no hashing wanted).
// written != n → error with the honest count: a short read means the source
// shrank/truncated mid-flight and the chunk must NOT be recorded complete
// (io.EOF and transport errors both surface wrapped, via %w). ctx is an ENTRY
// check only — aborting an in-flight chunk is the caller's watchdog closing
// the sftp client (Upload pattern), which unblocks the read/write this
// function is parked in; the caller owns src/dst lifetime.
func RelayCopyChunk(ctx context.Context, src, dst *sftp.File, offset, n int64, tee []io.Writer) (written int64, err error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if _, err := src.Seek(offset, io.SeekStart); err != nil {
		return 0, fmt.Errorf("seek src to %d: %w", offset, err)
	}
	if _, err := dst.Seek(offset, io.SeekStart); err != nil {
		return 0, fmt.Errorf("seek dst to %d: %w", offset, err)
	}
	written, err = io.CopyN(dst, io.TeeReader(io.LimitReader(src, n), io.MultiWriter(tee...)), n)
	if err != nil {
		return written, fmt.Errorf("chunk copy at offset %d stopped after %d of %d bytes: %w", offset, written, n, err)
	}
	if written != n { // CopyN errors on any short copy — belt and braces for the §2 length discipline
		return written, fmt.Errorf("chunk copy at offset %d: %d of %d bytes", offset, written, n)
	}
	return written, nil
}

// RelayChunkDone is one completed chunk's record in the on-disk manifest. The
// `i` / `sha256` field names ARE the cross-version protocol (ADR 0001, spec
// §3) — never rename them; sha256 is lowercase hex (64 chars).
type RelayChunkDone struct {
	I      int    `json:"i"`
	SHA256 string `json:"sha256"`
}

// RelayManifest is the on-disk transfer manifest <to_path>.sshmgr-manifest.json
// (spec §3). Its field names are protocol: version 1 fields only ever grow
// (ADR 0001 consequence), so decoding deliberately TOLERATES unknown fields
// (plain json.Unmarshal) — a v1 reader must keep reading a manifest written by
// a later version. Chunks lists ONLY completed chunks (holes = pending); the
// root/file digests are never persisted — derived at completion and discarded.
type RelayManifest struct {
	Version         int              `json:"version"`
	ChunkBytes      int64            `json:"chunk_bytes"`
	SourceSize      int64            `json:"source_size"`
	SourceMtimeUnix int64            `json:"source_mtime_unix"`
	Chunks          []RelayChunkDone `json:"chunks"`
}

// RelayReadManifest opens and parses the manifest at manifestPath (POSIX path).
// A missing file surfaces as the os.ErrNotExist class — the §2⑥ state
// combination table keys on exactly that via errors.Is. Parse-defense (the
// derived size bound and structural validation: version==1, chunk_bytes match,
// index bounds/uniqueness, 64-hex, completed bytes ≤ source_size) lives at the
// preflight layer (§2⑥), which Stats the size BEFORE calling this — the
// primitive stays a thin open→read→unmarshal.
func RelayReadManifest(sc *sftp.Client, manifestPath string) (*RelayManifest, error) {
	f, err := sc.Open(manifestPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("read manifest %s: %w", manifestPath, err)
	}
	var m RelayManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", manifestPath, err)
	}
	return &m, nil
}

// RelayWriteManifestAtomic persists m as the manifest at manifestPath (POSIX
// path): marshal → write <manifestPath>.tmp (create/truncate) → CHECKED Close
// (SFTP write failures can surface only at the close/flush packet — WriteFile
// precedent) → PosixRename tmp over the real name (atomic overwrite — the §3
// hard dependency; servers lacking posix-rename are rejected at stage 0 by the
// RelayPosixRenameOK probe, so there is deliberately no fallback here). A
// stale <manifestPath>.tmp from a crashed run is harmless: create/truncate
// simply overwrites it, and a .tmp never survives a successful write (the
// rename MOVES it). Called once per completed chunk — the whole-file rewrite
// is the §3 protocol's accepted O(chunks²) write amplification, bounded by the
// 16384-chunk gate (§2③b).
func RelayWriteManifestAtomic(sc *sftp.Client, manifestPath string, m *RelayManifest) error {
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	tmp := manifestPath + ".tmp"
	out, err := sc.Create(tmp)
	if err != nil {
		return fmt.Errorf("create manifest tmp %s: %w", tmp, err)
	}
	if _, err := out.Write(b); err != nil {
		_ = out.Close()
		return fmt.Errorf("write manifest tmp %s: %w", tmp, err)
	}
	if err := out.Close(); err != nil { // checked Close — a flush error IS a write failure
		return fmt.Errorf("close manifest tmp %s: %w", tmp, err)
	}
	if err := sc.PosixRename(tmp, manifestPath); err != nil {
		return fmt.Errorf("posix-rename manifest to %s: %w", manifestPath, err)
	}
	return nil
}
