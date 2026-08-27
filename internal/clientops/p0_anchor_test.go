package clientops

// Plan 40 §1 (P0) — anchor fact/policy split. The time anchor is a FACT ("the
// pinned server said THIS at this pull"), so recording it is gated on the
// SAFETY precondition (pin != "" — a pinned TLS response's Date is
// uninjectable), NOT on the pulling process's policy env. The production
// incident this closes: client TUI [s] sync (env-less process) overwrote
// server_anchored=false; every env-carrying mcp --cache then refused to load —
// a self-inflicted loop (third instance of the "env must be on both sides"
// trap). The never-anchors form narrows to plaintext (injectable Date).

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDoPull_P0_PinnedWithoutEnv_Anchors — matrix §1.4 #1+#2: a pinned pull in
// an env-LESS process records ServerAnchored=true, and the env-CARRYING load
// side accepts that cache (the incident's exact combination).
func TestDoPull_P0_PinnedWithoutEnv_Anchors(t *testing.T) {
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "")
	date := time.Now().UTC().Format(http.TimeFormat)
	dir := t.TempDir()
	withDEK(t)
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	url, pin := newPinnedTLSServer(t, snapshotHandler(ptr(date), nil))
	if _, err := DoPull(url, "code", pin, PullOpts{}); err != nil {
		t.Fatalf("pinned pull without env: %v", err)
	}
	m := readMetaForTest(t, dir)
	if !m.ServerAnchored {
		t.Fatal("P0: pinned pull WITHOUT env must record ServerAnchored=true (fact, not policy)")
	}
	// Flip the SAME machine to B-on and load — this exact combination was the
	// production breakage (env-less pull overwrote the anchor, env'd load refused).
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "24h")
	snap, err := LoadCacheSnapshot()
	if err != nil {
		t.Fatalf("env-carrying load must accept the env-less pull's cache: %v", err)
	}
	if snap == nil {
		t.Fatal("snapshot must load")
	}
}

// TestDoPull_P0_PinnedWithoutEnv_BadDate_Refused — matrix §1.4 #3: with env
// absent, a pinned pull STILL refuses a missing/malformed Date (fail-closed:
// the anchor now exists on every pinned pull, so it must be a valid one).
func TestDoPull_P0_PinnedWithoutEnv_BadDate_Refused(t *testing.T) {
	for _, tc := range []struct {
		name string
		date *string
	}{
		{name: "missing", date: nil},
		{name: "malformed", date: ptr("not-a-date")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "")
			url, pin := newPinnedTLSServer(t, snapshotHandler(tc.date, nil))
			dir := t.TempDir()
			withDEK(t)
			withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
			_, err := DoPull(url, "code", pin, PullOpts{})
			if err == nil || !strings.Contains(err.Error(), "no valid Date header") {
				t.Fatalf("want Date refusal without env, got %v", err)
			}
			if _, serr := os.Stat(filepath.Join(dir, "cache.bin")); !os.IsNotExist(serr) {
				t.Fatal("cache.bin must not be written without a valid anchor")
			}
		})
	}
}

// TestDoPull_P0_PinnedWithoutEnv_Skew_Refused — matrix §1.4 #4: with env
// absent, a pinned pull STILL refuses a >1h skewed Date (a wrong anchor is
// worse than none — the rollback gate would refuse the load later).
func TestDoPull_P0_PinnedWithoutEnv_Skew_Refused(t *testing.T) {
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "")
	skewed := time.Now().Add(2 * time.Hour).Format(http.TimeFormat)
	url, pin := newPinnedTLSServer(t, snapshotHandler(ptr(skewed), nil))
	dir := t.TempDir()
	withDEK(t)
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	_, err := DoPull(url, "code", pin, PullOpts{})
	if err == nil || !strings.Contains(err.Error(), "server clock skew too large") {
		t.Fatalf("want skew refusal without env, got %v", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "cache.bin")); !os.IsNotExist(serr) {
		t.Fatal("cache.bin must not be written on skew")
	}
}

// TestDoPull_P0_PlaintextWithoutEnv_NotAnchored — matrix §1.4 #5 second half:
// post-P0 the never-anchors form is plaintext ONLY (its Date is injectable and
// cannot anchor); B-off no longer suppresses anchoring on pinned pulls.
func TestDoPull_P0_PlaintextWithoutEnv_NotAnchored(t *testing.T) {
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "")
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"servers":[],"credentials":[]}`)
	}))
	defer plain.Close()
	dir := t.TempDir()
	withDEK(t)
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	if _, err := DoPull(plain.URL, "code", "", PullOpts{AllowPlain: true}); err != nil {
		t.Fatalf("plaintext B-off pull: %v", err)
	}
	m := readMetaForTest(t, dir)
	if m.ServerAnchored {
		t.Fatal("plaintext must never anchor (injectable Date)")
	}
}

// TestDoPull_P0_RejectPathLeavesOldCacheIntact — matrix §1.4 #7: a refused
// pull (skew gate) writes NOTHING — the pre-existing cache stays
// byte-identical and loadable under the env-carrying side. (Confirmed on
// master pre-change by the xcheck probe; this pins it against regression.)
func TestDoPull_P0_RejectPathLeavesOldCacheIntact(t *testing.T) {
	dir := t.TempDir()
	withDEK(t)
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})

	// 1. healthy anchored pull (env-less, the new normal), with a flippable Date.
	flipped := ptr(time.Now().UTC().Format(http.TimeFormat))
	url, pin := newPinnedTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", *flipped)
		fmt.Fprint(w, `{"servers":[],"credentials":[]}`)
	})
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "")
	if _, err := DoPull(url, "code", pin, PullOpts{}); err != nil {
		t.Fatalf("healthy pull: %v", err)
	}
	sumBin := fileSum(t, filepath.Join(dir, "cache.bin"))
	sumMeta := fileSum(t, filepath.Join(dir, "cache.meta.json"))

	// 2. flip the Date 3h back — skew gate must refuse.
	*flipped = time.Now().Add(-3 * time.Hour).UTC().Format(http.TimeFormat)
	_, err := DoPull(url, "code", pin, PullOpts{})
	if err == nil || !strings.Contains(err.Error(), "server clock skew too large") {
		t.Fatalf("want skew refusal, got %v", err)
	}

	// 3. byte-identical + still loadable under B-on.
	if got := fileSum(t, filepath.Join(dir, "cache.bin")); got != sumBin {
		t.Fatal("cache.bin must be byte-identical after a refused pull")
	}
	if got := fileSum(t, filepath.Join(dir, "cache.meta.json")); got != sumMeta {
		t.Fatal("cache.meta.json must be byte-identical after a refused pull")
	}
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "24h")
	if _, lerr := LoadCacheSnapshot(); lerr != nil {
		t.Fatalf("old cache must stay loadable: %v", lerr)
	}
}

func fileSum(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
