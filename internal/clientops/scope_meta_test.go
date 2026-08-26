package clientops

import (
	"net/http"
	"testing"
)

// TestDoPullRecordsScopeProvenance (Plan 39, code-review #3): cache.meta
// carries scoped=true ONLY when the serve sent the X-Sshmgr-Snapshot-Scope
// header; a header-less 200 (pre-Plan-39 serve / test stub) records false —
// the honest unverified state that keeps legacy caches from passing as
// cropped (CacheScopeVerified is the read side).
func TestDoPullRecordsScopeProvenance(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", dir)
	withDEKFake(t)

	// Variant A: no scope header (pre-Plan-39 serve shape) → unverified.
	plain := newPinnedSnapshotServer(t, func(r *http.Request) (int, string) {
		return 200, `{"version":1,"servers":[]}`
	})
	if err := DoPull(plain.URL, "c", plain.Pin, PullOpts{}); err != nil {
		t.Fatal(err)
	}
	if CacheScopeVerified() {
		t.Fatal("header-less 200 must record scoped=false (unverified)")
	}

	// Variant B: the Plan-39 header → scoped=true.
	scopedURL, scopedPin := newPinnedTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Sshmgr-Snapshot-Scope", "profile")
		w.Write([]byte(`{"version":1,"servers":[]}`))
	})
	if err := DoPull(scopedURL, "c", scopedPin, PullOpts{}); err != nil {
		t.Fatal(err)
	}
	if !CacheScopeVerified() {
		t.Fatal("X-Sshmgr-Snapshot-Scope: profile must record scoped=true")
	}
}
