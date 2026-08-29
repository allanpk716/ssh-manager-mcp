package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Download tuning knobs (spec §4.1): a stalled download (idle 60s with zero
// bytes) or an over-long one (total 10min, 200MiB cap) is aborted. Package
// vars solely so tests can shrink them; production code must never mutate
// them.
var (
	downloadIdle     = 60 * time.Second
	downloadTotal    = 10 * time.Minute
	downloadMaxBytes = int64(200) << 20
)

// checksumsName is the goreleaser checksum asset name (.goreleaser.yml
// checksum.name_template).
const checksumsName = "checksums.txt"

// sha256HexRe matches a hex sha256 digest (either case; normalized to
// lowercase before use).
var sha256HexRe = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// ParseChecksums extracts the expected sha256 hex digest of asset from a
// sha256sum-style checksums file: one entry per line in the
// "<64-hex>␣␣<filename>" two-space text-mode format goreleaser emits, CRLF
// tolerated, blank lines skipped. The digest is returned lowercase.
//
// Fail-closed (spec §4.2(1)): a non-blank line that does not match the format
// is an error (a truncated or tampered checksums file must not be silently
// skimmed), a missing asset line is an error (缺行拒绝), and an empty asset
// argument is an error. Duplicate entries: the first match wins.
func ParseChecksums(data []byte, asset string) (string, error) {
	if asset == "" {
		return "", fmt.Errorf("checksums.txt: no entry for empty asset name")
	}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		hash, name, ok := strings.Cut(line, "  ")
		if !ok || !sha256HexRe.MatchString(hash) {
			return "", fmt.Errorf("checksums.txt line %d: want <sha256-hex>  <filename>, got %q",
				i+1, truncateForLog(line))
		}
		if name == asset {
			return strings.ToLower(hash), nil
		}
	}
	return "", fmt.Errorf("checksums.txt: no entry for asset %q", asset)
}

// DownloadAsset streams rawURL into destDir while computing sha256 on the
// fly, then — and only then — verifies wantSHA256 and renames the temp file
// to its final name (the URL path's last segment).
//
// Invariants (spec §4.1/§4.2(2)):
//   - any failure — network, stall, size cap, checksum mismatch — leaves
//     destDir without the target file: the temp file is removed and the final
//     name is only ever created from fully verified bytes (零残留);
//   - the initial URL and every redirect hop pass the transport rule
//     (allowedHop);
//   - wantSHA256, when non-empty, must be a 64-hex digest; the empty string
//     skips verification and exists solely for bootstrapping checksums.txt
//     itself, whose trust anchor is the release transport, not another hash.
//
// Returns the final file path.
func DownloadAsset(ctx context.Context, rawURL, wantSHA256, destDir string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("download %q: %w", rawURL, err)
	}
	if err := checkHop(u); err != nil {
		return "", err
	}
	if wantSHA256 != "" && !sha256HexRe.MatchString(wantSHA256) {
		return "", fmt.Errorf("download %s: wantSHA256 is not a 64-hex sha256 digest", u.Redacted())
	}
	name, err := safeURLFileName(u)
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, downloadTotal)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", name, err)
	}
	resp, err := httpDo(NewHTTPClient(), req)
	if err != nil {
		if resp != nil {
			resp.Body.Close() // redirect-abort path hands back a (closed) body
		}
		return "", fmt.Errorf("download %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: HTTP %d (want 200)", name, resp.StatusCode)
	}
	if resp.ContentLength > downloadMaxBytes {
		return "", fmt.Errorf("download %s: content-length %d exceeds %d byte limit",
			name, resp.ContentLength, downloadMaxBytes)
	}

	tmp, err := os.CreateTemp(destDir, ".sshmgr-download-*.part")
	if err != nil {
		return "", fmt.Errorf("download %s: %w", name, err)
	}
	committed := false
	defer func() {
		if !committed {
			tmp.Close()
			os.Remove(tmp.Name()) // zero-residue invariant: verified bytes only
		}
	}()

	hasher := sha256.New()
	var total int64
	buf := make([]byte, 64<<10)
	src := &idleTimeoutReader{r: resp.Body, idle: downloadIdle, last: time.Now()}
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			total += int64(n)
			if total > downloadMaxBytes {
				return "", fmt.Errorf("download %s: exceeds %d byte limit after %d bytes",
					name, downloadMaxBytes, total)
			}
			if _, werr := tmp.Write(buf[:n]); werr != nil {
				return "", fmt.Errorf("download %s: %w", name, werr)
			}
			_, _ = hasher.Write(buf[:n]) // hash.Write is specified never to error
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", fmt.Errorf("download %s: %w", name, rerr)
		}
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("download %s: %w", name, err)
	}
	if wantSHA256 != "" {
		got := hex.EncodeToString(hasher.Sum(nil))
		want := strings.ToLower(wantSHA256)
		if got != want {
			return "", fmt.Errorf("download %s: sha256 mismatch: got %s want %s", name, got, want)
		}
	}
	final := filepath.Join(destDir, name)
	if err := os.Rename(tmp.Name(), final); err != nil {
		return "", fmt.Errorf("download %s: %w", name, err)
	}
	committed = true
	return final, nil
}

// idleTimeoutReader enforces the idle-stall rule (spec §4.1: 60s with zero
// bytes aborts the download). net/http has no per-read deadline on a response
// body, so the wrapper times every Read: it aborts before issuing a read when
// no progress has happened for idle, and after a read that itself blocked for
// ≥ idle — a blocking read hides the stall from the pre-read check, and a
// connection silent for most of that window is stalled even if a byte arrives
// at the very end.
type idleTimeoutReader struct {
	r    io.Reader
	idle time.Duration
	last time.Time // last progress instant (construction, or last n>0 Read)
}

func (w *idleTimeoutReader) Read(p []byte) (int, error) {
	now := time.Now()
	if wait := now.Sub(w.last); wait >= w.idle {
		return 0, fmt.Errorf("download stalled: no bytes for %s (idle limit %s)",
			wait.Round(time.Millisecond), w.idle)
	}
	start := now
	n, err := w.r.Read(p)
	now = time.Now()
	if span := now.Sub(start); span >= w.idle {
		return n, fmt.Errorf("download stalled: read blocked %s (idle limit %s)",
			span.Round(time.Millisecond), w.idle)
	}
	if n > 0 {
		w.last = now
	}
	return n, err
}

// safeURLFileName derives the on-disk name from the URL path's last segment
// and enforces a strict [A-Za-z0-9._-] charset: release asset names
// (sshmgr_<ver>_<os>_<arch>.<zip|tar.gz>) and checksums.txt both fit, while
// everything unsafe — separators (a URL segment may legally carry '\' or ':',
// which are path/ADS metacharacters on Windows), traversal, unicode, control
// characters — fails closed before anything is written.
func safeURLFileName(u *url.URL) (string, error) {
	name := path.Base(u.Path)
	if name == "" || name == "." || name == "/" {
		return "", fmt.Errorf("download %s: URL has no file name", u.Redacted())
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' ||
			c == '.' || c == '_' || c == '-') {
			return "", fmt.Errorf("download %s: file name %q has characters outside [A-Za-z0-9._-]",
				u.Redacted(), name)
		}
	}
	return name, nil
}
