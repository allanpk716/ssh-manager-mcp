package sshbroker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"

	"github.com/pkg/sftp"
)

// UploadResult holds the outcome of an upload.
type UploadResult struct {
	Files     int   // number of files uploaded
	Bytes     int64 // total bytes uploaded (may be < source size if Truncated)
	Truncated bool  // true if maxBytes was hit mid-upload
}

// Upload copies localPath (a file OR a directory, recursively) to remotePath over
// SFTP — mirrors `scp -r localPath server:remotePath`. ctx is honored: on
// cancellation the watchdog closes the sftp client so the in-flight sftp op errors
// and the walk propagates; Upload returns ctx.Err() with the partial Files/Bytes
// counted before the cancel. The half-written remote file is left as-is (mirrors
// scp -r interrupted — cleanup is the caller's job). maxBytes == 0 = unlimited.
//
// maxBytes (§6) is enforced as TWO orthogonal layers (Plan 23):
//
//   - per-file pre-flight: any single file whose size is STRICTLY greater than
//     maxBytes is refused BEFORE transfer (zero bytes of it move, no remote file
//     is created) with capRefusedError naming file/size/cap; files completed
//     before the refusal remain. Exactly == cap is allowed. In dir walks a
//     symlink→file is measured by its TARGET (os.Stat follow, Plan 24) so the
//     gate matches the follow-the-link transfer, and a broken link is a walk
//     error; a symlink→directory is refused outright (Plan 26 — see uploadDir).
//   - cumulative walk-halt: when every file is within the cap but the running
//     total crosses it, Truncated=true and the walk halts BETWEEN files — the
//     in-flight file lands complete (see uploadFile's per-file-atomic note).
func (c *Client) Upload(ctx context.Context, localPath, remotePath string, maxBytes int64) (UploadResult, error) {
	sc, err := sftp.NewClient(c.c)
	if err != nil {
		return UploadResult{}, fmt.Errorf("sftp client: %w", err)
	}
	defer sc.Close()

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = sc.Close() // unblock in-flight sftp Write/Create → uploadFile errors → walk propagates
		case <-done:
		}
	}()

	info, err := os.Stat(localPath)
	if err != nil {
		return UploadResult{}, err
	}
	var res UploadResult
	ctr := &countingWriter{cap: maxBytes}
	if info.IsDir() {
		err = uploadDir(sc, localPath, remotePath, ctr, &res)
	} else {
		// Per-file pre-flight (Plan 23): a lone over-cap file is refused on the
		// os.Stat above — zero bytes transfer and NO remote file is created (the
		// error is the signal; UploadResult stays zero). cap==0 skips the gate.
		if maxBytes > 0 && info.Size() > maxBytes {
			return UploadResult{}, capRefusedError(localPath, info.Size(), maxBytes)
		}
		err = uploadFile(sc, localPath, remotePath, ctr, &res)
	}
	res.Bytes = ctr.total
	res.Truncated = ctr.truncated
	if ctx.Err() != nil {
		return res, ctx.Err() // cancellation precedence over copy/walk error
	}
	return res, err
}

// errCapStop is a filepath.Walk halt sentinel: when the counter flags truncation,
// the walk callback returns it to stop visiting further entries. It is swallowed
// at the uploadDir boundary — truncation is surfaced via UploadResult.Truncated,
// not as an error to the caller.
var errCapStop = errors.New("upload stopped: byte cap reached")

// capRefusedError builds the per-file pre-flight refusal (Plan 23): a file whose
// size is STRICTLY greater than the cap transfers zero bytes. The error is
// self-contained evidence — file path, actual size, cap — so the caller (agent
// at the MCP boundary) can shrink or split the payload without a second probe.
// cap arrives as a parameter from the caller (mcpserver passes MaxOutputBytes);
// sshbroker deliberately does not import mcpserver.
func capRefusedError(file string, size, cap int64) error {
	return fmt.Errorf("file %s (%d bytes) exceeds upload cap %d — refused before transfer (already-completed files remain)", file, size, cap)
}

// uploadFile puts a single file atomically: open local, create remote, io.Copy
// from local through a TeeReader that counts bytes into ctr. The copy runs to
// completion (or to the underlying io error); ctr flags truncation if cap was
// exceeded mid-copy but never aborts the stream — aborting would leave a
// half-written remote file. res.Files is bumped only on a fully-copied file.
// Callers enforce the per-file pre-flight BEFORE calling (Upload's single-file
// branch + uploadDir's walk callback), so an over-cap file never reaches this
// io.Copy on either path.
func uploadFile(sc *sftp.Client, localPath, remotePath string, ctr *countingWriter, res *UploadResult) error {
	in, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := sc.Create(remotePath)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, io.TeeReader(in, ctr)); err != nil {
		return err
	}
	res.Files++
	return nil
}

