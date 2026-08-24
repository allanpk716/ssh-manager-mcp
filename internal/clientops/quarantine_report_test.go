package clientops

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestQuarantineReportChain pins rev4 §4's three-tier attribution + time guard.
// The manifest JSON shape is T3's quarantineManifest ({state,reason,ts,steps,
// degraded}) and the meta shape is cacheMeta ({url,pulled_at}) — read via the
// real CachePaths layout under SSHMGR_CACHE_DIR.
func TestQuarantineReportChain(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", dir)

	// Tier 3: nothing quarantined → not our report.
	if _, ok := QuarantineReport(errors.New("cache.bin missing")); ok {
		t.Fatal("no quarantine dir → no attribution")
	}

	// Tier 2: dir exists, manifest unwritten (and no meta on record).
	if err := os.MkdirAll(filepath.Join(dir, "quarantine"), 0o700); err != nil {
		t.Fatal(err)
	}
	msg, ok := QuarantineReport(errors.New("cache.bin missing"))
	if !ok || msg != "cache was quarantined (details unavailable — quarantine/manifest.json missing); re-enroll via cache pull" {
		t.Fatalf("tier2: ok=%v msg=%q", ok, msg)
	}

	// Tier 1: fresh manifest (ts newer than meta's pulled_at) → full attribution.
	if err := os.WriteFile(filepath.Join(dir, "cache.meta.json"), []byte(`{"url":"u","pulled_at":100}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "quarantine", "manifest.json"),
		[]byte(`{"state":"done","reason":"server rejected device code","ts":200,"steps":{"dek":"ok","auth":"ok","bin":"ok","meta":"ok"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	msg, ok = QuarantineReport(errors.New("cache.bin missing"))
	if !ok || msg != "cache quarantined by server rejection (token revoked?) — re-enroll via cache pull with a fresh device code" {
		t.Fatalf("tier1 done: ok=%v msg=%q", ok, msg)
	}

	// Time guard: an OLD manifest (ts <= meta.pulled_at) must NOT attribute —
	// a re-pull happened after the quarantine, so this bin-missing is unrelated.
	// NB: not even tier 2 — the readable-but-stale manifest proves the whole
	// quarantine dir is superseded residue (see report: plan-skeleton deviation).
	if err := os.WriteFile(filepath.Join(dir, "quarantine", "manifest.json"),
		[]byte(`{"state":"done","reason":"x","ts":50}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := QuarantineReport(errors.New("cache.bin missing")); ok {
		t.Fatal("stale manifest (pre-dates the last pull) must fall through to missing-cache")
	}

	// Degraded variant text.
	if err := os.WriteFile(filepath.Join(dir, "quarantine", "manifest.json"),
		[]byte(`{"state":"done","ts":300,"degraded":["dek"],"steps":{"dek":"boom"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cache.meta.json"), []byte(`{"url":"u","pulled_at":100}`), 0o600); err != nil {
		t.Fatal(err)
	}
	msg, ok = QuarantineReport(errors.New("cache.bin missing"))
	if !ok || msg != "cache quarantined by server rejection (token revoked?) [DEGRADED: [dek]] — re-enroll via cache pull with a fresh device code; manual cleanup may be needed" {
		t.Fatalf("degraded: ok=%v msg=%q", ok, msg)
	}

	// Interrupted variant.
	if err := os.WriteFile(filepath.Join(dir, "quarantine", "manifest.json"),
		[]byte(`{"state":"started","ts":400}`), 0o600); err != nil {
		t.Fatal(err)
	}
	msg, ok = QuarantineReport(errors.New("decrypt failed"))
	if !ok || msg != "cache quarantine was interrupted — the snapshot may still exist; re-enroll via cache pull, or inspect quarantine/manifest.json" {
		t.Fatalf("interrupted: ok=%v msg=%q", ok, msg)
	}

	// Meta ABSENT is the NORMAL post-quarantine shape — §2 step 5 deletes
	// cache.meta.json as part of the destruction, so a readable manifest must
	// still attribute in full (nothing on record can supersede it: only a
	// successful pull writes meta, and the quarantine just deleted it).
	// Plan-skeleton deviation: the sketched guard skipped tier 1 here, which
	// would push the primary flow into tier 2's factually-wrong "manifest.json
	// missing" text and fail the T5 e2e.
	if err := os.Remove(filepath.Join(dir, "cache.meta.json")); err != nil {
		t.Fatal(err)
	}
	msg, ok = QuarantineReport(errors.New("cache decrypt failed"))
	if !ok || msg != "cache quarantine was interrupted — the snapshot may still exist; re-enroll via cache pull, or inspect quarantine/manifest.json" {
		t.Fatalf("meta-absent started: ok=%v msg=%q", ok, msg)
	}

	// Post-re-enroll residue: the successful pull reset the manifest (T5 reset
	// line) and left meta on record — the surviving quarantine/ dir alone must
	// NOT attribute a later unrelated loss.
	if err := os.Remove(filepath.Join(dir, "quarantine", "manifest.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cache.meta.json"), []byte(`{"url":"u","pulled_at":500}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := QuarantineReport(errors.New("cache.bin missing")); ok {
		t.Fatal("dir residue after a recorded pull must NOT attribute")
	}
}
