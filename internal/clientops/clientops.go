// Package clientops holds the client-side cache operations shared by the CLI
// (`cache pull` / `cache status` / `mcp --cache`) and the upcoming TUI console:
// path resolution, the pull credential (cache.auth.json), the snapshot pull
// itself, lazy/backoff auto-refresh, and hot-reload change detection.
//
// Extracted verbatim from internal/cli (zero-behavior move, Stream B Task 1)
// so internal/tui can consume it without importing internal/cli (import
// cycle). internal/cli keeps only the thin cobra wrappers.
package clientops

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vaultio"
)

// LazyPullTimeout bounds every automatic (lazy) pull: it runs on the spawn /
// tool-call critical path, so an unreachable broker must degrade within seconds,
// not hang MCP startup (xcheck pi#2/codex#3). Manual `cache pull` stays
// unbounded — the interactive user can Ctrl-C.
const LazyPullTimeout = 10 * time.Second

// lazyPullBackoff rate-limits automatic pulls to at most one attempt per TTL
// window (success or failure), so an offline machine doesn't retry — and block a
// tool call for up to LazyPullTimeout — on every call. The cache.bin mtime only
// advances on SUCCESS, so without this backoff a failed pull would re-fire per call.
var lazyPullBackoff struct {
	mu          sync.Mutex
	lastAttempt time.Time
}

// ResetLazyPullBackoffForTest zeroes the lazy-pull backoff window so tests can
// exercise the pull attempt path without waiting out a previous attempt.
func ResetLazyPullBackoffForTest() { lazyPullBackoff.lastAttempt = time.Time{} }

// cacheQuarantinedFlag is the Plan 34 rev4 §3 process-level "already
// quarantined" mark: once a pinned-401 rejection destroyed the cache in THIS
// process, MaybeLazyPull never auto-attempts again (a lingering cache.auth.json
// from a failed deletion retries only on the NEXT spawn — at most one
// destruction attempt per spawn).
var cacheQuarantinedFlag bool

// ResetCacheQuarantineForTest clears the process-level quarantine flag so tests
// can exercise the auto-pull path independently of an earlier quarantine (the
// flag intentionally lasts the process lifetime in production).
func ResetCacheQuarantineForTest() { cacheQuarantinedFlag = false }

// MaybeLazyPull runs ONE automatic pull when cache.bin is missing / older than
// maxAge and a persisted cache.auth.json exists. maxAge<=0 disables entirely —
// INCLUDING the missing-cache case (the first pull stays a deliberate manual
// step). Errors are returned for the caller to log; never fatal.
func MaybeLazyPull(maxAge time.Duration) error {
	if maxAge <= 0 {
		return nil
	}
	if cacheQuarantinedFlag {
		return nil // Plan 34: quarantined this process — no further auto-pulls
	}
	cred, err := ReadCacheCred()
	if err != nil {
		return err
	}
	if cred == nil {
		return nil
	}
	_, bin, _, _, err := CachePaths()
	if err != nil {
		return err
	}
	if info, statErr := os.Stat(bin); statErr == nil && time.Since(info.ModTime()) < maxAge {
		return nil // fresh enough
	}
	lazyPullBackoff.mu.Lock()
	if time.Since(lazyPullBackoff.lastAttempt) < maxAge {
		lazyPullBackoff.mu.Unlock()
		return nil
	}
	lazyPullBackoff.lastAttempt = time.Now()
	lazyPullBackoff.mu.Unlock()

	pin := cred.Pin // resolved pin wins over any embedded stale pin (cert rotation)
	code := cred.Token
	if pin == "" {
		if c, p, ok := SplitTokenPin(cred.Token); ok {
			code, pin = c, p
		}
	}
	if pin == "" {
		return fmt.Errorf("cache.auth.json has no pin; refusing plaintext auto-pull")
	}
	err = DoPull(cred.URL, code, pin, PullOpts{Timeout: LazyPullTimeout, StatusOut: os.Stderr})
	if errors.Is(err, ErrCacheQuarantined) {
		// Plan 34 rev4 §3 — the pinned server rejected the device code and the
		// cache is destroyed. Terminal for this process: the flag stops every
		// later boundary (even with a lingering cache.auth.json from a failed
		// deletion — that retries only on the NEXT spawn, ≤1 destruction
		// attempt per spawn), and the backoff window is reverted so the
		// attempt is NOT counted as a transient failure (rev4: 不进 backoff —
		// the flag, not the backoff, is the suppressor).
		cacheQuarantinedFlag = true
		lazyPullBackoff.mu.Lock()
		lazyPullBackoff.lastAttempt = time.Time{}
		lazyPullBackoff.mu.Unlock()
		fmt.Fprintf(os.Stderr, "cache QUARANTINED by server rejection: automatic pulls disabled for this session — re-enroll with a fresh device code\n")
		// Propagate the sentinel (rev4 §5 "lazy 哨兵传播"): callers only log
		// it, never act fatally — and mcp.go's "serving stale cache" wording
		// is guarded by cachePresent(), which is false post-quarantine.
	}
	return err
}

