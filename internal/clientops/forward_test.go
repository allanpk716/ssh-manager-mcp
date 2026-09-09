package clientops

// Plan 48 §2.1/§4 — the pin-forward client's branch table. Every branch text
// is a verbatim contract: the assertions byte-compare err.Error() against the
// constants in forward.go (one character off fails the test). The test servers
// are httptest TLS servers reached through the forwarder's real
// pinningTransport path, so the wire assertions below run over actual pinned
// TLS exactly like production.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/mcpserver"
)

// testPin is a well-formed (but meaningless) SPKI pin for constructor-level
// tests; branch tests derive the pin from the httptest server's own cert.
var testPin = "sha256:" + strings.Repeat("a", 64)

// newHostKey generates a throwaway ed25519 host key.
func newHostKey(t *testing.T) (blob []byte, pub ssh.PublicKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	pub, err = ssh.NewPublicKey(priv.Public())
	if err != nil {
		t.Fatalf("ssh.NewPublicKey: %v", err)
	}
	return pub.Marshal(), pub
}

// newTestForwarder builds a fully-capable forwarder whose pin is the httptest
// server's own leaf certificate — the production client construction (pinning
// transport + no-follow + timeout) end to end. The token is composite
// ("<code>:<pin>") so every branch test also locks SplitTokenPin stripping:
// the Authorization header must carry the BARE code.
func newTestForwarder(t *testing.T, srv *httptest.Server) *PinForwarder {
	t.Helper()
	pin := mcpserver.SPKIFingerprint(srv.Certificate())
	f, err := NewPinForwarder(CacheCred{URL: srv.URL, Token: "devcode-1:" + pin, Pin: pin})
	if err != nil {
		t.Fatalf("NewPinForwarder: %v", err)
	}
	return f
}

// assertPinPost checks the wire shape of one forwarding POST (§1.1 request).
func assertPinPost(t *testing.T, r *http.Request, blob []byte, host string, port int) {
	t.Helper()
	if r.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", r.Method)
	}
	if r.URL.Path != "/pin-hostkey" {
		t.Errorf("path = %q, want /pin-hostkey", r.URL.Path)
	}
	if got := r.Header.Get("Authorization"); got != "Bearer devcode-1" {
		t.Errorf("Authorization = %q, want the bare code after SplitTokenPin strip", got)
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("read body: %v", err)
		return
	}
	var req pinForwardRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Errorf("body is not the §1.1 JSON shape: %v (%.100s)", err, raw)
		return
	}
	if req.Host != host || req.Port != port {
		t.Errorf("body host/port = %q/%d, want %q/%d", req.Host, req.Port, host, port)
	}
	if req.KeyBlob != base64.StdEncoding.EncodeToString(blob) {
		t.Error("key_blob is not the base64(std) of the marshaled key")
	}
}

// TestPinForwardTexts_SpecVerbatim byte-compares every branch text against
// literals transcribed from the spec (§2.1 table + the §4 fence). The
// branch-mapping test above compares builder output against itself — it locks
// the WIRING; this one locks the WORDING: any drift of forward.go from the
// spec's verbatim contract fails here.
func TestPinForwardTexts_SpecVerbatim(t *testing.T) {
	const host = "192.168.1.108"
	const port = 22
	specTexts := []struct {
		name string
		got  string
		want string
	}{
		{"401", forwardMsg401,
			"device code rejected (invalid or revoked) — ask the owner to check cache-tokens; your local cache is NOT affected by this attempt"},
		{"403", forwardMsg403(host, port),
			"the broker refused pin forwarding for 192.168.1.108:22 — the target's server entry is probably not granted to this device's bound profile; ask the owner to grant it (or rebind via cache-tokens bind) and cache pull again"},
		{"404", forwardMsg404,
			"the broker does not support pin forwarding (the owner must upgrade the broker to >= v0.15.0, then retry)"},
		{"409 equal=false", forwardMsg409Diff(host, port),
			"the broker already has a different pin for 192.168.1.108:22 — run cache pull and retry; if it still fails, ask the owner to compare fingerprints (servers pin-hostkey)"},
		{"plaintext", forwardMsgPlaintext(host, port),
			"pin forwarding requires a pinned TLS server — no plaintext forwarding (set the server pin used by cache pull); the host key for 192.168.1.108:22 stays unpinned"},
		{"main", forwardMainMessage(host, port, "SHA256:AbCd"),
			"host key for 192.168.1.108:22 is unknown and cannot be pinned here (presented fingerprint: SHA256:AbCd). Retry while the broker is reachable — the pin is then forwarded and audited automatically — or ask the owner to run: sshmgr servers pin-hostkey <name> --fingerprint SHA256:AbCd"},
	}
	for _, tc := range specTexts {
		if tc.got != tc.want {
			t.Errorf("%s text drifted from the spec:\n got: %q\nwant: %q", tc.name, tc.got, tc.want)
		}
	}
}

