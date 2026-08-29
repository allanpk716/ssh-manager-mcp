package updater

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestBaseURL(t *testing.T) {
	t.Setenv(updateBaseEnv, "")
	if got := BaseURL(); got != "https://api.github.com" {
		t.Fatalf("BaseURL() with unset env = %q, want https://api.github.com", got)
	}
	t.Setenv(updateBaseEnv, "http://127.0.0.1:8080")
	if got := BaseURL(); got != "http://127.0.0.1:8080" {
		t.Fatalf("BaseURL() with env set = %q, want http://127.0.0.1:8080", got)
	}
	// Blank / whitespace-only counts as unset (spec §4.6: 显式设定才生效).
	t.Setenv(updateBaseEnv, "   ")
	if got := BaseURL(); got != "https://api.github.com" {
		t.Fatalf("BaseURL() with blank env = %q, want default", got)
	}
	t.Setenv(updateBaseEnv, "  https://mirror.example.com  ")
	if got := BaseURL(); got != "https://mirror.example.com" {
		t.Fatalf("BaseURL() with padded env = %q, want trimmed https://mirror.example.com", got)
	}
}

func TestIsLoopbackLiteral(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		{"localhost", false},         // a name that resolves — not a literal (spec 钉死)
		{"localhost.", false},        // trailing dot
		{"127.1", false},             // shortened IPv4
		{"0x7f.0.0.1", false},        // hex IPv4 variant
		{"2130706433", false},        // integer form
		{"127.0.0.1.", false},        // trailing dot
		{"::1%eth0", false},          // IPv6 zone (post-Hostname() form)
		{"[::1]", false},             // bracketed — not a Hostname() output
		{"127.0.0.1:8080", false},    // host:port — not a Hostname() output
		{"127.0.0.1:80@evil", false}, // userinfo spoof as raw host string
		{"::ffff:127.0.0.1", false},
		{"8.8.8.8", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := IsLoopbackLiteral(tc.host); got != tc.want {
			t.Errorf("IsLoopbackLiteral(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

func TestAllowedHop(t *testing.T) {
	cases := []struct {
		name    string
		rawURL  string
		custom  string // "" = default mode (4-host whitelist)
		wantErr bool
	}{
		// --- default mode ---
		{"default https api", "https://api.github.com/repos/x/y", "", false},
		{"default https github.com", "https://github.com/x/y", "", false},
		{"default https objects", "https://objects.githubusercontent.com/x", "", false},
		{"default https release-assets", "https://release-assets.githubusercontent.com/x", "", false},
		{"default https evil", "https://evil.example.com/x", "", true},
		{"default https host case-insensitive", "https://API.GitHub.Com/repos/x", "", false},
		{"default https trailing dot", "https://api.github.com./repos/x", "", true},
		{"default https loopback not whitelisted", "https://127.0.0.1/x", "", true},
		{"default http loopback allowed", "http://127.0.0.1:9000/x", "", false},
		{"default http ipv6 loopback allowed", "http://[::1]:9000/x", "", false},
		{"default http localhost rejected", "http://localhost:9000/x", "", true},
		{"default http short ipv4 rejected", "http://127.1/x", "", true},
		{"default http hex ipv4 rejected", "http://0x7f.0.0.1/x", "", true},
		{"default http ipv6 zone rejected", "http://[::1%25eth0]/x", "", true},
		{"default http trailing dot rejected", "http://localhost./x", "", true},
		{"default userinfo spoof rejected", "http://127.0.0.1:80@evil.example.com/x", "", true},
		{"default non-loopback http rejected", "http://192.0.2.9/x", "", true},
		{"default ftp scheme rejected", "ftp://127.0.0.1/x", "", true},
		{"default empty host rejected", "https:///x", "", true},

		// --- custom base mode (whitelist replaced by the base host) ---
		{"custom https base host", "https://mirror.example.com/x", "mirror.example.com", false},
		{"custom https base host case", "https://MIRROR.Example.COM/x", "mirror.example.com", false},
		{"custom https github host rejected", "https://api.github.com/x", "mirror.example.com", true},
		{"custom https other host rejected", "https://release-assets.githubusercontent.com/x", "mirror.example.com", true},
		{"custom http base host non-loopback rejected", "http://mirror.example.com/x", "mirror.example.com", true},
		{"custom http loopback hop still allowed", "http://127.0.0.1:8080/x", "mirror.example.com", false},
		{"custom https loopback hop rejected", "https://127.0.0.1/x", "mirror.example.com", true},
		{"custom invalid sentinel blocks all", "https://api.github.com/x", invalidBaseHost, true},
	}
	for _, tc := range cases {
		u, err := url.Parse(tc.rawURL)
		if err != nil {
			t.Fatalf("%s: url.Parse(%q): %v", tc.name, tc.rawURL, err)
		}
		err = allowedHop(u, tc.custom)
		if gotErr := err != nil; gotErr != tc.wantErr {
			t.Errorf("%s: allowedHop(%q, custom=%q) error = %v, wantErr %v",
				tc.name, tc.rawURL, tc.custom, err, tc.wantErr)
		}
	}
}

// fetchViaClient runs one GET through the production client (and the httpDo
// seam) and returns the body.
func fetchViaClient(rawURL string) (string, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := httpDo(NewHTTPClient(), req)
	if err != nil {
		if resp != nil {
			resp.Body.Close() // redirect-abort path hands back a (closed) body
		}
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

// TestNewHTTPClientPerHopChecks drives the real redirect machinery against an
// httptest loopback server: every redirect target is judged per hop before a
// connection to it is attempted (spec §4.2(4)). The initial request URL is
// validated by the callers via checkHop (net/http never invokes
// CheckRedirect for the first request) — covered by the discover/download
// tests.
func TestNewHTTPClientPerHopChecks(t *testing.T) {
	t.Setenv(updateBaseEnv, "") // default whitelist; loopback http hops allowed

	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hop1":
			http.Redirect(w, r, srvURL+"/hop2", http.StatusFound)
		case "/hop2":
			http.Redirect(w, r, srvURL+"/final", http.StatusFound)
		case "/final":
			fmt.Fprint(w, "ok")
		case "/to-evil-http":
			http.Redirect(w, r, "http://evil.example.com/x", http.StatusFound)
		case "/to-evil-https":
			http.Redirect(w, r, "https://evil.example.com/x", http.StatusFound)
		case "/to-short-ipv4": // loopback VARIANT — must not pass
			http.Redirect(w, r, "http://127.1/x", http.StatusFound)
		case "/to-trailing-dot": // loopback VARIANT — must not pass
			http.Redirect(w, r, "http://localhost./x", http.StatusFound)
		case "/to-nonloopback-http": // base is loopback but this hop is not — http forbidden
			http.Redirect(w, r, "http://192.0.2.9/x", http.StatusFound)
		case "/loop":
			http.Redirect(w, r, srvURL+"/loop", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	// Allowed: a chain of loopback http hops rides to the end (逐跳环回放行).
	body, err := fetchViaClient(srvURL + "/hop1")
	if err != nil {
		t.Fatalf("loopback redirect chain: %v", err)
	}
	if strings.TrimSpace(body) != "ok" {
		t.Fatalf("loopback redirect chain body = %q, want ok", body)
	}

	// Blocked: each redirect target fails its per-hop judgment (no dial is
	// ever made to the targets — CheckRedirect aborts first).
	for _, p := range []string{
		"/to-evil-http",
		"/to-evil-https",
		"/to-short-ipv4",
		"/to-trailing-dot",
		"/to-nonloopback-http",
	} {
		if _, err := fetchViaClient(srvURL + p); err == nil || !strings.Contains(err.Error(), "blocked") {
			t.Errorf("%s: want blocked error, got %v", p, err)
		}
	}

	// Redirect cap: setting CheckRedirect replaces net/http's built-in limit
	// of 10, so the cap lives inside our CheckRedirect.
	if _, err := fetchViaClient(srvURL + "/loop"); err == nil || !strings.Contains(err.Error(), "10 redirects") {
		t.Errorf("redirect loop: want 'stopped after 10 redirects' error, got %v", err)
	}
}

// TestCustomBasePinsRedirectsToBaseHost — custom base replaces the whitelist:
// a redirect to any GitHub host must be rejected (spec §4.2(4) 白名单换为 base 宿主).
func TestCustomBasePinsRedirectsToBaseHost(t *testing.T) {
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/to-github":
			http.Redirect(w, r, "https://api.github.com/x", http.StatusFound)
		case "/to-githubassets":
			http.Redirect(w, r, "https://release-assets.githubusercontent.com/x", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL
	t.Setenv(updateBaseEnv, srv.URL)

	for _, p := range []string{"/to-github", "/to-githubassets"} {
		if _, err := fetchViaClient(srvURL + p); err == nil || !strings.Contains(err.Error(), "blocked") {
			t.Errorf("%s: want blocked (custom base replaces whitelist), got %v", p, err)
		}
	}
}

// TestInvalidBaseEnvFailsClosed — a garbage base env must block every path:
// discovery fails at base resolution, download at the initial hop check, and
// the client's redirect layer at the sentinel host — never a fallback to the
// default whitelist.
func TestInvalidBaseEnvFailsClosed(t *testing.T) {
	t.Setenv(updateBaseEnv, "::::not-a-url") // url.Parse error → sentinel host

	if _, err := LatestRelease(context.Background()); err == nil {
		t.Fatalf("LatestRelease: want base-resolution error, got nil")
	}
	dir := t.TempDir()
	if _, err := DownloadAsset(context.Background(), "http://127.0.0.1:1/x.zip", "", dir); err == nil {
		t.Fatalf("DownloadAsset: want initial-hop error, got nil")
	}
	if entries := dirEntriesFor(t, dir); len(entries) != 0 {
		t.Errorf("destDir not clean: %v", entries)
	}

	// Redirect layer: with the sentinel custom host, an https hop to a
	// default-whitelist host must be blocked — proving no fallback to the
	// default whitelist. (http loopback hops stay allowed by design: the
	// loopback exception is judged per hop, base-independently.)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://api.github.com/x", http.StatusFound)
	}))
	defer srv.Close()
	if _, err := fetchViaClient(srv.URL + "/r"); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("redirect with invalid base env: want blocked error, got %v", err)
	}
}

func dirEntriesFor(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}
