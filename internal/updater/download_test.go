package updater

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// dummySHA is a format-valid 64-hex digest for tests where the digest value
// itself is irrelevant (the download never completes or fails earlier).
var dummySHA = strings.Repeat("ab", 32)

func testSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func dirEntries(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestDownloadAssetSuccess(t *testing.T) {
	content := []byte("fake archive bytes for the sshmgr self-update flow")
	sum := testSHA256(content)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	}))
	defer srv.Close()
	// Custom base = the loopback httptest server: exercises the custom-base
	// initial hop check (host == base host over loopback http).
	t.Setenv(updateBaseEnv, srv.URL)
	dir := t.TempDir()

	rawURL := srv.URL + "/sshmgr_0.13.1_windows_amd64.zip"
	final, err := DownloadAsset(context.Background(), rawURL, sum, dir)
	if err != nil {
		t.Fatalf("DownloadAsset: %v", err)
	}
	if got := filepath.Base(final); got != "sshmgr_0.13.1_windows_amd64.zip" {
		t.Errorf("final name = %q, want sshmgr_0.13.1_windows_amd64.zip", got)
	}
	got, err := os.ReadFile(final)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("downloaded content mismatch")
	}
	// Exactly one file: no temp/.part residue next to the verified artifact.
	if entries := dirEntries(t, dir); len(entries) != 1 {
		t.Errorf("destDir entries = %d (%v), want 1", len(entries), entries)
	}

	// Bootstrap variant: empty wantSHA256 is structurally gated to
	// checksums.txt downloads only (whose trust anchor is the release
	// transport, not another hash) — this URL shape is the sanctioned path.
	dir2 := t.TempDir()
	final2, err := DownloadAsset(context.Background(), srv.URL+"/checksums.txt", "", dir2)
	if err != nil {
		t.Fatalf("DownloadAsset (no verify): %v", err)
	}
	if filepath.Base(final2) != "checksums.txt" {
		t.Errorf("final name = %q, want checksums.txt", filepath.Base(final2))
	}
}

// TestDownloadAssetEmptyHashGate — the empty-digest path only opens for
// checksums.txt: any other URL with an empty hash is refused before any
// request (R1 发现 2: 无校验路径不得有可误触的裸字符串入口).
func TestDownloadAssetEmptyHashGate(t *testing.T) {
	t.Setenv(updateBaseEnv, "")
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
	}))
	defer srv.Close()

	dir := t.TempDir()
	_, err := DownloadAsset(context.Background(), srv.URL+"/f.zip", "", dir)
	if err == nil || !strings.Contains(err.Error(), checksumsName) {
		t.Fatalf("want empty-hash gate error naming %s, got %v", checksumsName, err)
	}
	if hits != 0 {
		t.Errorf("server hits = %d, want 0 (gate must precede any request)", hits)
	}
	if entries := dirEntries(t, dir); len(entries) != 0 {
		t.Errorf("destDir not clean: %v", entries)
	}
}

func TestSafeURLFileName(t *testing.T) {
	cases := []struct {
		raw     string
		want    string
		wantErr bool
	}{
		{"https://x/y/sshmgr_0.13.0_windows_amd64.zip", "sshmgr_0.13.0_windows_amd64.zip", false},
		{"https://x/y/checksums.txt", "checksums.txt", false},
		{"https://x/y/", "y", false}, // trailing slash: Base drops it
		// Traversal names must die here — even though a rename onto a
		// directory would fail downstream, the fence is this guard.
		{"https://x/y/f.zip/..", "", true},
		{"https://x/y/../..", "", true},
		{"https://x/y/..%2F..", "", true}, // decoded Path is /y/../../
		{"https://x/y/.", "", true},
		{"https://x", "", true}, // no path at all
		// Windows metacharacters inside a legal URL segment.
		{"https://x/y/a:b.zip", "", true},
		{`https://x/y/a\b.zip`, "", true},
		{"https://x/y/ф.zip", "", true},
		// Windows device names pass the charset: rename-time OS refusal is a
		// usability error, not a security one (cannot escape destDir).
		{"https://x/y/con.zip", "con.zip", false},
	}
	for _, tc := range cases {
		u, err := url.Parse(tc.raw)
		if err != nil {
			t.Fatalf("url.Parse(%q): %v", tc.raw, err)
		}
		got, err := safeURLFileName(u)
		if tc.wantErr {
			if err == nil {
				t.Errorf("safeURLFileName(%q) = %q, want error", tc.raw, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("safeURLFileName(%q): %v", tc.raw, err)
			continue
		}
		if got != tc.want {
			t.Errorf("safeURLFileName(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// TestDownloadAssetRejectsDotDotURL — the ".." rejection through the full
// DownloadAsset path: refused before any request, zero residue.
func TestDownloadAssetRejectsDotDotURL(t *testing.T) {
	t.Setenv(updateBaseEnv, "")
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
	}))
	defer srv.Close()

	dir := t.TempDir()
	dummy := strings.Repeat("ab", 32)
	_, err := DownloadAsset(context.Background(), srv.URL+"/f.zip/..", dummy, dir)
	if err == nil || !strings.Contains(err.Error(), "usable file name") {
		t.Fatalf("want unusable-file-name error, got %v", err)
	}
	if hits != 0 {
		t.Errorf("server hits = %d, want 0", hits)
	}
	if entries := dirEntries(t, dir); len(entries) != 0 {
		t.Errorf("destDir not clean: %v", entries)
	}
}

func TestDownloadAssetMismatchZeroResidue(t *testing.T) {
	content := []byte("tampered payload")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	}))
	defer srv.Close()
	t.Setenv(updateBaseEnv, "")
	dir := t.TempDir()

	want := testSHA256([]byte("original payload"))
	_, err := DownloadAsset(context.Background(), srv.URL+"/sshmgr_0.13.1_linux_amd64.tar.gz", want, dir)
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("want sha256 mismatch error, got %v", err)
	}
	// Zero residue: the target file was never created and the temp file is gone.
	if entries := dirEntries(t, dir); len(entries) != 0 {
		t.Errorf("destDir not clean after mismatch: %v", entries)
	}
}

