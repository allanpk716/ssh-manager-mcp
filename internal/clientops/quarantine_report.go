// Package clientops: Plan 34 rev4 §4 — the spawn/status attribution chain.
// When LoadCacheSnapshot fails (bin gone / DEK gone / decrypt error), mcp
// --cache and `cache status` consult QuarantineReport FIRST, so a server-side
// rejection surfaces as what it is instead of a generic missing-cache error.
package clientops

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// QuarantineReport maps a failed cache load to the rev4 §4 report line.
// ok=false → the caller keeps its existing missing/decrypt error text.
//
// Tier 1 — quarantine/manifest.json readable AND parseable AND fresh: full
// attribution (done / done+degraded / started variants). Freshness is the rev4
// time constraint: when cache.meta.json is ON RECORD, the manifest must
// postdate its pulled_at (that meta was written by a successful pull, so an
// older manifest is a superseded quarantine and attributes NOTHING — not even
// tier 2, because a readable stale manifest proves the whole dir is residue of
// an already-healed cycle). A meta that is ABSENT still counts as fresh: the
// only routine that deletes meta is the quarantine itself, so no-meta-on-record
// means no successful pull happened after it — the primary post-quarantine
// spawn (§2 step 5 deleted the meta, step 5b wrote the manifest) lands HERE
// with full detail, never in tier 2.
// Tier 2 — manifest unreadable/corrupt but the quarantine/ dir exists AND no
// meta is on record: detail-free attribution. With a meta on record the dir is
// post-re-enroll residue (a pull succeeded after the manifest went away) and
// must not attribute.
// Tier 3 — nothing quarantine-shaped on disk: not ours.
//
// loadErr is a presence trigger only — the report never inspects error text
// (rev4: attribution is manifest-driven; a bin-missing and a decrypt-failure
// load get the same answer).
func QuarantineReport(loadErr error) (string, bool) {
	dir, _, metaPath, _, err := CachePaths()
	if err != nil {
		return "", false
	}
	if blob, merr := os.ReadFile(filepath.Join(dir, "quarantine", "manifest.json")); merr == nil {
		var m quarantineManifest // T3's writer type is the reader's contract
		if json.Unmarshal(blob, &m) == nil {
			fresh, onRecord := manifestVersusMeta(m.TS, metaPath)
			if onRecord && !fresh {
				// A successful pull postdates this manifest — a superseded
				// quarantine. Attribute nothing. This is also the crash-safe
				// backstop for a failed manifest reset on re-pull (rev4 §4).
				return "", false
			}
			switch {
			case m.State == "done" && len(m.Degraded) > 0:
				return fmt.Sprintf("cache quarantined by server rejection (token revoked?) [DEGRADED: %v] — re-enroll via cache pull with a fresh device code; manual cleanup may be needed", m.Degraded), true
			case m.State == "done":
				return "cache quarantined by server rejection (token revoked?) — re-enroll via cache pull with a fresh device code", true
			case m.State == "started":
				return "cache quarantine was interrupted — the snapshot may still exist; re-enroll via cache pull, or inspect quarantine/manifest.json", true
			}
			return "", false // unknown state — fall through conservatively (not ours)
		}
		// A present-but-corrupt manifest: the destruction may well have run, the
		// report just didn't survive — tier 2 decides below.
	}
	// Tier 2: the dir is the only surviving trace AND no pull is on record.
	if st, serr := os.Stat(filepath.Join(dir, "quarantine")); serr == nil && st.IsDir() && !metaOnRecord(metaPath) {
		return "cache was quarantined (details unavailable — quarantine/manifest.json missing); re-enroll via cache pull", true
	}
	return "", false
}

// manifestVersusMeta compares the manifest timestamp with the last successful
// pull's meta record. onRecord=false: no parseable meta exists — nothing on
// record can supersede the manifest (never pulled, or the quarantine deleted
// the meta in §2 step 5). onRecord=true: fresh is the strict ts > pulled_at.
func manifestVersusMeta(ts int64, metaPath string) (fresh, onRecord bool) {
	mb, err := os.ReadFile(metaPath)
	if err != nil {
		return false, false
	}
	var meta cacheMeta
	if json.Unmarshal(mb, &meta) != nil {
		return false, false
	}
	return ts > meta.PulledAt, true
}

// metaOnRecord reports whether a parseable cache.meta.json exists — i.e. a
// successful pull is on record for this cache dir.
func metaOnRecord(metaPath string) bool {
	mb, err := os.ReadFile(metaPath)
	if err != nil {
		return false
	}
	var meta cacheMeta
	return json.Unmarshal(mb, &meta) == nil
}
