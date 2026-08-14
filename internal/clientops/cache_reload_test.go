package clientops

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vaultio"
)

// writeCacheBin encrypts snap under mem's DEK into the test cache dir, exactly
// like cache pull does (mirrors mcp_cache_test.go's hand-rolled cache.bin).
func writeCacheBin(t *testing.T, mem *store.MemKeyProvider, snap *store.Snapshot) {
	t.Helper()
	dir := os.Getenv("SSHMGR_CACHE_DIR")
	blob, err := vaultio.EncryptWithKey(dekOf(t, mem), mustJSON(t, snap))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cache.bin"), blob, 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// dekOf returns the provider's DEK, generating + storing one on first use —
// the MemKeyProvider starts empty and `cache pull`'s loadOrCreateDEK normally
// seeds it; hand-rolled cache.bin writes happen before any pull.
func dekOf(t *testing.T, mem *store.MemKeyProvider) []byte {
	t.Helper()
	k, err := mem.Get()
	if errors.Is(err, store.ErrNotFound) {
		k, err = store.GenerateMasterKey()
		if err != nil {
			t.Fatal(err)
		}
		if err := mem.Set(k); err != nil {
			t.Fatal(err)
		}
		return k
	}
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func seedSnapCLI(t *testing.T, n int) *store.Snapshot {
	t.Helper()
	dir := t.TempDir()
	mk, _ := store.GenerateMasterKey()
	st, err := store.Open(filepath.Join(dir, "s.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	for i := 0; i < n; i++ {
		_, err := st.AddServer(&models.Server{Name: fmt.Sprintf("srv-%d", i), Host: "192.0.2.1", Port: 22, User: "u",
			AuthMethod: models.AuthPassword, CredentialID: cid})
		if err != nil {
			t.Fatal(err)
		}
	}
	snap, err := st.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestCacheReloader_HashChangeDetection(t *testing.T) {
	mem := withDEK(t)
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": cacheDir})
	ResetLazyPullBackoffForTest()

	writeCacheBin(t, mem, seedSnapCLI(t, 1))
	rel := NewCacheReloader(time.Hour)

	// unchanged → (nil, false, nil)
	snap, changed, err := rel.Check()
	if snap != nil || changed || err != nil {
		t.Fatalf("unchanged: got (%v,%v,%v)", snap, changed, err)
	}

	// same content, same size → still unchanged (mtime blind-spot guard)
	future := time.Now().Add(2 * time.Hour)
	bin := filepath.Join(cacheDir, "cache.bin")
	os.Chtimes(bin, future, future)
	snap, changed, err = rel.Check()
	if snap != nil || changed || err != nil {
		t.Fatalf("same-content rewrite must NOT trigger reload: got (%v,%v,%v)", snap, changed, err)
	}

	// different content (extra server), mtime pinned to the SAME future instant
	// → hash catches it even though size may match and mtime is identical.
	writeCacheBin(t, mem, seedSnapCLI(t, 2))
	os.Chtimes(bin, future, future)
	snap, changed, err = rel.Check()
	if err != nil || !changed || snap == nil {
		t.Fatalf("changed content must reload: got (%v,%v,%v)", snap, changed, err)
	}
	if len(snap.Servers) != 2 {
		t.Fatalf("reloaded snapshot has %d servers, want 2", len(snap.Servers))
	}

	// baseline advanced: immediate recheck → unchanged
	snap, changed, err = rel.Check()
	if snap != nil || changed || err != nil {
		t.Fatalf("post-reload recheck: got (%v,%v,%v)", snap, changed, err)
	}
}

func TestCacheReloader_CorruptFileKeepsOldBaseline(t *testing.T) {
	mem := withDEK(t)
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": cacheDir})
	writeCacheBin(t, mem, seedSnapCLI(t, 1))
	rel := NewCacheReloader(time.Hour)
	if _, changed, err := rel.Check(); changed || err != nil {
		t.Fatalf("baseline check: (%v,%v)", changed, err)
	}
	// garbage bytes (a torn write that somehow landed)
	os.WriteFile(filepath.Join(cacheDir, "cache.bin"), []byte("garbage-not-sshmgrv1"), 0o600)
	snap, changed, err := rel.Check()
	if snap != nil || changed || err == nil {
		t.Fatalf("corrupt file must be (nil,false,err): got (%v,%v,%v)", snap, changed, err)
	}
	// baseline NOT advanced: a later good file still reloads
	writeCacheBin(t, mem, seedSnapCLI(t, 2))
	snap, changed, err = rel.Check()
	if err != nil || !changed || snap == nil || len(snap.Servers) != 2 {
		t.Fatalf("recovery after corrupt: got (%v,%v,%v) servers=%d", snap, changed, err, len(snap.Servers))
	}
}

func TestCacheReloader_Unchanged_TriggersInSessionLazyPull(t *testing.T) {
	srv := newTLSSnapshotServer(t)
	mem := withDEK(t)
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": cacheDir})
	ResetLazyPullBackoffForTest()
	if err := WriteCacheCred(&CacheCred{URL: srv.url, Token: "code-123", Pin: srv.fp}); err != nil {
		t.Fatal(err)
	}
	writeCacheBin(t, mem, seedSnapCLI(t, 1))
	// make the cache STALE so the unchanged path wants a refresh
	past := time.Now().Add(-2 * time.Hour)
	os.Chtimes(filepath.Join(cacheDir, "cache.bin"), past, past)

	rel := NewCacheReloader(time.Hour)
	_, changed, err := rel.Check()
	if changed || err != nil {
		t.Fatalf("first check should be unchanged: (%v,%v)", changed, err)
	}
	// the in-session lazy pull ran → cache.bin rewritten (fresh mtime, new content is empty-snap from TLS server)
	if _, err := os.Stat(filepath.Join(cacheDir, "cache.bin")); err != nil {
		t.Fatal(err)
	}
	// next check sees the NEW file → reload (servers now 0, from the TLS stub)
	snap, changed, err := rel.Check()
	if err != nil || !changed {
		t.Fatalf("second check should reload after in-session pull: (%v,%v,%v)", snap, changed, err)
	}
	if len(snap.Servers) != 0 {
		t.Fatalf("post-refresh snapshot should hold the stub's 0 servers, got %d", len(snap.Servers))
	}
}
