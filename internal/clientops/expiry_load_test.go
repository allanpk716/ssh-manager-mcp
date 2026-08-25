package clientops

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vaultio"
)

// writeCacheFixture seeds a cache dir with a decryptable cache.bin (encrypted
// under the in-memory DEK) plus a meta carrying the given anchor state. It
// RETURNS the provider it installed so tests can hold the ACTIVE seam
// instance — the DEK-deletion assertion in TestLoad_AgedDestroys must observe
// the provider QuarantineCache actually deletes through (T2 review
// Important-1: an outer withDEK before the fixture gets silently overwritten,
// making the assertion vacuous). Carrying-level fix vs the brief: the repo's
// withDEK returns an EMPTY MemKeyProvider, so the key must be generated + Set
// here before Get.
func writeCacheFixture(t *testing.T, dir string, pulledAt int64, anchored bool) *store.MemKeyProvider {
	t.Helper()
	mem := withDEK(t)
	dek, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.Set(dek); err != nil {
		t.Fatal(err)
	}
	blob, err := vaultio.EncryptWithKey(dek, []byte(`{"servers":[],"credentials":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cache.bin"), blob, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeMetaFile(t, dir, pulledAt, anchored); err != nil {
		t.Fatal(err)
	}
	return mem
}

func writeMetaFile(t *testing.T, dir string, pulledAt int64, anchored bool) error {
	t.Helper()
	blob := fmt.Sprintf(`{"url":"u","pulled_at":%d,"server_anchored":%t}`, pulledAt, anchored)
	return os.WriteFile(filepath.Join(dir, "cache.meta.json"), []byte(blob), 0o600)
}

func intactFor(t *testing.T, dir string) bool {
	t.Helper()
	for _, f := range []string{"cache.bin", "cache.meta.json"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			return false
		}
	}
	return true
}

func TestLoad_BOff_Unchanged(t *testing.T) {
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "")
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	writeCacheFixture(t, dir, time.Now().Add(-365*24*time.Hour).Unix(), false) // absurd age, no anchor
	if _, err := LoadCacheSnapshot(); err != nil {
		t.Fatalf("B off must load regardless of age/anchor: %v", err)
	}
}

func TestLoad_InvalidEnv_Refused(t *testing.T) {
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "30m")
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	writeCacheFixture(t, dir, time.Now().Unix(), true)
	_, err := LoadCacheSnapshot()
	if err == nil || !strings.Contains(err.Error(), "invalid SSHMGR_CACHE_MAX_OFFLINE") {
		t.Fatalf("want invalid-env refusal, got %v", err)
	}
	if !intactFor(t, dir) {
		t.Fatal("invalid env must not destroy anything")
	}
}

func TestLoad_MetaMissing_Refused(t *testing.T) {
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "24h")
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	writeCacheFixture(t, dir, time.Now().Unix(), true)
	os.Remove(filepath.Join(dir, "cache.meta.json"))
	_, err := LoadCacheSnapshot()
	if err == nil || !strings.Contains(err.Error(), "missing or corrupt (or this machine never pulled)") {
		t.Fatalf("want meta-missing refusal, got %v", err)
	}
	// Carrying-level fix vs the brief: the test itself removed the meta, so
	// intactFor (both files) can never hold — what must survive is cache.bin.
	if _, serr := os.Stat(filepath.Join(dir, "cache.bin")); serr != nil {
		t.Fatal("meta missing must not destroy")
	}
}

func TestLoad_ProvenanceGate(t *testing.T) {
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "24h")
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	// B-off-era meta: explicit false.
	writeCacheFixture(t, dir, time.Now().Unix(), false)
	_, err := LoadCacheSnapshot()
	if err == nil || !strings.Contains(err.Error(), "no server-anchored time") {
		t.Fatalf("want provenance refusal, got %v", err)
	}
	if !intactFor(t, dir) {
		t.Fatal("provenance must not destroy")
	}
	// Legacy meta shape: no server_anchored field at all → zero value false.
	os.WriteFile(filepath.Join(dir, "cache.meta.json"), []byte(`{"url":"u","pulled_at":123}`), 0o600)
	_, err = LoadCacheSnapshot()
	if err == nil || !strings.Contains(err.Error(), "no server-anchored time") {
		t.Fatalf("want provenance refusal for legacy meta, got %v", err)
	}
}

func TestLoad_WithinWindow_Loads(t *testing.T) {
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "1h")
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	writeCacheFixture(t, dir, time.Now().Add(-30*time.Minute).Unix(), true)
	if _, err := LoadCacheSnapshot(); err != nil {
		t.Fatalf("in-window anchored cache must load: %v", err)
	}
}

func TestLoad_Rollback_Refused(t *testing.T) {
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "24h")
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	writeCacheFixture(t, dir, time.Now().Add(2*time.Hour).Unix(), true) // future anchor
	_, err := LoadCacheSnapshot()
	if err == nil || !strings.Contains(err.Error(), "system clock is behind the snapshot's server time anchor") {
		t.Fatalf("want rollback refusal, got %v", err)
	}
	if !strings.Contains(err.Error(), "fix system time, then run cache pull") {
		t.Fatalf("rollback text must direct clock repair first: %v", err)
	}
	if !intactFor(t, dir) {
		t.Fatal("rollback must not destroy")
	}
}

// TestLoad_AgedDestroys is matrix 3: the six destruction assertions plus the
// post-destruction report path (expired message on the NEXT load too).
func TestLoad_AgedDestroys(t *testing.T) {
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "1h")
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	mem := writeCacheFixture(t, dir, time.Now().Add(-2*time.Hour).Unix(), true)

	_, err := LoadCacheSnapshot()
	if err == nil {
		t.Fatal("aged cache must be refused")
	}
	if !errors.Is(err, ErrCacheQuarantined) {
		t.Fatalf("expiry error must wrap ErrCacheQuarantined, got %v", err)
	}
	if !strings.Contains(err.Error(), "cache snapshot expired (offline") {
		t.Fatalf("expiry text missing: %v", err)
	}
	qdir := filepath.Join(dir, "quarantine")
	entries, _ := os.ReadDir(qdir)
	found := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "cache.bin.quarantined-") {
			found = true
		}
	}
	if !found {
		t.Fatal("cache.bin must be isolated into quarantine/")
	}
	if _, serr := os.Stat(filepath.Join(dir, "cache.meta.json")); !os.IsNotExist(serr) {
		t.Fatal("meta must be deleted (destruction step 4)")
	}
	if _, derr := mem.Get(); derr == nil {
		t.Fatal("DEK must be deleted")
	}
	// manifest reason + post-destruction report path.
	mblob, rerr := os.ReadFile(filepath.Join(qdir, "manifest.json"))
	if rerr != nil || !strings.Contains(string(mblob), `"reason":"`+expiryReason+`"`) {
		t.Fatalf("manifest must carry expiryReason: %v %s", rerr, mblob)
	}
	_, err2 := LoadCacheSnapshot() // next spawn: meta-missing gate fires…
	if err2 == nil || !strings.Contains(err2.Error(), "missing or corrupt") {
		t.Fatalf("post-destruction load should hit the meta gate: %v", err2)
	}
	if msg, ok := QuarantineReport(err2); !ok || !strings.HasPrefix(msg, "cache expired: offline beyond SSHMGR_CACHE_MAX_OFFLINE — snapshot destroyed") {
		t.Fatalf("post-destruction report must be the expired line, got %q ok=%v", msg, ok)
	}
}

// TestLoad_RecheckAborts is matrix 16 branch A: a trusted pull re-anchors
// between the age verdict and the re-check → destruction aborts.
func TestLoad_RecheckAborts(t *testing.T) {
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "1h")
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	writeCacheFixture(t, dir, time.Now().Add(-2*time.Hour).Unix(), true)

	date := time.Now().UTC().Format(http.TimeFormat)
	url, pin := newPinnedTLSServer(t, snapshotHandler(ptr(date), nil))
	ResetExpiryHooksForTest()
	expiryTestHooks.afterAgeCheck = func() {
		if err := DoPull(url, "code", pin, PullOpts{}); err != nil {
			t.Errorf("hook pull: %v", err)
		}
	}
	t.Cleanup(ResetExpiryHooksForTest)
	if _, err := LoadCacheSnapshot(); err != nil {
		t.Fatalf("re-anchored load must succeed: %v", err)
	}
	if !intactFor(t, dir) {
		t.Fatal("abort must leave cache files in place")
	}
}

// TestLoad_RecheckFailure_Refused is matrix 16's injection leg: the re-check
// read fails → refuse WITHOUT destroying (independent copy, not §3.2's).
func TestLoad_RecheckFailure_Refused(t *testing.T) {
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "1h")
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	writeCacheFixture(t, dir, time.Now().Add(-2*time.Hour).Unix(), true)

	ResetExpiryHooksForTest()
	expiryTestHooks.afterAgeCheck = func() {
		os.WriteFile(filepath.Join(dir, "cache.meta.json"), []byte("{not json"), 0o600)
	}
	t.Cleanup(ResetExpiryHooksForTest)
	_, err := LoadCacheSnapshot()
	if err == nil || !strings.Contains(err.Error(), "cache expiry re-check failed") {
		t.Fatalf("want re-check-failure refusal, got %v", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "cache.bin")); serr != nil {
		t.Fatal("re-check failure must not destroy the snapshot")
	}
}

// TestLoad_DestroyRacingPull_Residual is matrix 16 branch B: a pull landing
// AFTER the re-check still gets destroyed — the registered millisecond-window
// residual, nailed behaviorally: outcome = fresh cache quarantined + re-pull
// self-heals.
func TestLoad_DestroyRacingPull_Residual(t *testing.T) {
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "1h")
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	writeCacheFixture(t, dir, time.Now().Add(-2*time.Hour).Unix(), true)

	date := time.Now().UTC().Format(http.TimeFormat)
	url, pin := newPinnedTLSServer(t, snapshotHandler(ptr(date), nil))
	ResetExpiryHooksForTest()
	expiryTestHooks.afterRecheck = func() {
		if err := DoPull(url, "code", pin, PullOpts{}); err != nil {
			t.Errorf("hook pull: %v", err)
		}
	}
	t.Cleanup(ResetExpiryHooksForTest)
	_, err := LoadCacheSnapshot()
	if err == nil || !strings.Contains(err.Error(), "cache snapshot expired") {
		t.Fatalf("residual window must still report expiry: %v", err)
	}
	// Self-heal: one re-pull restores a loadable cache.
	if err := DoPull(url, "code", pin, PullOpts{}); err != nil {
		t.Fatalf("self-heal pull: %v", err)
	}
	if _, err := LoadCacheSnapshot(); err != nil {
		t.Fatalf("post-heal load: %v", err)
	}
}

// TestLoad_Concurrent_NoAnchorWrites is matrix 13: concurrent loads never
// touch meta (the zero-anchor-write design, behaviorally).
func TestLoad_Concurrent_NoAnchorWrites(t *testing.T) {
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "1h")
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	writeCacheFixture(t, dir, time.Now().Unix(), true)
	before, _ := os.ReadFile(filepath.Join(dir, "cache.meta.json"))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = LoadCacheSnapshot()
		}()
	}
	wg.Wait()
	after, _ := os.ReadFile(filepath.Join(dir, "cache.meta.json"))
	if string(before) != string(after) {
		t.Fatalf("meta mutated by loads:\nbefore %s\nafter  %s", before, after)
	}
}

// TestLoad_AgeFromOldAnchor is matrix 18: with a fresh bin but a retained old
// anchor (meta write failed), age is computed from the OLD anchor — asserted
// without claiming a direction (bounded ≤ 2K both ways, spec §2.4).
func TestLoad_AgeFromOldAnchor(t *testing.T) {
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "1h")
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	oldAnchor := time.Now().Add(-90 * time.Minute).Unix()
	writeCacheFixture(t, dir, oldAnchor, true)

	date := time.Now().UTC().Format(http.TimeFormat)
	url, pin := newPinnedTLSServer(t, snapshotHandler(ptr(date), nil))
	FailNextMetaWriteForTest() // pull succeeds, meta write fails, old anchor stays
	if err := DoPull(url, "code", pin, PullOpts{}); err != nil {
		t.Fatalf("pull must survive meta-write failure (WARNING): %v", err)
	}
	m := readMetaForTest(t, dir)
	if m.PulledAt != oldAnchor {
		t.Fatalf("old anchor must be retained, got %d want %d", m.PulledAt, oldAnchor)
	}
	_, err := LoadCacheSnapshot() // 90m > 1h cap → aged per the OLD anchor
	if err == nil || !strings.Contains(err.Error(), "cache snapshot expired") {
		t.Fatalf("age must be judged from the retained old anchor: %v", err)
	}
	// The in-window direction: a 30m-old retained anchor loads fine.
	writeCacheFixture(t, dir, time.Now().Add(-30*time.Minute).Unix(), true)
	FailNextMetaWriteForTest()
	if err := DoPull(url, "code", pin, PullOpts{}); err != nil {
		t.Fatalf("second pull: %v", err)
	}
	if _, err := LoadCacheSnapshot(); err != nil {
		t.Fatalf("in-window old anchor must load: %v", err)
	}
}

// TestQuarantineReport_ReasonDispatch is matrix 12: exact-equality dispatch.
func TestQuarantineReport_ReasonDispatch(t *testing.T) {
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	qdir := filepath.Join(dir, "quarantine")
	os.MkdirAll(qdir, 0o700)
	cases := []struct {
		name     string
		manifest string
		want     string
	}{
		{"expired-done", `{"state":"done","reason":"` + expiryReason + `","ts":100}`, "cache expired: offline beyond SSHMGR_CACHE_MAX_OFFLINE — snapshot destroyed; run cache pull (the device code is still valid unless revoked)"},
		{"expired-started", `{"state":"started","reason":"` + expiryReason + `","ts":100}`, "cache expiry destruction was interrupted — the snapshot may still exist; re-enroll via cache pull, or inspect quarantine/manifest.json"},
		{"server-rejected-done", `{"state":"done","reason":"` + serverRejectedReason + `","ts":100}`, "cache quarantined by server rejection (token revoked?) — re-enroll via cache pull with a fresh device code"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			os.WriteFile(filepath.Join(qdir, "manifest.json"), []byte(tc.manifest), 0o600)
			msg, ok := QuarantineReport(errors.New("load failed"))
			if !ok || !strings.HasPrefix(msg, tc.want) {
				t.Fatalf("dispatch mismatch:\n got %q (ok=%v)\nwant prefix %q", msg, ok, tc.want)
			}
		})
	}
	// degraded variants keep their [DEGRADED: ...] segment.
	os.WriteFile(filepath.Join(qdir, "manifest.json"),
		[]byte(`{"state":"done","reason":"`+expiryReason+`","ts":100,"degraded":["dek"]}`), 0o600)
	msg, _ := QuarantineReport(errors.New("x"))
	if !strings.Contains(msg, "[DEGRADED: [dek]] — snapshot destroyed") {
		t.Fatalf("expired+degraded text wrong: %q", msg)
	}
}
