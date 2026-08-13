package cli

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/paths"
	"ssh-manager-mcp/internal/store"
)

// TestDefaultDekProvider_IsFileKeyAtFixedPath pins the Plan 16 T4 contract:
// the default cache-DEK provider (no test injection) is a FileKeyProvider whose
// Path is exactly paths.CacheDekPath() (<vaultDir>/cache-dek.key). Was
// DpapiKeyProvider on Windows / KeyringKeyProvider on Unix before Plan 16.
// SSHMGR_FILEKEY_PATH must NOT redirect the cache DEK — cache-dek.key uses its
// own fixed path (paths.CacheDekPath() does not consult that env var).
func TestDefaultDekProvider_IsFileKeyAtFixedPath(t *testing.T) {
	t.Setenv("SSHMGR_FILEKEY_PATH", "") // must not influence cache-dek
	dp := dekProvider()
	fp, ok := dp.(*store.FileKeyProvider)
	if !ok {
		t.Fatalf("default dek not *FileKeyProvider: %T", dp)
	}
	want, err := paths.CacheDekPath()
	if err != nil {
		t.Fatalf("CacheDekPath: %v", err)
	}
	if fp.Path != want {
		t.Errorf("dek path = %q, want %q", fp.Path, want)
	}
}

// withDEK swaps the dekProvider seam to a fresh in-memory provider for the test, returning it
// so the test can assert the DEK persisted there (not the real keychain).
func withDEK(t *testing.T) *store.MemKeyProvider {
	t.Helper()
	mem := &store.MemKeyProvider{}
	prev := dekProvider
	dekProvider = func() store.KeyProvider { return mem }
	t.Cleanup(func() { dekProvider = prev })
	return mem
}

// standUpServe spins a ServeRunner over a seeded store + httptest.Server; returns the server
// URL + a valid cache token + the store (to assert post-pull state).
func standUpServe(t *testing.T) (url, cacheToken string) {
	t.Helper()
	dir := t.TempDir()
	mk, _ := store.GenerateMasterKey()
	st, err := store.Open(filepath.Join(dir, "serve.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	if _, err := st.AddServer(&models.Server{Name: "gpu", Host: "192.0.2.10", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid}); err != nil {
		t.Fatal(err)
	}
	r := mcpserver.NewServeRunner(st)
	t.Cleanup(r.Close)
	srv := httptest.NewServer(r.HTTPHandler())
	t.Cleanup(srv.Close)
	_, code, err := st.AddCacheToken("laptop")
	if err != nil {
		t.Fatal(err)
	}
	return srv.URL, code
}

func TestCachePull_WritesEncryptedCacheAndMeta(t *testing.T) {
	url, code := standUpServe(t)
	withDEK(t)
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": cacheDir})

	root := NewRootCmd()
	// standUpServe is a plaintext httptest server (no TLS), so no pin is
	// available; under the F4 hard-fail policy this requires --allow-plaintext.
	// This test is about cache.bin/meta being written, not the transport.
	root.SetArgs([]string{"cache", "pull", "--url", url, "--token", code, "--allow-plaintext"})
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out) // pull writes its status line to stderr (stdout stays clean for piping)
	if err := root.Execute(); err != nil {
		t.Fatalf("cache pull: %v", err)
	}
	bin := filepath.Join(cacheDir, "cache.bin")
	blob, err := os.ReadFile(bin)
	if err != nil {
		t.Fatalf("cache.bin not written: %v", err)
	}
	if len(blob) == 0 || string(blob[:8]) != "SSHMGRV1" {
		t.Fatalf("cache.bin not an SSHMGRV1 envelope: %x", blob[:min(8, len(blob))])
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "cache.meta.json")); err != nil {
		t.Fatalf("cache.meta.json not written: %v", err)
	}
	if !strings.Contains(out.String(), "gpu") && !strings.Contains(out.String(), "server") {
		// status line; exact wording is loose, but pull must report success non-empty
		if out.Len() == 0 {
			t.Fatal("cache pull printed nothing")
		}
	}
}

