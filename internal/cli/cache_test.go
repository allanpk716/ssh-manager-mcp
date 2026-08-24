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

	"ssh-manager-mcp/internal/clientops"
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
	dp := clientops.DekProvider()
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

// withDEK swaps the clientops.DekProvider seam to a fresh in-memory provider for the test, returning it
// so the test can assert the DEK persisted there (not the real keychain).
func withDEK(t *testing.T) *store.MemKeyProvider {
	t.Helper()
	mem := &store.MemKeyProvider{}
	prev := clientops.DekProvider
	clientops.DekProvider = func() store.KeyProvider { return mem }
	t.Cleanup(func() { clientops.DekProvider = prev })
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
	r, err := mcpserver.NewServeRunner(st)
	if err != nil {
		t.Fatalf("NewServeRunner: %v", err)
	}
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

// TestResolvePin, TestPinningTransport_BadPinErrors, and
// TestPinningTransport_NoPeerCert_HardFails moved to
// internal/clientops/pin_test.go with the pinning implementation.

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

// TestCachePullDoesNotPersistCredOn401 pins the rev4 §3 CLI ordering: a failed
// (401) pull must NOT overwrite the persisted credential — the old code stays
// (cachePullCmd persists cache.auth.json only AFTER DoPull returns nil, so a
// mistyped/revoked new code never lands on disk; the next lazy pull keeps using
// the old one). Driven through the full cobra command so the RunE ordering
// itself is under test; the stub is plaintext (the non-trigger face — this test
// is purely about the write-after-success timing, not the quarantine trigger,
// which is covered clientops-side in TestDoPullPinned401Quarantines).
func TestCachePullDoesNotPersistCredOn401(t *testing.T) {
	dir := t.TempDir()
	withEnv(t, map[string]string{
		"SSHMGR_CACHE_DIR": dir,
		"SSHMGR_SERVE_PIN": "", // plaintext stub: no pin anywhere
	})
	withDEK(t)
	if err := clientops.WriteCacheCred(&clientops.CacheCred{URL: "https://old", Token: "old-code", Pin: "sha256:" + strings.Repeat("1", 64)}); err != nil {
		t.Fatal(err)
	}
	// A 401 stub: any bearer is rejected server-side.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, "invalid cache token")
	}))
	t.Cleanup(srv.Close)

	root := NewRootCmd()
	root.SetArgs([]string{"cache", "pull", "--url", srv.URL, "--token", "bad-new-code", "--allow-plaintext"})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	err := root.Execute()
	if err == nil {
		t.Fatal("want 401 error")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("error must come from the 401 path, got: %v", err)
	}
	// cachePullCmd writes cache.auth.json only after a SUCCESSFUL pull — the
	// persisted cred must still be the untouched old one.
	cred, rerr := clientops.ReadCacheCred()
	if rerr != nil || cred == nil || cred.Token != "old-code" {
		t.Fatalf("cred = %+v err=%v — must be untouched old-code", cred, rerr)
	}
}

// TestCacheStatus_AttributesQuarantine is the Plan 34 final-review regression
// pin (Minor 1): in the isolated post-quarantine state (cache.bin gone, a done
// quarantine/manifest.json on disk, no meta on record), `cache status` must
// surface the server-rejection attribution — not a generic missing-cache
// error. This drives the REAL cobra command, so cache.go's QuarantineReport
// wiring (and, since both call sites share the same helper, mcp.go's) can't be
// silently dropped by a future refactor without failing this test.
func TestCacheStatus_AttributesQuarantine(t *testing.T) {
	withDEK(t) // in-memory DEK: LoadCacheSnapshot fails here regardless of machine state
	cacheDir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", cacheDir)

	// Isolation: no cache.bin (quarantined away), no cache.meta.json (deleted
	// by the quarantine itself — meta-absent is the primary post-quarantine
	// shape), and a done manifest → QuarantineReport tier 1 attributes.
	qdir := filepath.Join(cacheDir, "quarantine")
	if err := os.MkdirAll(qdir, 0o700); err != nil {
		t.Fatal(err)
	}
	mfest := fmt.Sprintf(`{"state":"done","reason":"server rejected device code","ts":%d,"steps":{"dek":"ok","auth":"ok","bin":"ok","meta":"ok"}}`, time.Now().Unix())
	if err := os.WriteFile(filepath.Join(qdir, "manifest.json"), []byte(mfest), 0o600); err != nil {
		t.Fatal(err)
	}

	root := NewRootCmd()
	root.SetArgs([]string{"cache", "status"})
	root.SetOut(&bytes.Buffer{})
	errBuf := &bytes.Buffer{}
	root.SetErr(errBuf)
	err := root.Execute()
	if err == nil {
		t.Fatal("cache status must fail when cache.bin is gone")
	}
	if !strings.Contains(err.Error(), "quarantined by server rejection") && !strings.Contains(errBuf.String(), "quarantined by server rejection") {
		t.Fatalf("status must attribute the quarantine (server rejection), got err=%v stderr=%q", err, errBuf.String())
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