// CachePaths resolves the cache directory (SSHMGR_CACHE_DIR override, else UserConfigDir/
// ssh-manager) and the three files within it: the encrypted snapshot, the meta sidecar, and
// the offline-audit sidecar (the audit sidecar is owned by T8; T7 only resolves the path).
func CachePaths() (dir, bin, meta, audit string, err error) {
	if dir = os.Getenv("SSHMGR_CACHE_DIR"); dir == "" {
		base, derr := os.UserConfigDir()
		if derr != nil {
			return "", "", "", "", derr
		}
		dir = filepath.Join(base, "ssh-manager")
	}
	return dir, filepath.Join(dir, "cache.bin"), filepath.Join(dir, "cache.meta.json"), filepath.Join(dir, "cache-audit.log"), nil
}

// atomicWriteUnique atomically replaces path with blob via a UNIQUE temp file +
// rename. Unlike a fixed ".tmp" name, concurrent writers (multiple `mcp --cache`
// spawns lazy-pulling at once, or a lazy pull racing a manual one) never
// interleave on the same temp file, so a torn blob can never be renamed into
// place (xcheck 2026-08-14, three-reviewer consensus). os.CreateTemp creates the
// temp 0600, which matches every current use (cache.bin/meta/auth are all 0600).
//
// On Windows, concurrent readers can hold the target file open, causing
// os.Rename to fail with "Access is denied". We retry briefly to handle these
// transient conflicts — the unique temp name ensures writers never conflict
// with each other, only with concurrent readers (which is expected and safe).
func atomicWriteUnique(path string, blob []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op after a successful rename
	if _, err := tmp.Write(blob); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Retry rename on Windows to handle ERROR_ACCESS_DENIED / fs.ErrPermission from concurrent readers
	var lastErr error
	for i := 0; i < 50; i++ {
		err := os.Rename(tmpPath, path)
		if err == nil {
			return nil // success
		}
		lastErr = err
		// On Windows, ERROR_ACCESS_DENIED / fs.ErrPermission from concurrent readers is transient
		// Other errors are not retryable
		if !errors.Is(err, fs.ErrPermission) {
			return err
		}
		time.Sleep(time.Duration(10+(i*2)) * time.Millisecond) // 10-110ms backoff
	}
	return lastErr
}

type cacheMeta struct {
	URL      string `json:"url"`
	PulledAt int64  `json:"pulled_at"` // pull time: server-clock anchored (ServerAnchored=true) or local clock (false)
	// ServerAnchored records whether PulledAt came from a pinned server Date
	// that passed the skew gate (Plan 37 §2.4). No omitempty: B-off pulls
	// serialize an explicit false; a legacy meta without the field reads as
	// the zero value false — which is exactly the provenance semantics.
	ServerAnchored bool `json:"server_anchored"`
}