// uploadDir walks localRoot recursively; for each entry it preserves the relative
// path under remoteRoot — dirs are mkdir'd, files are uploaded via uploadFile.
// Per-file pre-flight (Plan 23): a file individually over the cap is refused on
// the walk's own FileInfo BEFORE uploadFile — zero bytes of it transfer, no
// remote file is created, the walk stops there, and the refusal error propagates
// to the caller (unlike errCapStop, which is swallowed); files completed before
// the refusal remain. Symlink entries in the walk (Plan 24 gate alignment +
// Plan 26 dir semantics, cap-independent): a link is re-stat'ed with follow
// so the gate sees the TARGET's size — the transfer follows the link, so the
// check must too; a broken link (stat failure) propagates as a walk error.
// A link whose target is a DIRECTORY is refused by name (upload the target
// directory directly) — recursively following directory links is not
// supported (loop/double-visit risk). Separately, the walk halts (errCapStop) the moment the
// counter flags truncation, so no new file is started once the CUMULATIVE total
// crosses the cap — that layer only ever sees files within the per-file bound.
// The root dir itself is the first entry visited, so remoteRoot is always
// created before any child lands in it. Plan 26: the root is first resolved
// via filepath.EvalSymlinks (plus an explicit junction follow-through on
// Windows, whose reparse points EvalSymlinks skips) so a symlink/junction
// root (which Upload's entry Stat already followed) is walked as the real
// directory rather than misclassified by Walk's lstat as a file.
func uploadDir(sc *sftp.Client, localRoot, remoteRoot string, ctr *countingWriter, res *UploadResult) error {
	// Root resolution (Plan 26): Upload's entry os.Stat FOLLOWS links (so a
	// linked dir root reaches here), but filepath.Walk lstats the root and
	// would misclassify it as a file. Resolve once up front so the walk starts
	// at the real directory; nested entries keep lstat semantics (Task 2 adds
	// an explicit refusal for symlinked sub-directories). Resolution is
	// BEST-EFFORT: on Windows EvalSymlinks can fail on a path that merely
	// TRAVERSES a junction ancestor (go1.25.8: "system cannot find the path
	// specified") even though the root itself is fine — on failure keep the
	// original localRoot; a genuinely bad root surfaces from Walk itself.
	if resolved, rerr := filepath.EvalSymlinks(localRoot); rerr == nil {
		localRoot = resolved
	}
	// Windows junctions Lstat as ModeIrregular ("?"), which EvalSymlinks
	// skips (it follows only ModeSymlink), so a junction root would slip
	// through unresolved — the walk would still misclassify it as a file.
	// Keep following any reparse point Readlink accepts (symlink or
	// junction) to a fixpoint; the iteration cap mirrors the kernel's
	// ELOOP bound (symlink cycles must terminate, not hang).
	links := 0
	for {
		fi, ferr := os.Lstat(localRoot)
		if ferr != nil {
			return ferr
		}
		if fi.Mode()&(os.ModeSymlink|os.ModeIrregular) == 0 {
			break
		}
		if links++; links > 64 {
			return fmt.Errorf("root %s: too many levels of symbolic links", localRoot)
		}
		target, terr := os.Readlink(localRoot)
		if terr != nil {
			return terr
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(localRoot), target)
		}
		localRoot = target
	}
	walkErr := filepath.Walk(localRoot, func(walkPath string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if ctr.truncated {
			return errCapStop
		}
		rel, err := filepath.Rel(localRoot, walkPath)
		if err != nil {
			return err
		}
		target := path.Join(remoteRoot, filepath.ToSlash(rel)) // POSIX remote path — correct for a Linux server on any broker host (Windows-broker→Linux-server is the primary deployment)
		if info.IsDir() {
			return sc.Mkdir(target)
		}
		// Symlink handling (Plan 24 cap alignment, Plan 26 dir semantics):
		// Walk's FileInfo is lstat-based. For ANY link entry — os.ModeSymlink
		// (unix dir symlinks; Windows symlinks), plus os.ModeIrregular for a
		// Windows junction, which lstats as Irregular on Go ≥1.23 (mount
		// points are reparse points but not tagged SYMLINK — same bit pair
		// the root loop above follows) — re-stat with follow. Target is a
		// DIRECTORY → refuse with a named error (following directory links
		// recursively is not supported — loop/double-visit risk; upload the
		// target directory directly). Target is a file → the followed size
		// participates in the per-file cap gate when armed (the transfer
		// follows the link, so the check must too — Plan 24). A broken link
		// fails the re-stat and propagates as a walk error naming the path.
		// The dir-refusal is deliberately cap-INDEPENDENT (cap==0 still
		// refuses), and at cap==0 a link→file entry now re-stats too —
		// semantic consistency, not a regression; non-link entries keep
		// Walk's FileInfo (no extra syscall on the common path).
		size := info.Size()
		if info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			st, err := os.Stat(walkPath)
			if err != nil {
				return err
			}
			if st.IsDir() {
				return fmt.Errorf("symlinked directory not uploaded: %s — upload the target directory directly (following directory links recursively is not supported)", walkPath)
			}
			size = st.Size()
		}
		if ctr.cap > 0 && size > ctr.cap {
			return capRefusedError(walkPath, size, ctr.cap)
		}
		return uploadFile(sc, walkPath, target, ctr, res)
	})
	if walkErr == errCapStop {
		return nil // truncation is reported via UploadResult.Truncated, not as error
	}
	return walkErr
}

