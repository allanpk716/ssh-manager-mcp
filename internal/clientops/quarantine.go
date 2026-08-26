// Package clientops: Plan 34 rev4 §2 — the client-side cache destruction
// routine. Called from exactly two authoritative trigger points: DoPull's
// pinned-401 server rejection (Plan 34, T4) and LoadCacheSnapshot's
// positive-age-expiry destruction (Plan 37 §3.4, gated on server-anchored
// expiry evidence plus a re-check confirmation). Every other failure class
// (network, TLS, non-401, plaintext pull, clock rollback, missing/corrupt
// meta) never reaches this file.
package clientops

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ErrCacheQuarantined is the Plan 34 sentinel: a PINNED server answered 401
// and the local cache was quarantined (rev4 §3). errors.Is-matchable; the
// wrapped text carries the DEGRADED step list for display only — the client's
// decision never depends on the server-supplied reason.
var ErrCacheQuarantined = errors.New("cache quarantined by server rejection")

// QuarantineResult reports per-step outcomes (rev4 §2). Steps maps the four
// destruction steps (dek/auth/bin/meta) to "ok" / "ok(absent)" / the failure
// text. Degraded lists the CRITICAL steps (dek/auth/bin) that failed; meta is
// non-critical and never appears there. ManifestWritten is false when the
// quarantine dir was unwritable (best-effort, never a precondition).
type QuarantineResult struct {
	Steps           map[string]string
	Degraded        []string
	ManifestWritten bool
}

// quarantineManifest is the cross-process report the spawn/status banner (T5,
// §4) reads: state started → done, with full step outcomes at done.
type quarantineManifest struct {
	State    string            `json:"state"` // started | done
	Reason   string            `json:"reason"`
	TS       int64             `json:"ts"`
	Steps    map[string]string `json:"steps,omitempty"`
	Degraded []string          `json:"degraded,omitempty"`
}

// QuarantineCache destroys the local cache per Plan 34 rev4 §2, DEK-first so a
// crash at ANY point leaves the ciphertext undecryptable at worst (crash-safe;
// no auto-resume — residue surfaces via the manifest at the next spawn and
// self-heals on re-enroll):
//
//  0. MkdirAll(<cacheDir>/quarantine/) + best-effort manifest {started} — NEVER a precondition;
//  1. DEK delete via the DekProvider seam (production: FileKeyProvider.Delete
//     on paths.CacheDekPath(), so SSHMGR_CACHE_DEK is honored; absent = idempotent success);
//  2. cache.auth.json delete (device code plaintext, zero tolerance);
//  3. cache.bin → quarantine/cache.bin.quarantined-<unix秒> rename (same-dir
//     subdir keeps it same-volume; single retained copy — a new isolation drops the old);
//  4. cache.meta.json delete (non-critical: failure never degrades);
//  5. best-effort manifest {done, steps, degraded}.
//
// Critical steps are dek/auth/bin; ANY error on them records DEGRADED —
// honestly, never silently. The rev4 idempotent exception: a destruction
// target that is ALREADY GONE counts as success, so re-quarantines and racing
// processes converge without damage. Cross-process mutual exclusion is
// deliberately absent (rev4: idempotence bounds the damage to reporting
// imprecision only).
//
// The returned error is non-nil ONLY in the DEGRADED case (always wrapping
// ErrCacheQuarantined); a clean or idempotent completion returns nil — DoPull
// (T4) raises the sentinel for the trigger itself.
func QuarantineCache(reason string) (QuarantineResult, error) {
	res := QuarantineResult{Steps: map[string]string{}}
	dir, bin, meta, _, err := CachePaths()
	if err != nil {
		// Sentinel-wrapped (Plan 34 final review, Minor 2): DoPull's 401 branch
		// passes a non-nil qerr through untouched, so this — the one
		// QuarantineCache error raised BEFORE any step ran — must stay
		// errors.Is-matchable against ErrCacheQuarantined like every other.
		return res, fmt.Errorf("%w: cache paths unavailable: %v", ErrCacheQuarantined, err)
	}
	qdir := filepath.Join(dir, "quarantine")

	// Step 0: intent manifest (best-effort — failure only logs).
	if mkErr := os.MkdirAll(qdir, 0o700); mkErr == nil {
		res.ManifestWritten = writeQuarantineManifest(qdir, &quarantineManifest{State: "started", Reason: reason, TS: time.Now().Unix()})
	} else {
		fmt.Fprintf(os.Stderr, "cache QUARANTINE: manifest dir unavailable (best-effort skipped): %v\n", mkErr)
	}

	// Step 1: DEK first — the key dies before anything else moves. The seam's
	// declared type is KeyProvider (Get/Set only); Delete is the optional
	// capability this routine consumes — an in-process provider without it has
	// nothing on disk, which counts as success.
	if d, ok := DekProvider("").(interface{ Delete() error }); ok {
		if dErr := d.Delete(); dErr != nil {
			res.Steps["dek"] = dErr.Error()
			res.Degraded = append(res.Degraded, "dek")
		} else {
			res.Steps["dek"] = "ok"
		}
	} else {
		res.Steps["dek"] = "ok(no-delete-provider)"
	}

	// Steps 2-4: auth, bin rename, meta — absent targets are idempotent success.
	credPath, _ := CacheCredPath()
	res.Steps["auth"] = removeOrRecord(credPath, "auth", true, &res.Degraded)
	res.Steps["bin"] = renameIntoQuarantine(bin, qdir, &res.Degraded)
	res.Steps["meta"] = removeOrRecord(meta, "meta", false, &res.Degraded)

	// Step 5: completion manifest (best-effort, same as step 0).
	if res.ManifestWritten {
		writeQuarantineManifest(qdir, &quarantineManifest{State: "done", Reason: reason, TS: time.Now().Unix(), Steps: res.Steps, Degraded: res.Degraded})
	}

	if len(res.Degraded) > 0 {
		fmt.Fprintf(os.Stderr, "cache QUARANTINED by server rejection (%s) [DEGRADED: %v]: steps failed — the old snapshot may still be decryptable, delete it manually; re-enroll with a fresh device code\n", reason, res.Degraded)
		return res, fmt.Errorf("%w [DEGRADED: %v] — the old snapshot may still be decryptable; delete it manually", ErrCacheQuarantined, res.Degraded)
	}
	fmt.Fprintf(os.Stderr, "cache QUARANTINED by server rejection (%s): snapshot isolated to quarantine/, device code + DEK deleted — re-enroll with a fresh device code\n", reason)
	return res, nil
}

