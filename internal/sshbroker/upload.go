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
// scp -r interrupted — cleanup is the caller's job). maxBytes caps TOTAL bytes (§6);
// on cap, Truncated=true and the walk halts. maxBytes == 0 = unlimited. See the
// "per-file atomic + walk-halt" note on uploadDir for cap semantics within a file.
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

// uploadFile puts a single file atomically: open local, create remote, io.Copy
// from local through a TeeReader that counts bytes into ctr. The copy runs to
// completion (or to the underlying io error); ctr flags truncation if cap was
// exceeded mid-copy but never aborts the stream — aborting would leave a
// half-written remote file. res.Files is bumped only on a fully-copied file.
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
// The walk halts (errCapStop) the moment the counter flags truncation, so no new
// file is started once the cap is exceeded. The root dir itself is the first
// entry visited, so remoteRoot is always created before any child lands in it.
func uploadDir(sc *sftp.Client, localRoot, remoteRoot string, ctr *countingWriter, res *UploadResult) error {
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
// remote file is not left half-written; the walk-halt in uploadDir is what
// enforces the cap between files.
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
