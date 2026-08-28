package mcpserver

import (
	"os"
	"strconv"
	"sync"
	"time"
)

// /pair/* rate limiting (Plan 42 spec §3.3-1, rev4 frozen): a per-IP fixed
// window counter per endpoint — zero dependencies, no background sweeper.
// The window anchor is the FIRST hit's arrival time; a hit past
// windowStart+60s opens a fresh window. LAN-facing posture: the map is
// opportunistically swept when it grows past rateLimiterSweepAt so spoofed
// source IPs cannot grow it without bound.

const (
	// rateLimiterSweepAt: at this many tracked IPs an Allow() pass purges
	// expired windows (amortized O(n) once per 4096 distinct IPs).
	rateLimiterSweepAt = 4096
	rateWindowSecs     = 60
)

// rateWindow is one IP's fixed-window counter.
type rateWindow struct {
	start int64 // unix seconds of the window's first hit
	count int
}

// rateLimiter admits at most perMin hits per IP per 60s window.
type rateLimiter struct {
	perMin  int
	mu      sync.Mutex
	windows map[string]rateWindow
}

// newRateLimiter builds a limiter admitting perMin requests per IP per minute.
// perMin <= 0 disables limiting (Allow always true) — defensive; the env
// clamps keep production values >= 1.
func newRateLimiter(perMin int) *rateLimiter {
	return &rateLimiter{perMin: perMin, windows: make(map[string]rateWindow)}
}

// Allow reports whether ip may proceed right now. Zero-allocation on the
// happy path beyond the map entry.
func (l *rateLimiter) Allow(ip string) bool {
	if l.perMin <= 0 {
		return true
	}
	now := time.Now().Unix()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.windows) >= rateLimiterSweepAt {
		for k, w := range l.windows {
			if now-w.start >= rateWindowSecs {
				delete(l.windows, k)
			}
		}
	}
	w, ok := l.windows[ip]
	if !ok || now-w.start >= rateWindowSecs {
		l.windows[ip] = rateWindow{start: now, count: 1}
		return true
	}
	if w.count >= l.perMin {
		return false
	}
	w.count++
	l.windows[ip] = w
	return true
}

// Frozen env seams + defaults + clamp ranges (spec §3.3-1 rev4, codex#7 —
// 一字不差). Env is read once at serve-runner construction (restart-effective,
// per spec); values outside the frozen range clamp; unparseable values fall
// back to the default (a rate-limit typo must not refuse serve startup — the
// store-side pending quotas backstop the security-relevant ceilings).
const (
	envPairEnrollPerMin  = "SSHMGR_SERVE_PAIR_ENROLL_PER_MIN"
	envPairPollPerMin    = "SSHMGR_SERVE_PAIR_POLL_PER_MIN"
	envPairFinishPerMin  = "SSHMGR_SERVE_PAIR_FINISH_PER_MIN"
	envPairPendingMaxIP  = "SSHMGR_SERVE_PAIR_PENDING_MAX_IP"
	envPairPendingMaxGlb = "SSHMGR_SERVE_PAIR_PENDING_MAX_GLOBAL"
)

// pairLimits carries the three /pair/* endpoint limiters.
type pairLimits struct {
	enroll, poll, finish *rateLimiter
}

// pairLimitsFromEnv reads the five frozen env seams, clamps each to its
// frozen range and returns the limiter set plus the two pending-queue quota
// values handed to store.AddPendingPairing.
func pairLimitsFromEnv() (lim pairLimits, pendingPerIP, pendingGlobal int) {
	return pairLimits{
		enroll: newRateLimiter(envLimit(envPairEnrollPerMin, 5, 1, 60)),
		poll:   newRateLimiter(envLimit(envPairPollPerMin, 30, 1, 120)),
		finish: newRateLimiter(envLimit(envPairFinishPerMin, 5, 1, 30)),
	}, envLimit(envPairPendingMaxIP, 2, 1, 10), envLimit(envPairPendingMaxGlb, 32, 1, 128)
}

// envLimit resolves one seam: absent/empty/unparseable → def; otherwise
// clamped to [min, max].
func envLimit(name string, def, min, max int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}