// removeOrRecord deletes path. An absent target is the rev4 idempotent
// exception ("ok(absent)"). Any other error is recorded into degraded when the
// step is critical, else only surfaced as the step's text (meta is non-critical).
func removeOrRecord(path, step string, critical bool, degraded *[]string) string {
	if path == "" {
		return "ok(no-path)"
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return "ok(absent)"
		}
		if critical {
			*degraded = append(*degraded, step)
		}
		return err.Error()
	}
	return "ok"
}

// renameIntoQuarantine moves bin into qdir under a timestamped name, first
// clearing any previously retained copy (single-copy retention, rev4 §2).
// bin itself already absent is the idempotent exception; a failure while bin
// is PRESENT (e.g. qdir unwritable) is a critical DEGRADED step. That verdict
// is based on bin's own stat — NOT on the rename error class, because on
// Windows a broken target path reports ERROR_PATH_NOT_FOUND, which errors.Is
// -matches ErrNotExist and would masquerade as idempotent success.
func renameIntoQuarantine(bin, qdir string, degraded *[]string) string {
	if _, serr := os.Stat(bin); serr != nil {
		if os.IsNotExist(serr) {
			return "ok(absent)"
		}
		*degraded = append(*degraded, "bin")
		return serr.Error()
	}
	if entries, derr := os.ReadDir(qdir); derr == nil {
		for _, e := range entries {
			if e.Name() != "manifest.json" {
				_ = os.Remove(filepath.Join(qdir, e.Name())) // drop the previous retained copy
			}
		}
	}
	if rErr := os.Rename(bin, filepath.Join(qdir, fmt.Sprintf("cache.bin.quarantined-%d", time.Now().Unix()))); rErr != nil {
		*degraded = append(*degraded, "bin")
		return rErr.Error()
	}
	return "ok"
}

// writeQuarantineManifest atomically writes qdir/manifest.json. Best-effort by
// contract: a failure logs to stderr and reports false; it never blocks or
// degrades any destruction step.
func writeQuarantineManifest(qdir string, m *quarantineManifest) bool {
	blob, err := json.Marshal(m)
	if err != nil {
		return false
	}
	if werr := atomicWriteUnique(filepath.Join(qdir, "manifest.json"), blob); werr != nil {
		fmt.Fprintf(os.Stderr, "cache QUARANTINE: manifest write failed (best-effort): %v\n", werr)
		return false
	}
	return true
}
