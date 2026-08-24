package clientops

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	DekProvider = func() store.KeyProvider { return f }
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

// TestFileKeyProviderDeleteIsIdempotent: absent key file → nil (rev4 幂等例外 —
// QuarantineCache treats "target already gone" as idempotent completion).
func TestFileKeyProviderDeleteIsIdempotent(t *testing.T) {
	f := &store.FileKeyProvider{Path: filepath.Join(t.TempDir(), "gone.key")}
	if err := f.Delete(); err != nil {
		t.Fatalf("Delete on absent: %v, want nil", err)
	}
}
