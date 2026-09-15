package updater

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// defaultBaseURL is the GitHub API base used when SSHMGR_UPDATE_BASE is unset
// (spec §4.1).
const defaultBaseURL = "https://api.github.com"

// updateBaseEnv is the env seam (spec §4.6): pointing it at a loopback
// httptest server drives the whole update flow in tests; pointing it at a
// production mirror rebuilds every API/asset URL onto that host (spec
// §4.2(4)).
const updateBaseEnv = "SSHMGR_UPDATE_BASE"

// invalidBaseHost is a sentinel custom host that can never equal a real
// Hostname() output — used to fail closed (block every hop) when the env seam
// holds an unparseable base.
const invalidBaseHost = "\x00invalid-base"

// defaultHosts is the default per-hop host whitelist (spec §4.2(4)). The
// measured 302 hop for this repo's release assets is
// release-assets.githubusercontent.com.
var defaultHosts = map[string]bool{
	"api.github.com":                       true,
	"github.com":                           true,
	"objects.githubusercontent.com":        true,
	"release-assets.githubusercontent.com": true,
}

// maxRedirects caps redirect following. A custom CheckRedirect replaces
// net/http's built-in default of 10, so the cap must be re-enforced here —
// without it a looping mirror would spin until the context deadline.
const maxRedirects = 10

// BaseURL returns the update source base: env SSHMGR_UPDATE_BASE when set to
// non-blank content (trimmed), else the GitHub API default (spec §4.6).
func BaseURL() string {
	if v := strings.TrimSpace(os.Getenv(updateBaseEnv)); v != "" {
		return v
	}
	return defaultBaseURL
}

// IsLoopbackLiteral reports whether host is exactly one of the two loopback
// literals {127.0.0.1, ::1}. The comparison is deliberately string equality
// over the output of url.Parse → URL.Hostname() (which strips port, brackets
// and userinfo): shortened/alternate encodings (127.1, 0x7f.0.0.1,
// 2130706433), resolvable names (localhost, localhost.), zone suffixes
// (::1%eth0) and trailing-dot forms are NOT in the set — spec §4.2(4) pins
// the exact-match semantics and its rejection matrix.
func IsLoopbackLiteral(host string) bool {
	return host == "127.0.0.1" || host == "::1"
}

// baseAndHost resolves the effective base for one update operation: the
// parsed base URL plus the custom host for per-hop validation (empty string =
// default mode, four-host whitelist). A non-default base that fails to parse
// yields both an error and a sentinel custom host, so every consumer fails
// closed.
func baseAndHost() (*url.URL, string, error) {
	raw := BaseURL()
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, invalidBaseHost, fmt.Errorf("invalid %s %q: not an absolute URL", updateBaseEnv, raw)
	}
	if raw == defaultBaseURL {
		return u, "", nil
	}
	return u, strings.ToLower(u.Hostname()), nil
}

// allowedHop is the per-hop transport rule (spec §4.2(4) 判定程序, applied to
// the initial URL and to every redirect hop):
//   - an https hop must land on the whitelist — the four GitHub hosts in
//     default mode, exactly the custom base host once SSHMGR_UPDATE_BASE is
//     set (host compared exactly after lower-casing, so trailing-dot and case
//     games fail); or
//   - an http hop is allowed only when its host is a loopback literal — the
//     test-fake exception, judged per hop (a loopback start does not bless
//     later hops).
//
// Anything else is rejected with the redacted URL.
func allowedHop(u *url.URL, custom string) error {
	host := strings.ToLower(u.Hostname())
	switch u.Scheme {
	case "https":
		if custom != "" {
			if host == custom {
				return nil
			}
		} else if defaultHosts[host] {
			return nil
		}
	case "http":
		if IsLoopbackLiteral(host) {
			return nil
		}
	}
	return fmt.Errorf("blocked redirect/scheme (want https on the host whitelist, or http on a loopback literal): %s", u.Redacted())
}

// checkHop validates one request URL against the transport rule. It is called
// by httpDo for every request — including the initial one, which net/http's
// CheckRedirect never sees — so the first hop is enforced structurally, not by
// caller convention (spec §4.2(4): 初始 URL 与每一跳都要校验).
func checkHop(u *url.URL) error {
	_, custom, err := baseAndHost()
	if err != nil {
		return err
	}
	return allowedHop(u, custom)
}

// NewHTTPClient returns the client for release discovery and downloads: every
// redirect target is judged by allowedHop before the client issues the next
// request, so a hostile or misconfigured hop aborts the transfer before any
// connection is made to it. A garbage SSHMGR_UPDATE_BASE blocks every hop
// (fail closed) instead of falling back to the default whitelist.
func NewHTTPClient() *http.Client {
	_, custom, err := baseAndHost()
	if err != nil {
		custom = invalidBaseHost
	}
	return &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			return allowedHop(req.URL, custom)
		},
	}
}

// httpDo is the request seam (spec §4.6). The production default enforces the
// transport rule on req.URL before issuing anything — this is what makes the
// INITIAL hop a structural guarantee instead of a caller convention
// (CheckRedirect covers the later hops; tests overriding this seam choose
// their own behavior).
var httpDo = func(c *http.Client, req *http.Request) (*http.Response, error) {
	if err := checkHop(req.URL); err != nil {
		return nil, err
	}
	return c.Do(req)
}

// readLimited reads r fully but aborts once more than max bytes arrive —
// release JSON and error snippets are bounded, never trusted unbounded.
func readLimited(r io.Reader, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("response exceeds %d byte limit", max)
	}
	return b, nil
}

// truncateForLog caps a string for inclusion in an error message.
func truncateForLog(s string) string {
	if len(s) > 80 {
		return s[:77] + "..."
	}
	return s
}
