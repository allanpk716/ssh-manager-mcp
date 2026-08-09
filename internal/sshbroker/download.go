package sshbroker

import (
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

// Download fetches remotePath from the connected server over SFTP. maxBytes > 0
// caps how much content is retained (the prefix); bytes beyond are counted then
// discarded, with Truncated set — so a huge file cannot blow up memory while the
// caller still learns its true size (mirrors Exec's cappedBuffer contract).
// maxBytes == 0 means unlimited.
func (c *Client) Download(remotePath string, maxBytes int64) (DownloadResult, error) {
	sc, err := sftp.NewClient(c.c) // open an SFTP channel over the existing *ssh.Client
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
	if _, err := io.Copy(buf, f); err != nil {
		return DownloadResult{}, err
	}
	return DownloadResult{Content: buf.buf.String(), Bytes: buf.total, Truncated: buf.truncated}, nil
}
