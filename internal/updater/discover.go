package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"time"

	"ssh-manager-mcp/internal/buildinfo"
)

// apiTimeout is the release-lookup deadline (spec §4.1: 30s). Var solely so
// tests can shrink it.
var apiTimeout = 30 * time.Second

const (
	apiMaxBodyBytes = int64(16) << 20 // bound on the release JSON document
	errSnippetBytes = int64(4) << 10  // body snippet kept for HTTP error messages
)

// Release is one GitHub release narrowed to what the update flow needs: the
// target asset for the running platform plus its checksums sidecar (spec
// §4.1).
type Release struct {
	Tag          string
	AssetName    string
	AssetURL     string
	ChecksumsURL string
}

// LatestRelease discovers the latest release (the GitHub /releases/latest
// endpoint excludes drafts and prereleases by construction) and resolves the
// asset for the running GOOS/GOARCH.
func LatestRelease(ctx context.Context) (*Release, error) {
	base, _, err := baseAndHost()
	if err != nil {
		return nil, err
	}
	return fetchRelease(ctx, releasesAPIURL(base, "latest"))
}

// ReleaseByTag discovers an exact tag — prereleases included; this is the
// downgrade / pin channel (spec §4.1). The tag is format-validated
// (ValidateTag) and path-escaped before it touches the request path.
func ReleaseByTag(ctx context.Context, tag string) (*Release, error) {
	if err := ValidateTag(tag); err != nil {
		return nil, err
	}
	base, _, err := baseAndHost()
	if err != nil {
		return nil, err
	}
	return fetchRelease(ctx, releasesAPIURL(base, "tags/"+url.PathEscape(tag)))
}

// releasesAPIURL rebuilds the releases API endpoint onto base: GitHub's path
// structure is copied verbatim onto the base's scheme/host — mirrors must
// serve GitHub's exact path layout for the custom base to be usable at all
// (spec §4.2(4)); a path prefix on the base is discarded by the same rule.
// For the ValidateTag-approved charset, url.PathEscape is the identity map,
// so assigning the escaped suffix to Path (the decoded field) cannot
// double-encode.
func releasesAPIURL(base *url.URL, suffix string) *url.URL {
	u := *base
	u.User = nil
	u.Path = "/repos/" + buildinfo.Owner + "/" + buildinfo.Repo + "/releases/" + suffix
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	return &u
}

// releasePayload is the subset of the GitHub release document the flow reads.
type releasePayload struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func fetchRelease(ctx context.Context, apiURL *url.URL) (*Release, error) {
	if err := checkHop(apiURL); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("release lookup %s: %w", apiURL.Redacted(), err)
	}
	resp, err := httpDo(NewHTTPClient(), req)
	if err != nil {
		if resp != nil {
			resp.Body.Close() // redirect-abort path hands back a (closed) body
		}
		return nil, fmt.Errorf("release lookup %s: %w", apiURL.Redacted(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := readLimited(resp.Body, errSnippetBytes)
		detail := ""
		if len(snippet) > 0 {
			detail = ": " + truncateForLog(string(snippet))
		}
		return nil, fmt.Errorf("release lookup %s: HTTP %d%s", apiURL.Redacted(), resp.StatusCode, detail)
	}
	body, err := readLimited(resp.Body, apiMaxBodyBytes)
	if err != nil {
		return nil, fmt.Errorf("release lookup %s: %w", apiURL.Redacted(), err)
	}
	var payload releasePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("release lookup %s: malformed release JSON: %w", apiURL.Redacted(), err)
	}
	// The tag drives the asset name and URL paths below — a server (or
	// mirror) answer outside the release tag grammar fails closed here.
	if err := ValidateTag(payload.TagName); err != nil {
		return nil, fmt.Errorf("release lookup %s: unusable tag_name: %w", apiURL.Redacted(), err)
	}
	want, err := AssetName(payload.TagName, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return nil, err
	}
	found := false
	rawAssetURL := ""
	available := make([]string, 0, len(payload.Assets))
	for _, a := range payload.Assets {
		available = append(available, a.Name)
		if a.Name == want {
			found = true
			rawAssetURL = a.BrowserDownloadURL
		}
	}
	if !found {
		return nil, fmt.Errorf("release %s: asset %q not found (available: %s)",
			payload.TagName, want, strings.Join(available, ", "))
	}
	if rawAssetURL == "" {
		return nil, fmt.Errorf("release %s: asset %q has empty browser_download_url", payload.TagName, want)
	}
	assetU, err := resolveAssetURL(rawAssetURL, apiURL)
	if err != nil {
		return nil, err
	}
	checksumsU, err := checksumsURLFor(assetU)
	if err != nil {
		return nil, err
	}
	return &Release{
		Tag:          payload.TagName,
		AssetName:    want,
		AssetURL:     assetU.String(),
		ChecksumsURL: checksumsU,
	}, nil
}

// resolveAssetURL turns the release document's browser_download_url into the
// URL the download will use. Default mode keeps GitHub's URL verbatim; a
// custom base rebuilds it onto the base scheme/host while keeping the source
// path and query byte-for-byte (spec §4.2(4): 取原 URL 的 path+query,host/scheme
// 换成 base 的). The source must be an absolute http(s) URL either way —
// relative or exotic answers fail closed.
func resolveAssetURL(rawAssetURL string, base *url.URL) (*url.URL, error) {
	u, err := url.Parse(rawAssetURL)
	if err != nil {
		return nil, fmt.Errorf("release asset URL %q: %w", rawAssetURL, err)
	}
	if !u.IsAbs() || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("release asset URL %q: not an absolute http(s) URL", rawAssetURL)
	}
	_, custom, err := baseAndHost()
	if err != nil {
		return nil, err
	}
	if custom == "" {
		return u, nil
	}
	u2 := *base
	u2.User = nil
	u2.Path = u.Path
	u2.RawPath = u.RawPath
	u2.RawQuery = u.RawQuery
	u2.Fragment = ""
	return &u2, nil
}

// checksumsURLFor derives the checksums.txt URL sitting beside a release
// asset: same directory, fixed name (.goreleaser.yml checksum.name_template).
func checksumsURLFor(assetU *url.URL) (string, error) {
	i := strings.LastIndexByte(assetU.Path, '/')
	if i < 0 {
		return "", fmt.Errorf("asset URL %s: cannot derive %s location", assetU.Redacted(), checksumsName)
	}
	u2 := *assetU
	u2.Path = assetU.Path[:i+1] + checksumsName
	u2.RawPath = ""
	u2.Fragment = ""
	return u2.String(), nil
}