// CacheCred persists the pull credential (cache.auth.json) so `mcp --cache` can
// lazy-pull without env/flags. Pin is the RESOLVED effective pin from the last
// successful pull (env > flag > token-embedded) stored bare; the lazy path
// prefers it over any pin still embedded in Token (cert rotation: a manual
// --pin re-pull must override a stale embedded pin). The device code grants
// FUTURE snapshot pulls — cache.bin alone only holds the past — so this file is
// bearer-token-grade: 0600 + HardenACL on Windows, revoke on theft (threat-model §1.1).
type CacheCred struct {
	URL   string `json:"url"`
	Token string `json:"token"`         // bare device code
	Pin   string `json:"pin,omitempty"` // resolved effective pin at last successful pull
}

// CacheCredPath returns the path of cache.auth.json inside the cache dir.
func CacheCredPath() (string, error) {
	dir, _, _, _, err := CachePaths()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "cache.auth.json"), nil
}

// ReadCacheCred returns nil, nil when the file is absent (never enrolled / not
// yet pulled). A present-but-corrupt file is an error: silently ignoring it
// would disable auto-refresh invisibly.
func ReadCacheCred() (*CacheCred, error) {
	p, err := CacheCredPath()
	if err != nil {
		return nil, err
	}
	blob, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c CacheCred
	if err := json.Unmarshal(blob, &c); err != nil {
		return nil, fmt.Errorf("cache.auth.json corrupt: %w", err)
	}
	if c.URL == "" || c.Token == "" {
		return nil, fmt.Errorf("cache.auth.json incomplete (missing url/token)")
	}
	return &c, nil
}

// WriteCacheCred persists the credential atomically (unique temp + rename) and
// hardens the ACL on Windows (no-op on Unix where 0600 is the protection).
func WriteCacheCred(cred *CacheCred) error {
	p, err := CacheCredPath()
	if err != nil {
		return err
	}
	blob, err := json.Marshal(cred)
	if err != nil {
		return err
	}
	if err := atomicWriteUnique(p, blob); err != nil {
		return err
	}
	return store.HardenACL(p)
}

