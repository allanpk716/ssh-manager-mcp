package updater

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/buildinfo"
)

// releaseJSONBlob builds a GitHub release document with the given assets
// (name → browser_download_url).
func releaseJSONBlob(t *testing.T, tag string, assets map[string]string) []byte {
	t.Helper()
	type asset struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	}
	type payload struct {
		TagName string  `json:"tag_name"`
		Assets  []asset `json:"assets"`
	}
	p := payload{TagName: tag}
	for name, u := range assets {
		p.Assets = append(p.Assets, asset{Name: name, BrowserDownloadURL: u})
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// wantAssetFor computes the asset name the discovery layer must pick for tag
// on the test platform.
func wantAssetFor(t *testing.T, tag string) string {
	t.Helper()
	name, err := AssetName(tag, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	return name
}

// TestLatestReleaseCustomBaseRebuild — with SSHMGR_UPDATE_BASE set, the API
// request goes to the base with GitHub's path structure, and the asset +
// checksums URLs are rebuilt onto the base (scheme/host swapped, path kept
// verbatim) — spec §4.2(4).
func TestLatestReleaseCustomBaseRebuild(t *testing.T) {
	tag := "v0.13.1"
	asset := wantAssetFor(t, tag)
	githubAssetURL := "https://github.com/" + buildinfo.Owner + "/" + buildinfo.Repo +
		"/releases/download/" + tag + "/" + asset

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write(releaseJSONBlob(t, tag, map[string]string{
			asset:       githubAssetURL,
			"other.zip": "https://github.com/x/y/releases/download/" + tag + "/other.zip",
		}))
	}))
	defer srv.Close()
	t.Setenv(updateBaseEnv, srv.URL)

	rel, err := LatestRelease(context.Background())
	if err != nil {
		t.Fatalf("LatestRelease: %v", err)
	}
	wantAPIPath := "/repos/" + buildinfo.Owner + "/" + buildinfo.Repo + "/releases/latest"
	if gotPath != wantAPIPath {
		t.Errorf("request path = %q, want %q", gotPath, wantAPIPath)
	}
	if rel.Tag != tag {
		t.Errorf("Tag = %q, want %q", rel.Tag, tag)
	}
	if rel.AssetName != asset {
		t.Errorf("AssetName = %q, want %q", rel.AssetName, asset)
	}
	// Asset URL rebuilt onto the base: base scheme/host + verbatim GitHub path.
	if !strings.HasPrefix(rel.AssetURL, srv.URL+"/") {
		t.Errorf("AssetURL = %q, want prefix %q", rel.AssetURL, srv.URL+"/")
	}
	if !strings.HasSuffix(rel.AssetURL, "/releases/download/"+tag+"/"+asset) {
		t.Errorf("AssetURL = %q, want suffix /releases/download/%s/%s", rel.AssetURL, tag, asset)
	}
	// Checksums sit beside the asset in the SAME (fully preserved GitHub
	// path) directory on the base host.
	wantChecksums := srv.URL + "/" + buildinfo.Owner + "/" + buildinfo.Repo +
		"/releases/download/" + tag + "/checksums.txt"
	if rel.ChecksumsURL != wantChecksums {
		t.Errorf("ChecksumsURL = %q, want %q", rel.ChecksumsURL, wantChecksums)
	}
}

// TestLatestReleaseDefaultBasePassthrough — default mode keeps GitHub's
// browser_download_url verbatim (no rebuild). httptest cannot intercept the
// default api.github.com URL, so the httpDo seam feeds a canned response.
func TestLatestReleaseDefaultBasePassthrough(t *testing.T) {
	t.Setenv(updateBaseEnv, "")
	tag := "v0.12.0"
	asset := wantAssetFor(t, tag)
	githubAssetURL := "https://github.com/" + buildinfo.Owner + "/" + buildinfo.Repo +
		"/releases/download/" + tag + "/" + asset

	orig := httpDo
	defer func() { httpDo = orig }()
	var gotURL string
	httpDo = func(c *http.Client, req *http.Request) (*http.Response, error) {
		gotURL = req.URL.String()
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(string(releaseJSONBlob(t, tag, map[string]string{asset: githubAssetURL})))),
			Header:     http.Header{},
			Request:    req,
		}, nil
	}

	rel, err := LatestRelease(context.Background())
	if err != nil {
		t.Fatalf("LatestRelease: %v", err)
	}
	wantAPI := "https://api.github.com/repos/" + buildinfo.Owner + "/" + buildinfo.Repo + "/releases/latest"
	if gotURL != wantAPI {
		t.Errorf("request URL = %q, want %q", gotURL, wantAPI)
	}
	if rel.AssetURL != githubAssetURL {
		t.Errorf("AssetURL = %q, want verbatim %q", rel.AssetURL, githubAssetURL)
	}
	wantChecksums := "https://github.com/" + buildinfo.Owner + "/" + buildinfo.Repo +
		"/releases/download/" + tag + "/checksums.txt"
	if rel.ChecksumsURL != wantChecksums {
		t.Errorf("ChecksumsURL = %q, want %q", rel.ChecksumsURL, wantChecksums)
	}
}

