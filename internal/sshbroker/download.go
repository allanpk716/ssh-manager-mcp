package sshbroker

import (
	"context"
	"fmt"
	"io"

	"github.com/pkg/sftp"
)

// DownloadResult holds the outcome of a remote-file download.
type DownloadResult struct {
	Content   string // the (possibly capped) file content — the prefix when Truncated
	Bytes     int64  // total file size (may exceed len(Content) when capped)
	Truncated bool   // true if the file exceeded maxBytes and Content is only the prefix
}

// Download fetches remotePath over SFTP. ctx is honored: on cancellation the
// watchdog closes the sftp file/client so the in-flight io.Copy aborts, and
// Download returns ctx.Err() with the partial Content/Bytes it captured before
// the cancel (Truncated stays false — the cap was not hit). maxBytes > 0 caps
// retained content (the prefix); bytes beyond are counted then discarded, with
// Truncated set (mirrors Exec's cappedBuffer contract). maxBytes == 0 = unlimited.
func (c *Client) Download(ctx context.Context, remotePath string, maxBytes int64) (DownloadResult, error) {
	sc, err := sftp.NewClient(c.c)
	if err != nil {
		return DownloadResult{}, fmt.Errorf("sftp client: %w", err)
	}
	defer sc.Close()
	f, err := sc.Open(remotePath)
	if err != nil {
		return DownloadResult{}, err
	}
	defer f.Close()

	buf := &cappedBuffer{cap: maxBytes}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = f.Close() // unblock the in-flight io.Copy Read
			_ = sc.Close()
		case <-done:
		}
	}()

	_, copyErr := io.Copy(buf, f)
	res := DownloadResult{Content: buf.buf.String(), Bytes: buf.total, Truncated: buf.truncated}
	if ctx.Err() != nil {
		return res, ctx.Err() // cancellation — partial Content/Bytes preserved, Truncated stays false
	}
	if copyErr != nil {
		return res, copyErr
	}
	return res, nil
}
