// Package clientops: Plan 40 batch 2 §1 — first-enroll auto-relocation.
package clientops

import (
	"os"
	"path/filepath"
)

// vacuumMarkerFiles are the default slot's history markers (spec §1.1 cond 4,
// vacuum v4): meta is rewritten on every successful pull (the natural trace of
// "this slot once held material"), config records deliberate cap policy. ANY of
// them present = default-slot intent → a bare pull must NOT relocate.
var vacuumMarkerFiles = []string{"cache.bin", "cache.auth.json", "cache.meta.json", "cache.config.json"}

// defaultSlotVacuum reports whether dir carries none of the marker files.
func defaultSlotVacuum(dir string) bool {
	for _, f := range vacuumMarkerFiles {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			return false
		}
	}
	return true
}

// singleSlotOverrideEnvSet reports whether a full-override env is present
// (spec §1.1 cond 5): either one keeps the pull in single-slot semantics, so
// auto-relocation is off. SSHMGR_CACHE_DEK_DIR is a coherent directory-level
// seam (the whole DEK tree moves) and does NOT count.
func singleSlotOverrideEnvSet() bool {
	return os.Getenv("SSHMGR_CACHE_DIR") != "" || os.Getenv("SSHMGR_CACHE_DEK") != ""
}

// DefaultSlotVacuum is the exported form for the TUI (auto-picker trigger uses
// the SAME four-file judgment as relocation, spec §3.2).
func DefaultSlotVacuum() (bool, error) {
	dir, _, _, _, err := CachePaths()
	if err != nil {
		return false, err
	}
	return defaultSlotVacuum(dir), nil
}

// SingleSlotOverrideEnvSet is the exported form for the TUI single-slot mode
// banner/disables (spec §3.5).
func SingleSlotOverrideEnvSet() bool { return singleSlotOverrideEnvSet() }