// TestReleaseByTag — valid tag hits .../releases/tags/{tag}; invalid tags are
// rejected client-side before any request leaves the process.
func TestReleaseByTag(t *testing.T) {
	tag := "v0.13.0-rc.1"
	asset := wantAssetFor(t, tag)
	var gotPath string
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		gotPath = r.URL.Path
		w.Write(releaseJSONBlob(t, tag, map[string]string{
			asset: "https://github.com/x/y/releases/download/" + tag + "/" + asset,
		}))
	}))
	defer srv.Close()
	t.Setenv(updateBaseEnv, srv.URL)

	rel, err := ReleaseByTag(context.Background(), tag)
	if err != nil {
		t.Fatalf("ReleaseByTag(%q): %v", tag, err)
	}
	wantPath := "/repos/" + buildinfo.Owner + "/" + buildinfo.Repo + "/releases/tags/" + tag
	if gotPath != wantPath {
		t.Errorf("request path = %q, want %q", gotPath, wantPath)
	}
	if rel.Tag != tag {
		t.Errorf("Tag = %q, want %q", rel.Tag, tag)
	}

	// Invalid tags: rejected before any HTTP traffic.
	for _, bad := range []string{"v1..2", "1.2.3;rm", "../evil", "", "v0.13.0 rc1"} {
		if _, err := ReleaseByTag(context.Background(), bad); err == nil {
			t.Errorf("ReleaseByTag(%q): want validation error, got nil", bad)
		}
	}
	if hits != 1 {
		t.Errorf("server hits = %d, want 1 (invalid tags must not reach the wire)", hits)
	}
}

func TestLatestReleaseErrorPaths(t *testing.T) {
	tag := "v0.13.2"
	asset := wantAssetFor(t, tag)

	t.Run("missing asset", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write(releaseJSONBlob(t, tag, map[string]string{
				"some-other.zip": "https://github.com/x/y/releases/download/" + tag + "/some-other.zip",
			}))
		}))
		defer srv.Close()
		t.Setenv(updateBaseEnv, srv.URL)
		_, err := LatestRelease(context.Background())
		if err == nil || !strings.Contains(err.Error(), asset) {
			t.Errorf("want missing-asset error naming %q, got %v", asset, err)
		}
	})

	t.Run("bad tag_name from server", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write(releaseJSONBlob(t, "not-a-version!!", map[string]string{}))
		}))
		defer srv.Close()
		t.Setenv(updateBaseEnv, srv.URL)
		_, err := LatestRelease(context.Background())
		if err == nil || !strings.Contains(err.Error(), "tag_name") {
			t.Errorf("want unusable tag_name error, got %v", err)
		}
	})

	t.Run("http 404", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		}))
		defer srv.Close()
		t.Setenv(updateBaseEnv, srv.URL)
		_, err := LatestRelease(context.Background())
		if err == nil || !strings.Contains(err.Error(), "404") {
			t.Errorf("want HTTP 404 error, got %v", err)
		}
	})

	t.Run("malformed json", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("<html>not json</html>"))
		}))
		defer srv.Close()
		t.Setenv(updateBaseEnv, srv.URL)
		_, err := LatestRelease(context.Background())
		if err == nil || !strings.Contains(err.Error(), "JSON") {
			t.Errorf("want malformed JSON error, got %v", err)
		}
	})

	t.Run("asset url not absolute", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write(releaseJSONBlob(t, tag, map[string]string{asset: "/relative/only"}))
		}))
		defer srv.Close()
		t.Setenv(updateBaseEnv, srv.URL)
		_, err := LatestRelease(context.Background())
		if err == nil || !strings.Contains(err.Error(), "absolute") {
			t.Errorf("want not-absolute asset URL error, got %v", err)
		}
	})
}

// TestLatestReleaseAPITimeout — release lookup is bounded (spec §4.1: 30s;
// shrunk here via the package var).
func TestLatestReleaseAPITimeout(t *testing.T) {
	orig := apiTimeout
	apiTimeout = 100 * time.Millisecond
	defer func() { apiTimeout = orig }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.Write([]byte("{}"))
	}))
	defer srv.Close()
	t.Setenv(updateBaseEnv, srv.URL)

	_, err := LatestRelease(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context.DeadlineExceeded, got %v", err)
	}
}

// TestLatestReleaseNonLoopbackHTTPBase — a custom base over plain http is only
// legal on loopback literals; anything else fails at the initial hop check,
// before any connection (spec §4.6).
func TestLatestReleaseNonLoopbackHTTPBase(t *testing.T) {
	t.Setenv(updateBaseEnv, "http://192.0.2.9:8080")
	if _, err := LatestRelease(context.Background()); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("want blocked error for non-loopback http base, got %v", err)
	}
}
