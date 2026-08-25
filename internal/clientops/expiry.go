// Package clientops: Plan 37 B 时限快照 — the offline cache expiry machinery:
// the SSHMGR_CACHE_MAX_OFFLINE seam, the shared clock-tolerance constant, the
// quarantine reason constants, and the test seams for the destructive-race
// matrix. The pull-side gates live in DoPull; the load-side gates in
// LoadCacheSnapshot (both in clientops.go).
package clientops

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// cacheSkewTolerance is the design's frozen K (§5): ONE constant serving both
// the pull-side skew gate (|server Date − local now| ≤ K) and the load-side
// rollback tolerance (now ≥ PulledAt − K). Same constant on both sides is what
// makes "a clock that passed the pull gate can never trip the load gate" hold.
const cacheSkewTolerance = time.Hour

// The only two quarantine manifest reasons (§4). QuarantineReport dispatches
// its message on exact equality against expiryReason; every other reason —
// i.e. the Plan 34 server-rejection one — keeps the legacy texts verbatim.
const (
	expiryReason         = "snapshot expired (offline beyond SSHMGR_CACHE_MAX_OFFLINE)"
	serverRejectedReason = "server rejected device code"
)

// cacheMaxOffline parses SSHMGR_CACHE_MAX_OFFLINE (§1): unset/empty/zero = off
// (0, nil); a valid duration >= 1h = on; anything else — unparseable, negative,
// or a positive sub-hour value — is a fail-closed error carrying the raw value
// (both LoadCacheSnapshot and DoPull refuse on it; there is no half-open state).
func cacheMaxOffline() (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv("SSHMGR_CACHE_MAX_OFFLINE"))
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("invalid SSHMGR_CACHE_MAX_OFFLINE %q: must be a Go duration >= 1h (e.g. 168h; unset/0 disables expiry)", v)
	}
	if d == 0 {
		return 0, nil
	}
	if d < cacheSkewTolerance {
		return 0, fmt.Errorf("invalid SSHMGR_CACHE_MAX_OFFLINE %q: must be a Go duration >= 1h (e.g. 168h; unset/0 disables expiry)", v)
	}
	return d, nil
}

// readCacheMeta loads+decodes cache.meta.json. Any read or parse failure is an
// error — the load-side gates treat "no usable anchor" as refuse-not-destroy.
func readCacheMeta(path string) (cacheMeta, error) {
	var m cacheMeta
	blob, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(blob, &m); err != nil {
		return m, fmt.Errorf("corrupt cache.meta.json: %w", err)
	}
	return m, nil
}

// failNextMetaWriteForTest, when armed, makes the NEXT DoPull's meta write
// fail (the pull itself still succeeds — the §2.4 WARNING path). Test-only
// seam for the commit-order matrix (§8.18); nil-effect in production.
var failNextMetaWriteForTest bool

// FailNextMetaWriteForTest arms the one-shot meta write failure.
func FailNextMetaWriteForTest() { failNextMetaWriteForTest = true }

// expiryTestHooks lets tests orchestrate the destructive-race matrix (spec
// §8.16) at the three load-side points: after the age verdict (before the
// re-read) and after the re-check verdict (before destruction). Nil in
// production; ResetExpiryHooksForTest clears them between tests.
var expiryTestHooks struct {
	afterAgeCheck func()
	afterRecheck  func()
}

// ResetExpiryHooksForTest clears the load-side expiry hooks.
func ResetExpiryHooksForTest() {
	expiryTestHooks.afterAgeCheck = nil
	expiryTestHooks.afterRecheck = nil
}
