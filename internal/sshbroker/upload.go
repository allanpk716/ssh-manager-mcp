package sshbroker

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/pkg/sftp"
)

// UploadResult holds the outcome of an upload.
type UploadResult struct {
	Files     int   // number of files uploaded
	Bytes     int64 // total bytes uploaded (may be < source size if Truncated)
	Truncated bool  // true if maxBytes was hit mid-upload
}

// Upload copies localPath (a file OR a directory, recursively) to remotePath on
// the server over SFTP — mirrors `scp -r localPath server:remotePath`. A file is
// put directly; a directory is walked (filepath.Walk), each subdir mkdir'd, each
// file sftp.Create'd + io.Copy'd. maxBytes > 0 caps the TOTAL bytes uploaded (the
// §6 bound); on cap, Truncated=true and the walk halts. maxBytes == 0 = unlimited.
//
// Cap semantics — "per-file atomic + walk-halt": countingWriter never hard-stops
// an in-flight io.Copy (that would leave a corrupt, half-written remote file);
// instead, the moment cumulative bytes exceed the cap the writer flags Truncated,
// and uploadDir halts the walk so no NEW file is started past the cap. The
// currently-uploading file completes atomically. Net effect: the cap bounds the
// worst-case overshoot to one file (a 14-byte tree with cap=3 uploads one ~7-byte
// file fully then stops, instead of streaming the whole tree). The §6 intent —
// bound total upload size — is honored, and Truncated is the surfaced signal.
//
// The local file is read from the broker's filesystem (the agent's machine); the
// agent chooses localPath (it already has the file — Upload just transfers it).
func (c *Client) Upload(localPath, remotePath string, maxBytes int64) (UploadResult, error) {
	sc, err := sftp.NewClient(c.c) // open an SFTP channel over the existing *ssh.Client
	if err != nil {
		return UploadResult{}, fmt.Errorf("sftp client: %w", err)
	}
	defer sc.Close()
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
	walkErr := filepath.Walk(localRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if ctr.truncated {
			return errCapStop
		}
		rel, err := filepath.Rel(localRoot, path)
		if err != nil {
			return err
		}
		target := filepath.Join(remoteRoot, rel)
		if info.IsDir() {
			return sc.Mkdir(target)
		}
		return uploadFile(sc, path, target, ctr, res)
	})
	if walkErr == errCapStop {
		return nil // truncation is reported via UploadResult.Truncated, not as error
	}
	return walkErr
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