// MkdirAll creates the directory named remotePath on the server, along with any
// necessary parents, over SFTP — mirroring os.MkdirAll semantics. If remotePath
// already exists as a directory this is a no-op; if it exists as a regular file
// an error is returned. It is the broker primitive UploadForProfile uses to
// ensure remotePath's PARENT exists before a transfer: T1's Upload puts files via
// sftp.Create and dirs via sftp.Mkdir, both of which require the destination's
// parent to pre-exist. MkdirAll-the-parent at the MCP boundary matches the
// `scp --parents` UX so an agent can target a freshly-named destination without a
// preparatory exec_command.
//
// remotePath is a POSIX path (the remote server's convention). For cross-platform
// robustness the path is normalized to forward slashes before delegating to
// sftp.Client.MkdirAll (which scans for '/' as its element separator): a Windows
// backslash path arriving here still resolves against a Windows-host testsshd,
// and a POSIX path is the native case for a real Linux remote.
func (c *Client) MkdirAll(remotePath string) error {
	sc, err := sftp.NewClient(c.c)
	if err != nil {
		return fmt.Errorf("sftp client: %w", err)
	}
	defer sc.Close()
	return sc.MkdirAll(filepath.ToSlash(remotePath))
}

// countingWriter is a minimal io.Writer that counts bytes and flags truncation at
// cap WITHOUT retaining content (upload streams to the remote; nothing to keep,
// unlike cappedBuffer which retains the prefix). The cap is advisory within a
// single file — Write always accepts all bytes so io.Copy completes and the
// remote file is not left half-written. With the Plan 23 per-file pre-flight in
// place, this advisory path only ever sees files within the cap; the flag it
// raises is the CUMULATIVE-layer signal that halts the walk in uploadDir.
type countingWriter struct {
	cap       int64 // 0 = unlimited
	total     int64
	truncated bool
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.total += int64(len(p))
	if w.cap > 0 && w.total > w.cap {
		w.truncated = true
	}
	return len(p), nil // always accept — cap is advisory within a file (see above)
}

// WriteFile writes r to remotePath over SFTP, creating the PARENT directory
// first (scp --parents UX) — both under ONE watchdog: ctx cancellation closes
// the sftp client, unblocking an in-flight op (Upload's watchdog pattern).
// Plan 33 spec rev3 §2.2, three load-bearing pins:
//   - parent via PURE POSIX path.Dir — never filepath.ToSlash (a backslash is
//     a legal POSIX filename char; on a Windows broker ToSlash would rewrite
//     /tmp/a\b into /tmp/a/b and create the WRONG parent, and the behavior
//     would drift with the broker's OS. Upload's existing Client.MkdirAll
//     carries that debt — registered in spec §8, not fixed here);
//   - sc.Create truncates an existing file (overwrite semantics, upload_file
//     parity);
//   - out.Close is EXPLICIT and CHECKED: SFTP write failures can surface only
//     at Close (flush/final packet), so success = io.Copy OK AND Close OK; a
//     Close error IS a write failure. (Upload's uploadFile uses a bare
//     `defer out.Close()` — registered debt, not fixed here.)
//
// On any failure the remote may hold a partially-written file and/or the
// created parent dir — cleanup is the caller's job (scp parity).
func (c *Client) WriteFile(ctx context.Context, remotePath string, r io.Reader) error {
	sc, err := sftp.NewClient(c.c)
	if err != nil {
		return fmt.Errorf("sftp client: %w", err)
	}
	defer sc.Close()

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = sc.Close() // unblock in-flight sftp op → WriteFile errors (Upload watchdog pattern)
		case <-done:
		}
	}()

	if err := sc.MkdirAll(path.Dir(remotePath)); err != nil {
		return fmt.Errorf("mkdir parent: %w", err)
	}
	out, err := sc.Create(remotePath)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil { // checked Close — spec rev3 §2.2
		return fmt.Errorf("close: %w", err)
	}
	return nil
}