// TestPinForwarder_Forward_BranchTexts walks the §2.1 branch table: one
// httptest server per status, each response text byte-compared against the
// verbatim contract.
func TestPinForwarder_Forward_BranchTexts(t *testing.T) {
	blob, pub := newHostKey(t)
	fp := ssh.FingerprintSHA256(pub)
	const host = "192.168.1.108"
	const port = 22

	cases := []struct {
		name      string
		status    int
		body      string
		want      string // exact Error() text; "" = want nil error
		wantEqual bool   // errors.Is(err, ErrPinForwardEqual)
	}{
		{"201 created", http.StatusCreated, `{"fingerprint":"SHA256:abc","server_name":"板"}`, "", false},
		{"401", http.StatusUnauthorized, "Unauthorized", forwardMsg401, false},
		{"403", http.StatusForbidden, "forbidden", forwardMsg403(host, port), false},
		{"404 old broker", http.StatusNotFound, "404 page not found", forwardMsg404, false},
		{"409 equal=false", http.StatusConflict, `{"error":"already pinned","equal":false}`, forwardMsg409Diff(host, port), false},
		{"409 equal=true", http.StatusConflict, `{"error":"already pinned","equal":true}`, forwardMsg409Equal(host, port), true},
		{"400 passthrough json", http.StatusBadRequest, `{"error":"unparseable key_blob"}`,
			"pin forward for 192.168.1.108:22: broker returned 400 — unparseable key_blob", false},
		{"413 passthrough plain", http.StatusRequestEntityTooLarge, "request body too large",
			"pin forward for 192.168.1.108:22: broker returned 413 — request body too large", false},
		{"500", http.StatusInternalServerError, "boom", forwardMainMessage(host, port, fp), false},
		{"503", http.StatusServiceUnavailable, "", forwardMainMessage(host, port, fp), false},
		{"302 unfollowed", http.StatusFound, "", forwardMainMessage(host, port, fp), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertPinPost(t, r, blob, host, port)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			err := newTestForwarder(t, srv).Forward(host, port, blob)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Forward: want nil, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Forward: want error, got nil")
			}
			if err.Error() != tc.want {
				t.Fatalf("branch text mismatch (verbatim contract):\n got: %q\nwant: %q", err.Error(), tc.want)
			}
			if got := errors.Is(err, ErrPinForwardEqual); got != tc.wantEqual {
				t.Fatalf("errors.Is(err, ErrPinForwardEqual) = %v, want %v", got, tc.wantEqual)
			}
		})
	}
}

// TestPinForwarder_Forward_RedirectNotFollowed proves the 302 is consumed as
// the final response (ErrUseLastResponse — the DoPull precedent): the redirect
// target is never dialed and the failure lands on the main message.
func TestPinForwarder_Forward_RedirectNotFollowed(t *testing.T) {
	blob, pub := newHostKey(t)
	fp := ssh.FingerprintSHA256(pub)
	var followed bool
	mux := http.NewServeMux()
	mux.HandleFunc("/pin-hostkey", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	})
	mux.HandleFunc("/elsewhere", func(w http.ResponseWriter, r *http.Request) {
		followed = true
		w.WriteHeader(http.StatusCreated)
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	err := newTestForwarder(t, srv).Forward("h", 22, blob)
	if followed {
		t.Fatal("the 302 was followed — redirects must never be followed (Plan 37 §2.0)")
	}
	want := forwardMainMessage("h", 22, fp)
	if err == nil || err.Error() != want {
		t.Fatalf("unfollowed redirect must land on the main message:\n got: %v\nwant: %q", err, want)
	}
}

// TestPinForwarder_Forward_Unreachable_MainMessage: a refused connection is
// the §4 main message, and the message carries the TARGET's host:port plus
// its presented fingerprint — not the broker's address.
func TestPinForwarder_Forward_Unreachable_MainMessage(t *testing.T) {
	blob, pub := newHostKey(t)
	fp := ssh.FingerprintSHA256(pub)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // closed port → connection refused

	f, err := NewPinForwarder(CacheCred{URL: "https://" + addr, Token: "devcode-1", Pin: testPin})
	if err != nil {
		t.Fatal(err)
	}
	got := f.Forward("192.168.1.108", 22, blob)
	want := forwardMainMessage("192.168.1.108", 22, fp)
	if got == nil || got.Error() != want {
		t.Fatalf("unreachable broker must render the main message:\n got: %v\nwant: %q", got, want)
	}
}

// TestPinForwarder_Forward_Timeout_MainMessage: a hanging broker must not hang
// the SSH handshake — the (test-shrunk) client timeout trips and renders the
// §4 main message within bounded time (T8b's mechanism).
func TestPinForwarder_Forward_Timeout_MainMessage(t *testing.T) {
	blob, pub := newHostKey(t)
	fp := ssh.FingerprintSHA256(pub)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1 * time.Second) // the 200ms client timeout trips long before this lands
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	f := newTestForwarder(t, srv)
	f.client.Timeout = 200 * time.Millisecond // test-only shrink of forwardTimeout

	start := time.Now()
	err := f.Forward("192.168.1.108", 22, blob)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Forward blocked for %v — the timeout must bound the handshake path", elapsed)
	}
	want := forwardMainMessage("192.168.1.108", 22, fp)
	if err == nil || err.Error() != want {
		t.Fatalf("timeout must render the main message:\n got: %v\nwant: %q", err, want)
	}
}