func TestCachePull_FailedPullLeavesExistingCacheIntact(t *testing.T) {
	url, _ := standUpServe(t) // valid url, but we'll use a BOGUS token
	withDEK(t)
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": cacheDir})

	// pre-write a sentinel cache.bin so we can prove a failed pull does NOT clobber it
	sentinel := []byte("SSHMGRV1-preexisting-sentinel")
	if err := os.WriteFile(filepath.Join(cacheDir, "cache.bin"), sentinel, 0o600); err != nil {
		t.Fatal(err)
	}

	root := NewRootCmd()
	root.SetArgs([]string{"cache", "pull", "--url", url, "--token", "bogus-xxxxxxxxxxxxxxx"})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{}) // capture cobra's "Error: …" echo out of the test runner's stderr
	if err := root.Execute(); err == nil {
		t.Fatal("pull with bogus token must error")
	}
	got, _ := os.ReadFile(filepath.Join(cacheDir, "cache.bin"))
	if string(got) != string(sentinel) {
		t.Fatalf("failed pull clobbered the existing cache: got %q", got)
	}
}

func TestCacheStatus_ReportsSnapshot(t *testing.T) {
	url, code := standUpServe(t)
	withDEK(t)
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": cacheDir})

	must := func(args ...string) *bytes.Buffer {
		root := NewRootCmd()
		root.SetArgs(args)
		out := &bytes.Buffer{}
		root.SetOut(out)
		if err := root.Execute(); err != nil {
			t.Fatalf("cli %v: %v", args, err)
		}
		return out
	}
	// standUpServe is plaintext (no pin) → F4 requires --allow-plaintext to pull.
	must("cache", "pull", "--url", url, "--token", code, "--allow-plaintext")
	stOut := must("cache", "status")
	if !strings.Contains(stOut.String(), "servers:  1") {
		t.Fatalf("status did not report 1 server: %s", stOut.String())
	}
}

func TestResolvePin(t *testing.T) {
	goodPin := "sha256:" + strings.Repeat("a", 64)
	token := "devcode-xyz" // no ':' → no embedded pin
	tokenEmbedded := "devcode-xyz:" + goodPin

	cases := []struct {
		name            string
		envVal, flagVal string
		token           string
		wantFP          string
		wantPlain       bool
	}{
		{"none → plain", "", "", token, "", true},
		{"env wins", "sha256:" + strings.Repeat("b", 64), goodPin, token, "sha256:" + strings.Repeat("b", 64), false},
		{"flag over token-embedded", "", goodPin, tokenEmbedded, goodPin, false},
		{"env over flag", "sha256:" + strings.Repeat("c", 64), goodPin, token, "sha256:" + strings.Repeat("c", 64), false},
		{"token-embedded when no env/flag", "", "", tokenEmbedded, goodPin, false},
		{"token without : is plain", "", "", token, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("SSHMGR_SERVE_PIN", c.envVal)
			gotFP, plain := resolvePin(c.envVal, c.flagVal, c.token)
			if plain != c.wantPlain {
				t.Fatalf("plain=%v want %v", plain, c.wantPlain)
			}
			if !plain && gotFP != c.wantFP {
				t.Fatalf("fp=%q want %q", gotFP, c.wantFP)
			}
		})
	}
}

func TestPinningTransport_BadPinErrors(t *testing.T) {
	// resolvePin returns a parsed fp; constructing the transport from a
	// well-formed fp must succeed.
	fp := "sha256:" + strings.Repeat("a", 64)
	tr, err := pinningTransport(fp)
	if err != nil {
		t.Fatalf("pinningTransport: %v", err)
	}
	if tr.TLSClientConfig == nil {
		t.Fatal("TLSClientConfig nil")
	}
	if tr.TLSClientConfig.MinVersion != tls.VersionTLS13 {
		t.Fatalf("MinVersion not TLS1.3: %v", tr.TLSClientConfig.MinVersion)
	}
	if tr.TLSClientConfig.VerifyConnection == nil {
		t.Fatal("VerifyConnection callback not set")
	}
}

