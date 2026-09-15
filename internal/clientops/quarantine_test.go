package clientops

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/store"
)

// fakeDEK backs the DekProvider seam with a MemKeyProvider whose Delete is
// scripted: deleteErr != nil → failure; otherwise the call is recorded.
// QuarantineCache's DEK step only ever calls Delete (never Get/Set), so the
// zero-value mem provider is a fine base.
type fakeDEK struct {
	store.MemKeyProvider
	deleteErr error
	deleted   bool
}

func (f *fakeDEK) Delete() error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = true
	return nil
}

func withDEKFake(t *testing.T) *fakeDEK {
	t.Helper()
	f := &fakeDEK{}
	prev := DekProvider
	DekProvider = func(string) store.KeyProvider { return f }
	t.Cleanup(func() { DekProvider = prev })
	return f
}

// seedCache writes the cache-side artifacts into the CURRENT SSHMGR_CACHE_DIR
// (t.Setenv it first), resolving their paths via the REAL CachePaths /
// CacheCredPath so the quarantine routine is pinned to the same layout pull
// writes (dir/cache.bin, dir/cache.meta.json, dir/cache.auth.json).
func seedCache(t *testing.T, dir string) (bin, meta, cred string) {
	t.Helper()
	d, b, m, _, err := CachePaths()
	if err != nil {
		t.Fatal(err)
	}
	if d != dir {
		t.Fatalf("CachePaths dir = %q, want SSHMGR_CACHE_DIR %q", d, dir)
	}
	c, err := CacheCredPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for p, content := range map[string]string{
		b: "ciphertext-bytes",
		m: `{"url":"https://x","pulled_at":1}`,
		c: `{"url":"https://x","token":"dev-code","pin":"sha256:aa"}`,
	} {
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return b, m, c
}

// TestQuarantineDestroysFourAndWritesManifest: the happy path — DEK deleted,
// auth/meta gone, bin isolated (exactly manifest + one retained copy), manifest
// done with the four step outcomes, nil error; a second run is idempotent.
func TestQuarantineDestroysFourAndWritesManifest(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", dir)
	f := withDEKFake(t)
	bin, meta, cred := seedCache(t, dir)

	res, err := QuarantineCache("server rejected device code")
	if err != nil {
		t.Fatalf("QuarantineCache: %v", err)
	}
	if len(res.Degraded) != 0 || !res.ManifestWritten {
		t.Fatalf("res = %+v, want no degraded + manifest written", res)
	}
	if !f.deleted {
		t.Fatal("DEK provider Delete not called")
	}
	for _, p := range []string{meta, cred} {
		if _, serr := os.Stat(p); !os.IsNotExist(serr) {
			t.Fatalf("%s must be deleted", p)
		}
	}
	// bin isolated: quarantine dir holds exactly manifest.json + one renamed copy.
	qdir := filepath.Join(dir, "quarantine")
	entries, rerr := os.ReadDir(qdir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(entries) != 2 {
		t.Fatalf("quarantine entries = %d, want 2 (manifest + bin)", len(entries))
	}
	if _, serr := os.Stat(bin); !os.IsNotExist(serr) {
		t.Fatal("cache.bin must be gone from the cache dir")
	}
	// The completion manifest records the done state with all four outcomes.
	var mf struct {
		State    string            `json:"state"`
		Steps    map[string]string `json:"steps"`
		Degraded []string          `json:"degraded"`
	}
	blob, rerr := os.ReadFile(filepath.Join(qdir, "manifest.json"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if jerr := json.Unmarshal(blob, &mf); jerr != nil {
		t.Fatalf("manifest unmarshal: %v", jerr)
	}
	if mf.State != "done" || len(mf.Degraded) != 0 || len(mf.Steps) != 4 {
		t.Fatalf("manifest = %+v, want state=done, 4 steps, no degraded", mf)
	}
	// Re-quarantine is idempotent: every target already gone → nil error, no degraded.
	res2, err2 := QuarantineCache("server rejected device code")
	if err2 != nil {
		t.Fatalf("second QuarantineCache: %v, want nil (idempotent)", err2)
	}
	if len(res2.Degraded) != 0 {
		t.Fatalf("second run Degraded = %v, want empty", res2.Degraded)
	}
}

// TestQuarantineDegradedOnDEKFailure: a critical-step failure marks DEGRADED in
// the result AND the returned error (sentinel-wrapped), still completes the
// other steps, and reports honestly.
func TestQuarantineDegradedOnDEKFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", dir)
	f := withDEKFake(t)
	f.deleteErr = errors.New("keyring unavailable")
	seedCache(t, dir)

	res, err := QuarantineCache("server rejected device code")
	if err == nil {
		t.Fatal("want error carrying DEGRADED")
	}
	if !strings.Contains(err.Error(), "DEGRADED") || !strings.Contains(err.Error(), "dek") {
		t.Fatalf("err = %q, want DEGRADED + step name", err)
	}
	if !errors.Is(err, ErrCacheQuarantined) {
		t.Fatal("err must wrap ErrCacheQuarantined (errors.Is-matchable)")
	}
	if len(res.Degraded) != 1 || res.Degraded[0] != "dek" {
		t.Fatalf("res.Degraded = %v, want [dek]", res.Degraded)
	}
	// Other steps STILL ran (best-effort destruction, never rollback).
	if _, serr := os.Stat(filepath.Join(dir, "cache.auth.json")); !os.IsNotExist(serr) {
		t.Fatal("auth.json must still be deleted despite DEK failure")
	}
	if _, serr := os.Stat(filepath.Join(dir, "cache.meta.json")); !os.IsNotExist(serr) {
		t.Fatal("meta.json must still be deleted despite DEK failure")
	}
}

// TestQuarantineIdempotentOnMissingArtifacts: quarantining an empty cache dir
// (nothing ever pulled) is all-absent → NO degraded, nil error (rev4 幂等例外).
func TestQuarantineIdempotentOnMissingArtifacts(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", dir)
	withDEKFake(t)
	res, err := QuarantineCache("server rejected device code")
	if err != nil {
		t.Fatalf("QuarantineCache on empty dir: %v, want nil (idempotent)", err)
	}
	if len(res.Degraded) != 0 {
		t.Fatalf("res.Degraded = %v, want empty (idempotent)", res.Degraded)
	}
}

// TestQuarantineManifestBestEffort: an unwritable quarantine dir must NOT stop
// the destruction — DEK/auth/meta still die, bin rename degrades (its target is
// broken while bin itself is present), the report says manifest unwritten.
func TestQuarantineManifestBestEffort(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", dir)
	f := withDEKFake(t)
	seedCache(t, dir)
	// Pre-create quarantine as a FILE so MkdirAll fails deterministically.
	if err := os.WriteFile(filepath.Join(dir, "quarantine"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := QuarantineCache("server rejected device code")
	if err == nil {
		t.Fatal("bin rename must fail (target is a file) → DEGRADED expected")
	}
	if !strings.Contains(err.Error(), "DEGRADED") {
		t.Fatalf("err = %q, want DEGRADED", err)
	}
	if !errors.Is(err, ErrCacheQuarantined) {
		t.Fatal("err must wrap ErrCacheQuarantined (errors.Is-matchable)")
	}
	if len(res.Degraded) != 1 || res.Degraded[0] != "bin" {
		t.Fatalf("res.Degraded = %v, want [bin]", res.Degraded)
	}
	if res.ManifestWritten {
		t.Fatal("manifest must be reported unwritten")
	}
	if !f.deleted {
		t.Fatal("DEK deletion must still run (manifest is not a precondition)")
	}
	if _, serr := os.Stat(filepath.Join(dir, "cache.auth.json")); !os.IsNotExist(serr) {
		t.Fatal("auth.json must still be deleted")
	}
	if _, serr := os.Stat(filepath.Join(dir, "cache.meta.json")); !os.IsNotExist(serr) {
		t.Fatal("meta.json must still be deleted")
	}
	// bin itself could not move: still in the cache dir (honestly degraded, not lost).
	if _, serr := os.Stat(filepath.Join(dir, "cache.bin")); serr != nil {
		t.Fatalf("cache.bin must remain in place (rename failed): %v", serr)
	}
}

// TestQuarantineCachePathsErrorWrapsSentinel pins the Plan 34 final-review
// Minor 2 fix: when CachePaths itself fails, QuarantineCache must return a
// SENTINEL-wrapped error — DoPull's 401 branch passes a non-nil qerr through
// untouched, so this is the one error path that must stay errors.Is-matchable
// against ErrCacheQuarantined. Deterministic failure construction:
// SSHMGR_CACHE_DIR blank (so CachePaths falls back to os.UserConfigDir) AND
// the platform config env blank (AppData on Windows; XDG_CONFIG_HOME/HOME
// elsewhere). NB the alternative of pointing SSHMGR_CACHE_DIR at a file does
// NOT construct a failure — a non-empty override never consults
// UserConfigDir, so CachePaths cannot fail that way.
func TestQuarantineCachePathsErrorWrapsSentinel(t *testing.T) {
	t.Setenv("SSHMGR_CACHE_DIR", "")
	if runtime.GOOS == "windows" {
		t.Setenv("AppData", "")
	} else {
		t.Setenv("XDG_CONFIG_HOME", "")
		t.Setenv("HOME", "")
	}
	res, err := QuarantineCache("server rejected device code")
	if err == nil {
		t.Fatal("want error when cache paths are unavailable")
	}
	if !errors.Is(err, ErrCacheQuarantined) {
		t.Fatalf("err = %v, must wrap ErrCacheQuarantined", err)
	}
	if !strings.Contains(err.Error(), "cache paths unavailable") {
		t.Fatalf("err = %v, must name the cache-paths failure", err)
	}
	if res.ManifestWritten {
		t.Fatal("no manifest may be claimed when paths never resolved")
	}
}

// TestFileKeyProviderDeleteIsIdempotent: absent key file → nil (rev4 幂等例外 —
// QuarantineCache treats "target already gone" as idempotent completion).
func TestFileKeyProviderDeleteIsIdempotent(t *testing.T) {
	f := &store.FileKeyProvider{Path: filepath.Join(t.TempDir(), "gone.key")}
	if err := f.Delete(); err != nil {
		t.Fatalf("Delete on absent: %v, want nil", err)
	}
}

// --- Plan 34 T4: the DoPull trigger wiring + its harnesses ------------------

// pinnedSnapshotServer is an httptest TLS server whose self-signed cert's SPKI
// fingerprint is the pin a pinned DoPull must verify.
type pinnedSnapshotServer struct {
	URL string
	Pin string
}

// newPinnedSnapshotServer spins an httptest TLS server with a fresh self-signed
// ed25519 cert (same shape as newTLSSnapshotServer in cache_cred_test.go) and
// returns its URL + SPKI pin. The per-request handler decides status + body, so
// ONE server can accept the first pull and 401 the second (revoked) one.
func newPinnedSnapshotServer(t *testing.T, handler func(*http.Request) (int, string)) *pinnedSnapshotServer {
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
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pin := mcpserver.SPKIFingerprint(cert)
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
		status, body := handler(r)
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{tlsCert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return &pinnedSnapshotServer{URL: srv.URL, Pin: pin}
}

// newPlainSnapshotServer spins a plaintext httptest server answering every
// request with a fixed status — the non-trigger face (no pin can be set against
// it, so a 401 from here must NEVER quarantine).
func newPlainSnapshotServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		fmt.Fprint(w, "stub")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestDoPullPinned401Quarantines: pinned TLS + 401 → the artifacts are
// destroyed and the sentinel is returned (T3 review watch item: this asserts
// errors.Is on DoPull's REAL return value — the end-to-end trigger chain, not a
// unit-level fake). The server here is a stub whose TLS cert we pin; the FIRST
// pull succeeds, the SECOND (revoked server-side) 401s.
func TestDoPullPinned401Quarantines(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", dir)
	withDEKFake(t)
	seedCache(t, dir)

	srv := newPinnedSnapshotServer(t, func(r *http.Request) (int, string) {
		if r.Header.Get("Authorization") == "Bearer good-code" {
			return 200, `{"version":1,"servers":[]}`
		}
		return 401, `invalid cache token: revoked`
	})
	// First pull OK (re-writes cache.bin as a real envelope; proves the pin path).
	if _, err := DoPull(srv.URL, "good-code", srv.Pin, PullOpts{}); err != nil {
		t.Fatalf("first pull: %v", err)
	}
	// Revoked second pull → sentinel + destruction.
	_, err := DoPull(srv.URL, "revoked-code", srv.Pin, PullOpts{})
	if !errors.Is(err, ErrCacheQuarantined) {
		t.Fatalf("err = %v, want ErrCacheQuarantined", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "cache.auth.json")); !os.IsNotExist(serr) {
		t.Fatal("auth.json destroyed")
	}
	if _, serr := os.Stat(filepath.Join(dir, "cache.bin")); !os.IsNotExist(serr) {
		t.Fatal("cache.bin quarantined away")
	}
}

// TestDoPullNonTriggerFaces: plaintext 401, network error, and non-401 NEVER
// quarantine (rev4 §3 不触发面).
func TestDoPullNonTriggerFaces(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", dir)
	withDEKFake(t)
	bin, _, _ := seedCache(t, dir)

	// Plaintext 401: no pin → no destruction.
	srv401 := newPlainSnapshotServer(t, 401)
	if _, err := DoPull(srv401.URL, "x", "", PullOpts{AllowPlain: true}); err == nil {
		t.Fatal("want error")
	}
	if _, serr := os.Stat(bin); serr != nil {
		t.Fatal("plaintext 401 must NOT quarantine")
	}
	// Network error against a pinned-but-dead address.
	if _, err := DoPull("https://127.0.0.1:1/", "x", "sha256:"+strings.Repeat("0", 64), PullOpts{}); err == nil {
		t.Fatal("want error")
	}
	if _, serr := os.Stat(bin); serr != nil {
		t.Fatal("network error must NOT quarantine")
	}
	// Non-401 status.
	srv500 := newPlainSnapshotServer(t, 500)
	if _, err := DoPull(srv500.URL, "x", "", PullOpts{AllowPlain: true}); err == nil {
		t.Fatal("want error")
	}
	if _, serr := os.Stat(bin); serr != nil {
		t.Fatal("non-401 must NOT quarantine")
	}
}

// TestMaybeLazyPullNoRetryAfterQuarantine: after a sentinel, the in-process
// flag stops further automatic attempts even though cache.auth.json deletion
// "failed" (simulated by re-seeding it). Also pins rev4 §5's "lazy 哨兵传播":
// the sentinel is PROPAGATED to the caller on the trigger pull itself.
func TestMaybeLazyPullNoRetryAfterQuarantine(t *testing.T) {
	ResetCacheQuarantineForTest()
	t.Cleanup(ResetCacheQuarantineForTest) // the flag is process-lifetime by design — never leak it into other tests

	dir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", dir)
	withDEKFake(t)
	seedCache(t, dir)
	srv := newPinnedSnapshotServer(t, func(*http.Request) (int, string) { return 401, "revoked" })
	if err := WriteCacheCred(&CacheCred{URL: srv.URL, Token: "x", Pin: srv.Pin}); err != nil {
		t.Fatal(err)
	}
	ResetLazyPullBackoffForTest()
	if err := MaybeLazyPull(time.Nanosecond); !errors.Is(err, ErrCacheQuarantined) {
		t.Fatalf("lazy: err = %v, want sentinel", err)
	}
	// Simulate a FAILED auth.json deletion (cred survived) — the flag must still
	// prevent a second automatic attempt in THIS process.
	if err := WriteCacheCred(&CacheCred{URL: srv.URL, Token: "x", Pin: srv.Pin}); err != nil {
		t.Fatal(err)
	}
	ResetLazyPullBackoffForTest()
	if err := MaybeLazyPull(time.Nanosecond); err != nil {
		t.Fatalf("post-quarantine lazy pull must be a silent no-op, got %v", err)
	}
}

// --- Plan 34 T5: the spawn-surface report chain (rev4 §4 + §8 e2e) ----------

// TestE2EQuarantineFullChain: pull ok → server flips to 401 → pull quarantines
// → spawn-time LoadCacheSnapshot error maps to the tier-1 report → re-pull with
// a good code restores (manifest attribution reset).
func TestE2EQuarantineFullChain(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", dir)
	withDEKFake(t)
	revoked := false
	srv := newPinnedSnapshotServer(t, func(*http.Request) (int, string) {
		if revoked {
			return 401, `invalid cache token: revoked`
		}
		return 200, `{"version":1,"servers":[],"credentials":[]}`
	})
	if _, err := DoPull(srv.URL, "good-code", srv.Pin, PullOpts{}); err != nil {
		t.Fatalf("seed pull: %v", err)
	}
	revoked = true
	if _, err := DoPull(srv.URL, "good-code", srv.Pin, PullOpts{}); !errors.Is(err, ErrCacheQuarantined) {
		t.Fatalf("revoked pull: %v", err)
	}
	// Spawn-time surface: load fails, report attributes. NB the quarantine
	// itself deleted cache.meta.json (§2 step 5), so this asserts the
	// meta-absent tier-1 attribution — the PRIMARY post-quarantine shape (see
	// quarantine_report.go for the guard semantics).
	_, lerr := LoadCacheSnapshot()
	if lerr == nil {
		t.Fatal("load must fail post-quarantine")
	}
	if msg, ok := QuarantineReport(lerr); !ok {
		t.Fatalf("report missing: %v", lerr)
	} else if !strings.Contains(msg, "quarantined by server rejection") {
		t.Fatalf("msg = %q", msg)
	}
	// Re-enroll: server accepts again; pull succeeds; attribution resets.
	revoked = false
	if _, err := DoPull(srv.URL, "good-code", srv.Pin, PullOpts{}); err != nil {
		t.Fatalf("re-enroll pull: %v", err)
	}
	os.Remove(filepath.Join(dir, "cache.bin")) // simulate an unrelated later loss
	if _, ok := QuarantineReport(errors.New("cache.bin missing")); ok {
		t.Fatal("post-re-pull bin loss must NOT attribute to quarantine (reset held)")
	}
}

// TestDoPullPinned403UnboundDoesNotQuarantine (Plan 39): the unbound-device-code
// refusal arrives as 403 — deliberately NOT the pinned-401 quarantine trigger
// (rev4 §3's trigger face stays exactly "pinned 401"). The client's cache
// survives; the error names the owner-side repair.
func TestDoPullPinned403UnboundDoesNotQuarantine(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", dir)
	withDEKFake(t)
	bin, _, cred := seedCache(t, dir)

	srv := newPinnedSnapshotServer(t, func(r *http.Request) (int, string) {
		return 403, "device code not bound to a profile — owner: run `sshmgr cache-tokens bind <name> <profile>` on the server"
	})
	_, err := DoPull(srv.URL, "unbound-code", srv.Pin, PullOpts{})
	if err == nil {
		t.Fatal("want error")
	}
	if errors.Is(err, ErrCacheQuarantined) {
		t.Fatalf("pinned 403 must NOT quarantine (Plan 39: the trigger face stays pinned-401-only): %v", err)
	}
	if !strings.Contains(err.Error(), "not bound") {
		t.Fatalf("error must name the unbound-profile cause, got: %v", err)
	}
	for _, f := range []string{bin, cred} {
		if _, serr := os.Stat(f); serr != nil {
			t.Fatalf("%s must survive a 403 refusal: %v", filepath.Base(f), serr)
		}
	}
}

// TestDoPull403DiscriminatesByBody (code-review #6): the unbound advice fires
// ONLY on the serve's own "not bound to a profile" body; any other 403
// (proxy/WAF/fail-closed) reports generically with the body excerpt and never
// sends the owner chasing a pointless bind. Neither face quarantines.
func TestDoPull403DiscriminatesByBody(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", dir)
	withDEKFake(t)
	bin, _, cred := seedCache(t, dir)

	srv := newPinnedSnapshotServer(t, func(r *http.Request) (int, string) {
		if r.Header.Get("Authorization") == "Bearer unbound-code" {
			return 403, "device code not bound to a profile — owner: run `sshmgr cache-tokens bind <name> <profile>` on the server"
		}
		return 403, "403 Forbidden: client IP not in allowlist"
	})
	// The serve's own refusal → unbound advice.
	_, err := DoPull(srv.URL, "unbound-code", srv.Pin, PullOpts{})
	if err == nil || !strings.Contains(err.Error(), "not bound to a profile") {
		t.Fatalf("unbound 403 must name the cause, got: %v", err)
	}
	if errors.Is(err, ErrCacheQuarantined) {
		t.Fatalf("no 403 face may quarantine: %v", err)
	}
	// A proxy-shaped 403 → generic, body excerpted, NO bind advice.
	_, err = DoPull(srv.URL, "proxy-blocked", srv.Pin, PullOpts{})
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "not bound") || strings.Contains(err.Error(), "cache-tokens bind") {
		t.Fatalf("proxy 403 must NOT carry the bind advice, got: %v", err)
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("generic 403 must carry status + body excerpt, got: %v", err)
	}
	for _, f := range []string{bin, cred} {
		if _, serr := os.Stat(f); serr != nil {
			t.Fatalf("%s must survive both 403 faces: %v", filepath.Base(f), serr)
		}
	}
}