func TestDownloadAssetMalformedSHA(t *testing.T) {
	t.Setenv(updateBaseEnv, "")
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
	}))
	defer srv.Close()

	dir := t.TempDir()
	_, err := DownloadAsset(context.Background(), srv.URL+"/f.zip", "not-hex", dir)
	if err == nil || !strings.Contains(err.Error(), "64-hex") {
		t.Fatalf("want 64-hex validation error, got %v", err)
	}
	if hits != 0 {
		t.Errorf("server hits = %d, want 0 (validation must precede any request)", hits)
	}
}

func TestDownloadAssetSizeCap(t *testing.T) {
	t.Setenv(updateBaseEnv, "")
	orig := downloadMaxBytes
	downloadMaxBytes = 1024
	defer func() { downloadMaxBytes = orig }()
	big := bytes.Repeat([]byte("a"), 2048)

	t.Run("content-length precheck", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "2048")
			w.Write(big)
		}))
		defer srv.Close()
		dir := t.TempDir()
		_, err := DownloadAsset(context.Background(), srv.URL+"/f.zip", testSHA256(big), dir)
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("want exceeds-limit error, got %v", err)
		}
		if entries := dirEntries(t, dir); len(entries) != 0 {
			t.Errorf("destDir not clean: %v", entries)
		}
	})

	t.Run("streamed over-limit", func(t *testing.T) {
		// Two flushed writes force chunked transfer (unknown ContentLength),
		// so the cap must fire in the streaming loop.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write(big[:1024])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			w.Write(big[1024:])
		}))
		defer srv.Close()
		dir := t.TempDir()
		_, err := DownloadAsset(context.Background(), srv.URL+"/f.zip", testSHA256(big), dir)
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("want exceeds-limit error, got %v", err)
		}
		if entries := dirEntries(t, dir); len(entries) != 0 {
			t.Errorf("destDir not clean: %v", entries)
		}
	})
}