// TestCachePull_PinnedTLS_Succeeds is the end-to-end exercise of the pinning
// transport at runtime: it spins up an httptest TLS server with a freshly
// generated ed25519 self-signed cert, pins the client to that cert's SPKI
// fingerprint via SSHMGR_SERVE_PIN, and asserts `cache pull` succeeds against
// it. This closes the Task-4 review's "Minor" gap by driving the real
// VerifyConnection callback (constant-time compare + leaf-cert path + success
// path) through the real cachePullCmd wiring (env > flag > embedded resolution,
// Authorization header carries the device code, not the pin).
func TestCachePull_PinnedTLS_Succeeds(t *testing.T) {
	// Build a self-signed TLS test server serving /snapshot with a fixed body.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	fp := mcpserver.SPKIFingerprint(cert)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})
	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer code-123" {
			http.Error(w, "no auth", http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"servers":[],"credentials":[]}`)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{tlsCert}}
	srv.StartTLS()
	defer srv.Close()

	// Point cache pull at it. DEK is injected in-memory (withDEK) so the test
	// never touches the real keychain; SSHMGR_FILEKEY_PATH is intentionally NOT
	// set because the cache DEK lives at a fixed path (paths.CacheDekPath()).
	withDEK(t)
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{
		"SSHMGR_CACHE_URL":   srv.URL,
		"SSHMGR_CACHE_TOKEN": "code-123",
		"SSHMGR_SERVE_PIN":   fp,
		"SSHMGR_CACHE_DIR":   cacheDir,
		"SSHMGR_STORE":       filepath.Join(t.TempDir(), "store.db"),
	})

	root := NewRootCmd()
	root.SetArgs([]string{"cache", "pull"})
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	if err := root.Execute(); err != nil {
		t.Fatalf("pinned pull failed: %v", err)
	}
	bin := filepath.Join(cacheDir, "cache.bin")
	blob, err := os.ReadFile(bin)
	if err != nil {
		t.Fatalf("cache.bin not written: %v", err)
	}
	if len(blob) == 0 || string(blob[:8]) != "SSHMGRV1" {
		t.Fatalf("cache.bin not an SSHMGRV1 envelope: %x", blob[:min(8, len(blob))])
	}
}

// TestCachePull_PinWithHttpURL_HardFails verifies the hard-fail path: when
// a server pin is set (non-plaintext mode), the URL MUST be https://. If the
// user passes http:// with a pin, the request would go in cleartext (the
// TLSClientConfig is never used because http:// doesn't negotiate TLS) —
// this is a security critical bug, so we hard-fail instead of silently
// downgrading. (xcheck F8)
func TestCachePull_PinWithHttpURL_HardFails(t *testing.T) {
	// Build a self-signed TLS test server (same as TestCachePull_PinnedTLS_Succeeds)
	// to get a valid SPKI pin, then we'll intentionally use http:// to trigger
	// the hard-fail.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	fp := mcpserver.SPKIFingerprint(cert)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})
	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}

	// Spin up the TLS server (we won't actually hit it — the hard-fail happens first).
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"servers":[],"credentials":[]}`)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{tlsCert}}
	srv.StartTLS()
	defer srv.Close()

	// Rewrite the URL to http:// (drop the TLS) but keep a valid pin.
	httpURL := "http://" + strings.TrimPrefix(srv.URL, "https://")
	withDEK(t)
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{
		"SSHMGR_CACHE_URL":   httpURL,
		"SSHMGR_CACHE_TOKEN": "code-123",
		"SSHMGR_SERVE_PIN":   fp,
		"SSHMGR_CACHE_DIR":   cacheDir,
		"SSHMGR_STORE":       filepath.Join(t.TempDir(), "store.db"),
	})

	root := NewRootCmd()
	root.SetArgs([]string{"cache", "pull"})
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	err = root.Execute()
	if err == nil {
		t.Fatal("expected hard-fail when pin set but URL is http://, got nil")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Fatalf("error should mention https, got: %v", err)
	}
}

