package clientops

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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

// TestInstanceRouting_Pinned401QuarantinesOwnInstance: a revoked device code
// (pinned 401) on one instance's pull must quarantine THAT instance's slot —
// bin isolated under instances/<name>/quarantine/, per-instance DEK deleted —
// while a sibling instance keeps its cache and stays loadable.
func TestInstanceRouting_Pinned401QuarantinesOwnInstance(t *testing.T) {
	userDir := redirectUserConfigDir(t)
	dekDir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DEK_DIR", dekDir) // real per-instance FileKeyProvider
	t.Setenv("SSHMGR_CACHE_DEK", "")

	revoked := false
	srv := newPinnedSnapshotServer(t, func(*http.Request) (int, string) {
		if revoked {
			return 401, `invalid cache token: revoked`
		}
		return 200, `{"servers":[],"credentials":[]}`
	})

	// (a) seed two instances with a good code
	for _, name := range []string{"agentA", "agentB"} {
		if err := DoPull(srv.URL, "good-code", srv.Pin, PullOpts{Instance: name}); err != nil {
			t.Fatalf("%s seed pull: %v", name, err)
		}
	}

	// (b)+(c) flip to 401: agentA's next pull must quarantine ONLY agentA
	revoked = true
	err := DoPull(srv.URL, "good-code", srv.Pin, PullOpts{Instance: "agentA"})
	if !errors.Is(err, ErrCacheQuarantined) {
		t.Fatalf("revoked instance pull must wrap ErrCacheQuarantined, got %v", err)
	}
	idir := filepath.Join(userDir, "ssh-manager", "instances", "agentA")
	if _, serr := os.Stat(filepath.Join(idir, "cache.bin")); !os.IsNotExist(serr) {
		t.Fatal("agentA cache.bin must be gone (isolated to instances/agentA/quarantine/)")
	}
	qentries, _ := os.ReadDir(filepath.Join(idir, "quarantine"))
	isolated := false
	for _, e := range qentries {
		if strings.HasPrefix(e.Name(), "cache.bin.quarantined-") {
			isolated = true
		}
	}
	if !isolated {
		t.Fatal("agentA bin must be isolated under instances/agentA/quarantine/")
	}
	if _, serr := os.Stat(filepath.Join(dekDir, "cache-dek-agentA.key")); !os.IsNotExist(serr) {
		t.Fatal("agentA per-instance DEK must be deleted by the 401 quarantine")
	}

	// (d) the sibling survives untouched and stays loadable; the default slot
	// was never created.
	if _, serr := os.Stat(filepath.Join(userDir, "ssh-manager", "instances", "agentB", "cache.bin")); serr != nil {
		t.Fatalf("agentB cache.bin must survive agentA's quarantine: %v", serr)
	}
	if _, lerr := LoadCacheSnapshotFor("agentB"); lerr != nil {
		t.Fatalf("agentB must stay loadable: %v", lerr)
	}
	if _, serr := os.Stat(filepath.Join(userDir, "ssh-manager", "cache.bin")); !os.IsNotExist(serr) {
		t.Fatal("default slot must stay empty")
	}
}