// TestPinForwarder_Forward_401_CacheUntouched: the Plan-34 quarantine lives
// ONLY on the pull path (§1.2) — a 401 on the forward path is an ordinary
// failure and must leave every cache file byte-identical, no quarantine dir.
func TestPinForwarder_Forward_401_CacheUntouched(t *testing.T) {
	blob, _ := newHostKey(t)
	dir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", dir)
	if err := WriteCacheCredFor("", &CacheCred{URL: "https://broker:7878", Token: "devcode-1", Pin: testPin}); err != nil {
		t.Fatal(err)
	}
	_, bin, _, _, err := CachePathsFor("")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("ciphertext-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	authPath := filepath.Join(dir, "cache.auth.json")
	credBefore, rerr := os.ReadFile(authPath)
	if rerr != nil {
		t.Fatal(rerr)
	}
	binBefore, rerr := os.ReadFile(bin)
	if rerr != nil {
		t.Fatal(rerr)
	}

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	err = newTestForwarder(t, srv).Forward("192.168.1.108", 22, blob)
	if err == nil || err.Error() != forwardMsg401 {
		t.Fatalf("401 text mismatch:\n got: %v\nwant: %q", err, forwardMsg401)
	}
	credAfter, rerr := os.ReadFile(authPath)
	if rerr != nil {
		t.Fatalf("cache.auth.json was touched by the forward path: %v", rerr)
	}
	binAfter, rerr := os.ReadFile(bin)
	if rerr != nil {
		t.Fatalf("cache.bin was touched by the forward path: %v", rerr)
	}
	if !bytes.Equal(credBefore, credAfter) || !bytes.Equal(binBefore, binAfter) {
		t.Fatal("a 401 on the forward path must not modify any cache file")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() == "quarantine" {
			t.Fatal("a quarantine run left its directory behind — the forward path must never destroy the cache")
		}
	}
}

// TestPinForwarder_NoCredential_FailsClosedToMainMessage: a deleted
// cache.auth.json means no forwarding capability whose failures look like an
// unreachable broker (§2.1 last row) — the §4 main message, never a silent
// local fallback.
func TestPinForwarder_NoCredential_FailsClosedToMainMessage(t *testing.T) {
	blob, pub := newHostKey(t)
	fp := ssh.FingerprintSHA256(pub)
	f, err := NewPinForwarder(CacheCred{})
	if err != nil {
		t.Fatalf("a missing credential must construct a no-capability forwarder, got: %v", err)
	}
	err = f.Forward("192.168.1.108", 22, blob)
	want := forwardMainMessage("192.168.1.108", 22, fp)
	if err == nil || err.Error() != want {
		t.Fatalf("missing cache.auth.json must fail closed with the main message:\n got: %v\nwant: %q", err, want)
	}
}

// TestPinForwarder_PlaintextRefusal: pin empty → the §2.1 plaintext refusal,
// verbatim, WITHOUT any dial (a dial would surface the main message instead).
func TestPinForwarder_PlaintextRefusal(t *testing.T) {
	blob, _ := newHostKey(t)
	// Closed port on purpose: if the implementation wrongly dialed, the error
	// would be the main message, not the refusal.
	f, err := NewPinForwarder(CacheCred{URL: "http://127.0.0.1:1", Token: "devcode-1", Pin: ""})
	if err != nil {
		t.Fatalf("no pin → no scheme requirement; construction must succeed: %v", err)
	}
	err = f.Forward("192.168.1.108", 22, blob)
	want := forwardMsgPlaintext("192.168.1.108", 22)
	if err == nil || err.Error() != want {
		t.Fatalf("plaintext refusal text mismatch:\n got: %v\nwant: %q", err, want)
	}
}

// TestNewPinForwarder_Construction locks the constructor contract: scheme
// sanity with a pin, the 10s timeout, the no-follow redirect policy, the
// composite-token strip, and the trailing-slash trim.
func TestNewPinForwarder_Construction(t *testing.T) {
	if _, err := NewPinForwarder(CacheCred{URL: "http://broker:7878", Token: "devcode-1", Pin: testPin}); err == nil {
		t.Fatal("a non-https URL with a pin must fail at construction")
	}
	f, err := NewPinForwarder(CacheCred{URL: "https://broker:7878/", Token: "devcode-1:" + testPin, Pin: testPin})
	if err != nil {
		t.Fatal(err)
	}
	if f.client.Timeout != forwardTimeout {
		t.Fatalf("client timeout = %v, want forwardTimeout (%v)", f.client.Timeout, forwardTimeout)
	}
	if f.client.CheckRedirect == nil {
		t.Fatal("CheckRedirect not set")
	}
	if err := f.client.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("CheckRedirect must return ErrUseLastResponse, got %v", err)
	}
	if f.client.Transport == nil {
		t.Fatal("pinned transport missing")
	}
	if f.code != "devcode-1" {
		t.Fatalf("code = %q, want the bare device code (composite token stripped)", f.code)
	}
	if f.url != "https://broker:7878" {
		t.Fatalf("url = %q, want the trailing slash trimmed", f.url)
	}
}