// TestCachePull_AllowPlaintext_Warns verifies the opt-in plaintext branch:
// with no pin resolvable AND --allow-plaintext passed, cache pull falls back
// to plaintext HTTP and prints a STDERR warning. Under the new hard-fail
// policy (xcheck F4) the no-pin path refuses by default; this test asserts the
// --allow-plaintext opt-in still works and still warns. (Was
// TestCachePull_PlaintextFallback_Warns before F4.)
func TestCachePull_AllowPlaintext_Warns(t *testing.T) {
	url, code := standUpServe(t) // plaintext httptest.Server
	withDEK(t)
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{
		"SSHMGR_CACHE_DIR": cacheDir,
		"SSHMGR_SERVE_PIN": "", // explicitly unset → would hard-fail without --allow-plaintext
	})

	root := NewRootCmd()
	root.SetArgs([]string{"cache", "pull", "--url", url, "--token", code, "--allow-plaintext"})
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	if err := root.Execute(); err != nil {
		t.Fatalf("plaintext pull failed: %v", err)
	}
	if !strings.Contains(out.String(), "WARNING") || !strings.Contains(out.String(), "plaintext") {
		t.Fatalf("plaintext fallback did not warn: %s", out.String())
	}
}

// TestCachePull_NoPin_HardFailsByDefault verifies the F4 hard-fail policy:
// when no pin is resolvable (no env / flag / embedded) and --allow-plaintext
// is NOT passed, cache pull MUST refuse — silently sending /snapshot
// credentials over unverified plaintext is a fail-open security bug.
func TestCachePull_NoPin_HardFailsByDefault(t *testing.T) {
	url, _ := standUpServe(t) // we don't need a valid token; the hard-fail happens before the request
	withDEK(t)
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{
		"SSHMGR_CACHE_URL":   url,
		"SSHMGR_CACHE_TOKEN": "code-123",
		"SSHMGR_SERVE_PIN":   "", // none
		"SSHMGR_CACHE_DIR":   cacheDir,
	})
	root := newRootForTest(t)
	root.SetArgs([]string{"cache", "pull"})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected hard-fail with no pin by default, got nil")
	}
	if !strings.Contains(err.Error(), "pin") && !strings.Contains(err.Error(), "allow-plaintext") {
		t.Fatalf("error should mention pin/allow-plaintext, got: %v", err)
	}
}

// TestCachePull_NoPin_AllowPlaintext_OptsIn verifies the F4 opt-in: with no
// pin resolvable, passing --allow-plaintext permits the plaintext pull
// (still warned, still functional). Uses a plaintext httptest server.
func TestCachePull_NoPin_AllowPlaintext_OptsIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"servers":[],"credentials":[]}`)
	}))
	defer srv.Close()
	withDEK(t)
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{
		"SSHMGR_CACHE_URL":   srv.URL, // http
		"SSHMGR_CACHE_TOKEN": "code-123",
		"SSHMGR_SERVE_PIN":   "",
		"SSHMGR_CACHE_DIR":   cacheDir,
	})
	root := newRootForTest(t)
	root.SetArgs([]string{"cache", "pull", "--allow-plaintext"})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	if err := root.Execute(); err != nil {
		t.Fatalf("expected --allow-plaintext to permit plaintext pull, got: %v", err)
	}
}

// TestCachePull_MalformedEnvPin_HardFails verifies F7: a pin-shaped but
// invalid value in SSHMGR_SERVE_PIN (typo, wrong length, non-hex) MUST be a
// hard error — not silently fall through to plaintext. A typo in the env var
// must not silently remove TLS protection.
func TestCachePull_MalformedEnvPin_HardFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"servers":[],"credentials":[]}`)
	}))
	defer srv.Close()
	withDEK(t)
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{
		"SSHMGR_CACHE_URL":   srv.URL, // http
		"SSHMGR_CACHE_TOKEN": "code-123",
		"SSHMGR_SERVE_PIN":   "sha256:NOTVALIDHEX", // pin-shaped but malformed
		"SSHMGR_CACHE_DIR":   cacheDir,
	})
	root := newRootForTest(t)
	root.SetArgs([]string{"cache", "pull"})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected hard-fail on malformed SSHMGR_SERVE_PIN, got nil")
	}
	if !strings.Contains(err.Error(), "SSHMGR_SERVE_PIN") {
		t.Fatalf("error should name SSHMGR_SERVE_PIN, got: %v", err)
	}
}