// LoadCacheSnapshot reads + DEK-decrypts + unmarshals the cache. Shared by `cache status` and
// `mcp --cache`. Returns an error if the cache is absent / corrupt / the DEK is missing.
//
// Plan 37 §3: with SSHMGR_CACHE_MAX_OFFLINE set, three gates run BEFORE the
// DEK is even touched (meta is plaintext): usable-anchor, provenance, and
// expiry/rollback. Destruction happens only on the expiry gate, only after a
// re-check confirms the anchor did not just advance (destructive-race guard).
func LoadCacheSnapshot() (*store.Snapshot, error) {
	_, bin, metaPath, _, err := CachePaths()
	if err != nil {
		return nil, err
	}
	maxOffline, err := cacheMaxOffline()
	if err != nil {
		return nil, err
	}
	if maxOffline > 0 {
		meta, merr := readCacheMeta(metaPath)
		if merr != nil {
			// §3.2: no usable anchor — refuse, never destroy (includes the
			// never-pulled first run; a fresh machine has no meta by design).
			return nil, fmt.Errorf("SSHMGR_CACHE_MAX_OFFLINE is set but cache.meta.json is missing or corrupt (or this machine never pulled) — refusing cache (no time anchor); run cache pull")
		}
		if !meta.ServerAnchored {
			// §3.3 provenance gate: a local-clock anchor never passed any gate.
			return nil, fmt.Errorf("cache.meta.json has no server-anchored time (pulled while SSHMGR_CACHE_MAX_OFFLINE was unset or by an older client) — refusing cache; run cache pull to establish a server time anchor")
		}
		anchorT := time.Unix(meta.PulledAt, 0)
		now := time.Now()
		if now.Sub(anchorT) > maxOffline {
			if expiryTestHooks.afterAgeCheck != nil {
				expiryTestHooks.afterAgeCheck()
			}
			// §3.4 re-check: destruction requires positive expiry evidence AND
			// confirmation. Re-read the anchor right before destroying.
			meta2, rerr := readCacheMeta(metaPath)
			if rerr != nil {
				return nil, fmt.Errorf("cache expiry re-check failed (%v) — refusing cache; run cache pull", rerr)
			}
			if expiryTestHooks.afterRecheck != nil {
				expiryTestHooks.afterRecheck()
			}
			if meta2.PulledAt > meta.PulledAt && meta2.ServerAnchored {
				// A concurrent trusted pull just re-anchored: abort destruction
				// and judge again from the fresh anchor.
				return LoadCacheSnapshot()
			}
			_, qerr := QuarantineCache(expiryReason)
			if qerr == nil {
				qerr = ErrCacheQuarantined
			}
			return nil, fmt.Errorf("%w — cache snapshot expired (offline %s > cap %s) — snapshot destroyed; run cache pull to re-enroll",
				qerr, now.Sub(anchorT).Round(time.Second), maxOffline)
		}
		if now.Before(anchorT.Add(-cacheSkewTolerance)) {
			// §3.5 rollback gate: refuse (never destroy) — a clock fault and a
			// clock attack look identical; the next trusted pull re-anchors.
			return nil, fmt.Errorf("system clock is behind the snapshot's server time anchor (local %s, anchor %s, tolerance 1h) — refusing cache (clock fault or tampering); fix system time, then run cache pull",
				now.Format(time.RFC3339), anchorT.Format(time.RFC3339))
		}
	}
	dek, err := loadDEK()
	if err != nil {
		return nil, fmt.Errorf("cache DEK not found in keychain (run `cache pull` first): %w", err)
	}
	blob, err := os.ReadFile(bin)
	if err != nil {
		return nil, err
	}
	plaintext, err := vaultio.DecryptWithKey(dek, blob)
	if err != nil {
		return nil, fmt.Errorf("cache decrypt failed (the DEK and cache.bin may be from different installs): %w", err)
	}
	var snap store.Snapshot
	if err := json.Unmarshal(plaintext, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// PullOpts tunes DoPull for its caller.
type PullOpts struct {
	AllowPlain bool          // plaintext opt-in — manual CLI only; the lazy path NEVER sets this
	Timeout    time.Duration // >0 → overall http.Client timeout (lazy: spawn/tool-call path must be bounded)
	StatusOut  io.Writer     // status/warning sink (nil → silent); CLI passes cmd.ErrOrStderr()
}

// DoPull fetches /snapshot from url with the device code and atomically writes
// cache.bin + cache.meta.json. pin=="" means no pin: AllowPlain must be true or
// the pull refuses (F4 hard-fail contract). pin!="" pins TLS to that SPKI
// fingerprint and requires an https:// URL (F8). Extracted from cachePullCmd so
// the spawn-lazy pull (mcp --cache) shares ONE implementation.
func DoPull(url, token, pin string, o PullOpts) error {
	code := token
	// Plan 37 §1/§2: fail-closed env precheck — an invalid cap refuses the
	// pull before any HTTP (no half-open "pulls but won't load" state).
	maxOffline, err := cacheMaxOffline()
	if err != nil {
		return err
	}
	var client *http.Client
	if pin == "" {
		if maxOffline > 0 {
			// Plan 37 §2.1: the time anchor requires a pinned TLS server;
			// a plaintext response's Date is injectable and cannot anchor.
			return fmt.Errorf("SSHMGR_CACHE_MAX_OFFLINE is set: refusing plaintext pull — the time anchor requires a pinned TLS server (unset the cap or remove --allow-plaintext)")
		}
		if !o.AllowPlain {
			return fmt.Errorf("no server pin provided: refusing to pull without TLS pin. " +
				"Set --pin/SSHMGR_SERVE_PIN (from `serve cert-info`), or pass --allow-plaintext for an insecure plaintext pull")
		}
		if o.StatusOut != nil {
			fmt.Fprintf(o.StatusOut, "WARNING: --allow-plaintext set: pulling over unverified HTTP (no TLS pin). /snapshot credentials travel in cleartext.\n")
		}
		// Plan 37 §2.0: redirects are never followed, on the plain client too —
		// a followed 30x would take the response (body AND headers) off the
		// transport the caller chose (xcheck experiment, 2026-08-25).
		client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	} else {
		// The device code goes to the Authorization header; the pin is for TLS only.
		// If the token is "<code>:<pin>", strip so the header carries just the code.
		if c, _, ok := SplitTokenPin(token); ok {
			code = c
		}
		if u, err := neturl.Parse(url); err != nil || u.Scheme != "https" {
			return fmt.Errorf("--url must be https:// when a server pin is set (got %q); "+
				"to pull plaintext instead, clear the pin (--pin/SSHMGR_SERVE_PIN) and pass --allow-plaintext", url)
		}
		tr, err := pinningTransport(pin)
		if err != nil {
			return err
		}
		client = &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	if o.Timeout > 0 {
		client.Timeout = o.Timeout
	}

	dek, err := loadOrCreateDEK()
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodGet, url+"/snapshot", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+code)
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("pull: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		io.Copy(io.Discard, res.Body) // keep the keep-alive socket reusable
		if res.StatusCode >= 300 && res.StatusCode < 400 {
			// Plan 37 §2.0: CheckRedirect returned ErrUseLastResponse, so a 3xx
			// surfaces here — never followed, on any client.
			return fmt.Errorf("pull: server returned %d (redirects are not followed)", res.StatusCode)
		}
		if pin != "" && res.StatusCode == 401 {
			// Plan 34 rev4 §3 — the ONLY quarantine trigger: a PINNED server
			// rejected the device code (revoked server-side). Plaintext 401s,
			// network/TLS failures, and non-401 statuses never reach here.
			// QuarantineCache returns nil on a clean/idempotent destruction —
			// the trigger itself raises the sentinel then; its DEGRADED error
			// already wraps ErrCacheQuarantined.
			_, qerr := QuarantineCache(serverRejectedReason)
			if qerr == nil {
				qerr = ErrCacheQuarantined
			}
			return fmt.Errorf("pull: %w — re-enroll with a fresh device code", qerr)
		}
		return fmt.Errorf("pull: server returned %d (is the authorization code valid/active?)", res.StatusCode)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	// Plan 37 §2.2-2.4: B on → the Date header of THIS pinned 200 response is
	// the time anchor. Missing/malformed Date or |server − local| > 1h refuses
	// the pull (fail-closed); a passing Date is written into meta.PulledAt with
	// ServerAnchored=true. B off keeps the legacy local clock + explicit false.
	pulledAt := time.Now().Unix()
	anchored := false
	if maxOffline > 0 {
		serverTime, perr := http.ParseTime(res.Header.Get("Date"))
		if perr != nil {
			return fmt.Errorf("pull succeeded but the response has no valid Date header — refusing to anchor cache time (SSHMGR_CACHE_MAX_OFFLINE requires a trusted server clock)")
		}
		if skew := time.Since(serverTime); skew > cacheSkewTolerance || skew < -cacheSkewTolerance {
			return fmt.Errorf("server clock skew too large (server %s vs local %s, cap 1h) — refusing pull: SSHMGR_CACHE_MAX_OFFLINE depends on an accurate clock; fix system time sync",
				serverTime.Format(time.RFC3339), time.Now().Format(time.RFC3339))
		}
		pulledAt = serverTime.Unix()
		anchored = true
	}
	blob, err := vaultio.EncryptWithKey(dek, body)
	if err != nil {
		return err
	}
	_, bin, metaPath, _, err := CachePaths()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
		return err
	}
	// Plan 37 §2.4 commit order, pinned: bin first, meta LAST — a crash can
	// leave (new bin + old anchor) whose age error is bounded <= 2K, never
	// (new anchor + old bin). Meta-write failure stays the legacy WARNING.
	if err := atomicWriteUnique(bin, blob); err != nil {
		return err
	}
	if failNextMetaWriteForTest {
		failNextMetaWriteForTest = false
		if o.StatusOut != nil {
			fmt.Fprintf(o.StatusOut, "WARNING: cache.meta.json write failed (source URL will show as unknown): test-injected failure\n")
		}
	} else {
		mb, _ := json.Marshal(cacheMeta{URL: url, PulledAt: pulledAt, ServerAnchored: anchored})
		if werr := atomicWriteUnique(metaPath, mb); werr != nil && o.StatusOut != nil {
			fmt.Fprintf(o.StatusOut, "WARNING: cache.meta.json write failed (source URL will show as unknown): %v\n", werr)
		}
	}
	// Plan 34 rev4 §4: a successful pull supersedes any quarantine attribution —
	// drop the stale manifest. Best-effort: if this removal fails, the §4 time
	// guard (manifest.ts > meta.pulled_at, and this pull just wrote a fresh
	// meta) still auto-invalidates it (crash-safe backstop).
	_ = os.Remove(filepath.Join(filepath.Dir(bin), "quarantine", "manifest.json"))
	var snap store.Snapshot
	_ = json.Unmarshal(body, &snap) // for the status line only
	if o.StatusOut != nil {
		fmt.Fprintf(o.StatusOut, "pulled %d servers / %d credentials into %s\n", len(snap.Servers), len(snap.Credentials), bin)
	}
	return nil
}

// CacheReloader detects cache.bin changes for hot-reload and kicks in-session
// lazy pulls. Change detection hashes the whole (encrypted) file — a vault
// snapshot is KBs, so this is ~µs per tool call — and is immune to the
// same-tick / same-size mtime blind spot (xcheck codex#4/kimi#4). The baseline
// is captured at construction, which the mcp command does BEFORE the initial
// LoadCacheSnapshot: a baseline taken after the load could swallow an external
// pull that landed mid-startup (the harmless residue — a pull racing the
// baseline — costs one redundant rebuild, never a missed one).
type CacheReloader struct {
	bin    string
	maxAge time.Duration
	sum    []byte // SHA-256 of the served cache.bin (nil until first successful load)
}

// NewCacheReloader captures the current cache.bin hash as the reload baseline.
// When paths are unavailable the reloader is returned empty and Check surfaces
// the error.
func NewCacheReloader(maxAge time.Duration) *CacheReloader {
	_, bin, _, _, err := CachePaths()
	if err != nil {
		return &CacheReloader{maxAge: maxAge} // Check() surfaces the error
	}
	return &CacheReloader{bin: bin, maxAge: maxAge, sum: fileSumOf(bin)}
}

func fileSumOf(path string) []byte {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	s := sha256.Sum256(blob)
	return s[:]
}

// Check implements the reload callback for mcpserver.RunStdioCache. The changed
// path mints a FRESH *store.Snapshot each time (LoadCacheSnapshot naturally
// allocates one — never cache or reuse a pointer across calls; the holder
// dedupes by pointer identity), and r.sum advances ONLY after this function's
// own successful decrypt+unmarshal. If the holder then fails to hydrate the new
// snapshot (e.g. the token was revoked in it), the change is intentionally
// dropped — that is the spec's Lazy revocation semantics.
//
// Concurrency contract: Check must be invoked SERIALIZED — mcpserver's
// cacheStoreHolder calls it under its mutex. It is NOT safe for concurrent use:
// r.sum carries no lock of its own, so racing calls could advance the baseline
// out of order.
func (r *CacheReloader) Check() (*store.Snapshot, bool, error) {
	if r.bin == "" {
		return nil, false, fmt.Errorf("cache paths unavailable")
	}
	blob, err := os.ReadFile(r.bin)
	if err != nil {
		return nil, false, err // gone/unreadable → keep serving the current store
	}
	s := sha256.Sum256(blob)
	if bytes.Equal(s[:], r.sum) {
		// Unchanged. In-session freshness: MaybeLazyPull no-ops while fresh and
		// backs off on failure; a successful pull changes the file, so the NEXT
		// call swaps the store in — this call deliberately finishes on the old
		// one (never half-old half-new within a single tool call).
		if err := MaybeLazyPull(r.maxAge); err != nil {
			fmt.Fprintf(os.Stderr, "ssh-manager: in-session cache refresh failed: %v\n", err)
		}
		return nil, false, nil
	}
	snap, err := LoadCacheSnapshot()
	if err != nil {
		return nil, false, err // corrupt/undecryptable → keep the old store, baseline NOT advanced
	}
	r.sum = s[:] // advance only on a successful load
	return snap, true, nil
}
