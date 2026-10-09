package sshbroker

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// ---- Plan 52: exec kill-grace env seam (spec D2) ----

// killGraceDefault / killGraceMin / killGraceMax bound SSHMGR_EXEC_KILL_GRACE:
// unset → 2s; anything outside [100ms, 30s] is a CONNECTION CONSTRUCTION
// failure (fail-closed), never a silent clamp. The floor keeps the grace a
// real window even on coarse OS timers; the ceiling bounds how long a caller
// can wait past its own timeout before the watchdog escalates to closing the
// connection.
const (
	killGraceDefault = 2 * time.Second
	killGraceMin     = 100 * time.Millisecond
	killGraceMax     = 30 * time.Second
)

// killGraceFromEnv parses SSHMGR_EXEC_KILL_GRACE — the window the exec
// watchdog waits between closing the session channel and closing the whole
// SSH connection (see killWatchdog). Unset / whitespace → killGraceDefault;
// unparsable / negative / outside [killGraceMin, killGraceMax] → an error
// naming the env var and the accepted range. Called at every connection
// construction (per-construction read, like the host-key-algorithms knob in
// connect.go — no process-level cache), so tests can override per connection
// and a long-lived broker never reuses a stale value.
func killGraceFromEnv() (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv("SSHMGR_EXEC_KILL_GRACE"))
	if v == "" {
		return killGraceDefault, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("SSHMGR_EXEC_KILL_GRACE: invalid duration %q (want [100ms, 30s])", v)
	}
	if d < killGraceMin || d > killGraceMax {
		return 0, fmt.Errorf("SSHMGR_EXEC_KILL_GRACE: %q outside the accepted range [100ms, 30s]", v)
	}
	return d, nil
}