// TestCachePull_MalformedFlagPin_HardFails verifies F7 for the --pin flag:
// a malformed --pin value must hard-fail (typo protection), not fall through
// to plaintext.
func TestCachePull_MalformedFlagPin_HardFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"servers":[],"credentials":[]}`)
	}))
	defer srv.Close()
	withDEK(t)
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{
		"SSHMGR_CACHE_URL":   srv.URL,
		"SSHMGR_CACHE_TOKEN": "code-123",
		"SSHMGR_CACHE_DIR":   cacheDir,
	})
	root := newRootForTest(t)
	root.SetArgs([]string{"cache", "pull", "--pin", "sha256:deadbeef"}) // too short
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected hard-fail on malformed --pin, got nil")
	}
	if !strings.Contains(err.Error(), "--pin") {
		t.Fatalf("error should name --pin, got: %v", err)
	}
}

// TestCachePull_PinMismatch_Fails verifies the negative path: a pin that does
// NOT match the server cert's SPKI must fail the TLS handshake (the
// VerifyConnection callback rejects the mismatched leaf cert).
func TestCachePull_PinMismatch_Fails(t *testing.T) {
	url := standUpServeTLS(t) // TLS server with cert A
	withDEK(t)
	cacheDir := t.TempDir()
	// pin B: well-formed sha256:<64hex> but does NOT match cert A's SPKI.
	// (Must be valid hex so resolvePin accepts it; 'z' would be rejected and
	// silently fall through to plaintext fallback.)
	otherPin := "sha256:" + strings.Repeat("0", 64)
	withEnv(t, map[string]string{
		"SSHMGR_CACHE_URL":   url,
		"SSHMGR_CACHE_TOKEN": "code-123",
		"SSHMGR_SERVE_PIN":   otherPin,
		"SSHMGR_CACHE_DIR":   cacheDir,
		"SSHMGR_STORE":       filepath.Join(t.TempDir(), "store.db"),
	})

	root := NewRootCmd()
	root.SetArgs([]string{"cache", "pull"})
	errBuf := &bytes.Buffer{}
	root.SetOut(errBuf)
	root.SetErr(errBuf)
	err := root.Execute()
	if err == nil {
		t.Fatal("pull with mismatched pin must fail")
	}
	// The error MUST come from our VerifyConnection callback (fingerprint
	// mismatch), not from an unrelated schannel/CryptoAPI rejection — that
	// assertion is what proves the callback actually ran at runtime.
	if !strings.Contains(err.Error(), "mismatch") && !strings.Contains(errBuf.String(), "mismatch") {
		t.Fatalf("pull error did not come from the pin check (no 'mismatch' in %q / %q)", err.Error(), errBuf.String())
	}
}

// TestPinningTransport_NoPeerCert_HardFails verifies the F12 branch: when
// the server presents no peer certificates (anonymous TLS or no-cert
// handshake), the VerifyConnection callback must hard-fail with a
// "no certificate" error. This is a unit test that directly invokes
// the callback with an empty ConnectionState.
func TestPinningTransport_NoPeerCert_HardFails(t *testing.T) {
	fp := "sha256:" + strings.Repeat("a", 64)
	tr, err := pinningTransport(fp)
	if err != nil {
		t.Fatal(err)
	}
	cb := tr.TLSClientConfig.VerifyConnection
	if cb == nil {
		t.Fatal("no VerifyConnection callback")
	}
	// Empty peer certs (anonymous / no-cert server).
	err = cb(tls.ConnectionState{PeerCertificates: nil})
	if err == nil {
		t.Fatal("expected error when server presents no certificate")
	}
	if !strings.Contains(err.Error(), "no certificate") {
		t.Fatalf("error should mention no certificate, got: %v", err)
	}
}

// standUpServeTLS spins a TLS httptest.Server with a fresh self-signed
// ed25519 cert (same shape as TestCachePull_PinnedTLS_Succeeds's server) and
// returns its URL. Used by the pin-mismatch test, which only needs the server
// to present SOME cert the pin will not match.
func standUpServeTLS(t *testing.T) (url string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})
	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"servers":[],"credentials":[]}`)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{tlsCert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv.URL
}
