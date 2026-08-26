package clientops

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestInstanceRouting_PullCredLoadQuarantine: one pinned pull with
// Instance="agentA" must land every artifact in instances/agentA/ (bin, meta,
// auth via WriteCacheCredFor), load back through LoadCacheSnapshotFor, and
// QuarantineCacheFor must destroy ONLY that instance's slot.
func TestInstanceRouting_PullCredLoadQuarantine(t *testing.T) {
	userDir := redirectUserConfigDir(t)
	dekDir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DEK_DIR", dekDir) // real per-instance FileKeyProvider
	t.Setenv("SSHMGR_CACHE_DEK", "")

	url, pin := newPinnedTLSServer(t, snapshotHandler(ptr(time.Now().UTC().Format(http.TimeFormat)), nil))
	if err := DoPull(url, "code", pin, PullOpts{Instance: "agentA"}); err != nil {
		t.Fatalf("instance pull: %v", err)
	}
	idir := filepath.Join(userDir, "ssh-manager", "instances", "agentA")
	if _, err := os.Stat(filepath.Join(idir, "cache.bin")); err != nil {
		t.Fatalf("instance bin missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(idir, "cache.meta.json")); err != nil {
		t.Fatalf("instance meta missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dekDir, "cache-dek-agentA.key")); err != nil {
		t.Fatalf("per-instance DEK missing: %v", err)
	}
	// default slot untouched
	if _, err := os.Stat(filepath.Join(userDir, "ssh-manager", "cache.bin")); !os.IsNotExist(err) {
		t.Fatal("default slot must stay empty")
	}

	// cred write/read round-trip in the instance slot
	if err := WriteCacheCredFor("agentA", &CacheCred{URL: url, Token: "code", Pin: pin}); err != nil {
		t.Fatalf("WriteCacheCredFor: %v", err)
	}
	cred, err := ReadCacheCredFor("agentA")
	if err != nil || cred == nil || cred.Token != "code" {
		t.Fatalf("ReadCacheCredFor = %+v, %v", cred, err)
	}
	if _, err := os.Stat(filepath.Join(idir, "cache.auth.json")); err != nil {
		t.Fatalf("instance auth missing: %v", err)
	}

	// load back (real FileKeyProvider DEK)
	snap, err := LoadCacheSnapshotFor("agentA")
	if err != nil || snap == nil {
		t.Fatalf("LoadCacheSnapshotFor: %v", err)
	}

	// quarantine destroys ONLY the instance slot (401-shaped trigger is DoPull's
	// business; here we call the routine directly)
	if _, qerr := QuarantineCacheFor("agentA", serverRejectedReason); qerr != nil {
		t.Fatalf("QuarantineCacheFor: %v", qerr)
	}
	if _, err := os.Stat(filepath.Join(idir, "cache.auth.json")); !os.IsNotExist(err) {
		t.Fatal("instance auth must be deleted by quarantine")
	}
	if _, err := os.Stat(dekDir + string(filepath.Separator) + "cache-dek-agentA.key"); !os.IsNotExist(err) {
		t.Fatal("instance DEK must be deleted by quarantine")
	}
	entries, _ := os.ReadDir(filepath.Join(idir, "quarantine"))
	if len(entries) == 0 {
		t.Fatal("quarantine/ under the instance dir must hold the isolated bin/manifest")
	}

	// a second instance survives untouched
	if err := DoPull(url, "code", pin, PullOpts{Instance: "agentB"}); err != nil {
		t.Fatalf("agentB pull: %v", err)
	}
	if _, err := QuarantineCacheFor("agentA", serverRejectedReason); err != nil {
		t.Fatalf("re-quarantine agentA (idempotent): %v", err)
	}
	if _, err := LoadCacheSnapshotFor("agentB"); err != nil {
		t.Fatalf("agentB must stay loadable: %v", err)
	}
}