// TestDownloadAssetIdleTimeout — a server that goes silent mid-stream must
// trip the idle rule (spec §4.1: 60s zero bytes; shrunk here).
func TestDownloadAssetIdleTimeout(t *testing.T) {
	t.Setenv(updateBaseEnv, "")
	origIdle, origTotal := downloadIdle, downloadTotal
	downloadIdle = 150 * time.Millisecond
	downloadTotal = 30 * time.Second
	defer func() { downloadIdle, downloadTotal = origIdle, origTotal }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("first"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(400 * time.Millisecond) // silent stretch ≥ idle limit
		w.Write([]byte("second"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	_, err := DownloadAsset(context.Background(), srv.URL+"/f.zip", dummySHA, dir)
	if err == nil || !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("want stalled error, got %v", err)
	}
	if entries := dirEntries(t, dir); len(entries) != 0 {
		t.Errorf("destDir not clean: %v", entries)
	}
}

// TestDownloadAssetTotalTimeout — the whole download is bounded (spec §4.1:
// 10min; shrunk here). idle is kept large so only the total budget can fire.
func TestDownloadAssetTotalTimeout(t *testing.T) {
	t.Setenv(updateBaseEnv, "")
	origIdle, origTotal := downloadIdle, downloadTotal
	downloadIdle = time.Minute
	downloadTotal = 250 * time.Millisecond
	defer func() { downloadIdle, downloadTotal = origIdle, origTotal }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.Write([]byte("too late"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	_, err := DownloadAsset(context.Background(), srv.URL+"/f.zip", dummySHA, dir)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context.DeadlineExceeded, got %v", err)
	}
	if entries := dirEntries(t, dir); len(entries) != 0 {
		t.Errorf("destDir not clean: %v", entries)
	}
}

// TestDownloadAssetRedirectToBadHost — the per-hop whitelist guards the
// download stream too; a redirect to a non-whitelisted host aborts with zero
// residue.
func TestDownloadAssetRedirectToBadHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://evil.example.com/x.zip", http.StatusFound)
	}))
	defer srv.Close()
	t.Setenv(updateBaseEnv, "")

	dir := t.TempDir()
	_, err := DownloadAsset(context.Background(), srv.URL+"/f.zip", dummySHA, dir)
	if err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("want blocked error, got %v", err)
	}
	if entries := dirEntries(t, dir); len(entries) != 0 {
		t.Errorf("destDir not clean: %v", entries)
	}
}

// TestDownloadAssetBlockedInitialHop — the initial URL is validated before any
// connection (net/http's CheckRedirect never sees the first request).
func TestDownloadAssetBlockedInitialHop(t *testing.T) {
	t.Setenv(updateBaseEnv, "")
	dir := t.TempDir()
	sum := testSHA256([]byte("x"))
	for _, raw := range []string{
		"http://192.0.2.9/sshmgr_0.13.1_linux_amd64.tar.gz", // non-loopback http
		"https://evil.example.com/x.zip",                    // https off-whitelist
		"ftp://127.0.0.1/x.zip",                             // unsupported scheme
	} {
		if _, err := DownloadAsset(context.Background(), raw, sum, dir); err == nil || !strings.Contains(err.Error(), "blocked") {
			t.Errorf("%s: want blocked error, got %v", raw, err)
		}
	}
	if entries := dirEntries(t, dir); len(entries) != 0 {
		t.Errorf("destDir not clean: %v", entries)
	}
}

func TestParseChecksums(t *testing.T) {
	h1 := strings.Repeat("ab", 32)
	h2 := strings.Repeat("cd", 32)
	h3 := strings.Repeat("ef", 32)
	zip := "sshmgr_0.13.0_windows_amd64.zip"
	tgz := "sshmgr_0.13.0_linux_amd64.tar.gz"

	cases := []struct {
		name    string
		data    string
		asset   string
		want    string
		wantErr string // "" = success
	}{
		{"lf multi-line", h1 + "  " + zip + "\n" + h2 + "  " + tgz + "\n", tgz, h2, ""},
		{"crlf tolerated", h1 + "  " + zip + "\r\n" + h2 + "  " + tgz + "\r\n", tgz, h2, ""},
		{"no trailing newline", h1 + "  " + zip + "\n" + h2 + "  " + tgz, tgz, h2, ""},
		{"blank lines skipped", "\n" + h1 + "  " + zip + "\n\n" + h2 + "  " + tgz + "\n\n", zip, h1, ""},
		{"uppercase digest normalized", strings.ToUpper(h1) + "  " + zip, zip, h1, ""},
		{"target first line", h3 + "  " + zip + "\n" + h1 + "  " + tgz, zip, h3, ""},
		{"duplicate entries first wins", h3 + "  " + zip + "\n" + h1 + "  " + zip, zip, h3, ""},
		{"missing asset", h1 + "  " + zip, tgz, "", "no entry"},
		{"empty data", "", zip, "", "no entry"},
		{"single space rejected", h1 + " " + zip, zip, "", "want <sha256-hex>  <filename>"},
		{"binary marker form rejected", h1 + " *" + zip, zip, "", "want <sha256-hex>  <filename>"},
		{"short hash rejected", "abc123  " + zip, zip, "", "want <sha256-hex>  <filename>"},
		{"non-hex rejected", strings.Repeat("zz", 32) + "  " + zip, zip, "", "want <sha256-hex>  <filename>"},
		{"empty asset arg", h1 + "  " + zip, "", "", "no entry"},
	}
	for _, tc := range cases {
		got, err := ParseChecksums([]byte(tc.data), tc.asset)
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("%s: ParseChecksums: %v", tc.name, err)
				continue
			}
			if got != tc.want {
				t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: want error containing %q, got %v", tc.name, tc.wantErr, err)
		}
	}
}
