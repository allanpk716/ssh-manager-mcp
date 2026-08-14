package clientops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMaybeLazyPull_MissingCacheWithCred_Pulls(t *testing.T) {
	ResetLazyPullBackoffForTest()
	srv := newTLSSnapshotServer(t)
	withDEK(t)
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": cacheDir})
	if err := WriteCacheCred(&CacheCred{URL: srv.url, Token: "code-123", Pin: srv.fp}); err != nil {
		t.Fatal(err)
	}
	if err := MaybeLazyPull(time.Hour); err != nil {
		t.Fatalf("MaybeLazyPull: %v", err)
	}
	blob, err := os.ReadFile(filepath.Join(cacheDir, "cache.bin"))
	if err != nil || len(blob) < 8 || string(blob[:8]) != "SSHMGRV1" {
		t.Fatalf("lazy pull did not write cache.bin: %v %d bytes", err, len(blob))
	}
}

func TestMaybeLazyPull_BackoffSuppressesRetry(t *testing.T) {
	ResetLazyPullBackoffForTest()
	srv := newTLSSnapshotServer(t)
	withDEK(t)
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": cacheDir})
	if err := WriteCacheCred(&CacheCred{URL: srv.url, Token: "code-123", Pin: srv.fp}); err != nil {
		t.Fatal(err)
	}
	if err := MaybeLazyPull(time.Hour); err != nil {
		t.Fatal(err)
	}
	// Remove the cache; the backoff window (1h) must suppress a second attempt.
	os.Remove(filepath.Join(cacheDir, "cache.bin"))
	if err := MaybeLazyPull(time.Hour); err != nil {
		t.Fatalf("backoff path must not error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "cache.bin")); err == nil {
		t.Fatal("backoff window must suppress an immediate re-pull")
	}
}

func TestMaybeLazyPull_ZeroDisablesEvenWhenCacheMissing(t *testing.T) {
	ResetLazyPullBackoffForTest()
	srv := newTLSSnapshotServer(t)
	withDEK(t)
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": cacheDir})
	if err := WriteCacheCred(&CacheCred{URL: srv.url, Token: "code-123", Pin: srv.fp}); err != nil {
		t.Fatal(err)
	}
	if err := MaybeLazyPull(0); err != nil {
		t.Fatalf("maxAge=0 must be a silent no-op: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "cache.bin")); err == nil {
		t.Fatal("maxAge=0 must not pull, even with cache.bin missing")
	}
}

func TestMaybeLazyPull_FreshCacheNoPull_NoCredNoPull(t *testing.T) {
	ResetLazyPullBackoffForTest()
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": cacheDir})
	// no cred at all → no-op even with stale/missing cache
	if err := MaybeLazyPull(time.Millisecond); err != nil {
		t.Fatalf("no-cred path must be a silent no-op: %v", err)
	}
	// cred present + FRESH cache → no-op (file untouched: write sentinel + now mtime)
	srv := newTLSSnapshotServer(t)
	if err := WriteCacheCred(&CacheCred{URL: srv.url, Token: "code-123", Pin: srv.fp}); err != nil {
		t.Fatal(err)
	}
	ResetLazyPullBackoffForTest()
	sentinel := []byte("SSHMGRV1-sentinel")
	if err := os.WriteFile(filepath.Join(cacheDir, "cache.bin"), sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := MaybeLazyPull(time.Hour); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(cacheDir, "cache.bin"))
	if string(got) != string(sentinel) {
		t.Fatal("fresh cache must not be re-pulled")
	}
}

func TestMaybeLazyPull_CredPinWinsOverEmbeddedStalePin(t *testing.T) {
	// Cert rotation: cred.Pin (new) must override the stale pin embedded in Token.
	ResetLazyPullBackoffForTest()
	srv := newTLSSnapshotServer(t) // "new" cert
	stalePin := "sha256:" + strings.Repeat("0", 64)
	withDEK(t)
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": cacheDir})
	if err := WriteCacheCred(&CacheCred{URL: srv.url, Token: "code-123:" + stalePin, Pin: srv.fp}); err != nil {
		t.Fatal(err)
	}
	if err := MaybeLazyPull(time.Hour); err != nil {
		t.Fatalf("cred.Pin must win over the stale embedded pin: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "cache.bin")); err != nil {
		t.Fatal("pull must have succeeded under the new pin")
	}
}
