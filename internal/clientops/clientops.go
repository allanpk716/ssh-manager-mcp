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
	"sort"
	"strings"
	"sync"
	"time"

	"ssh-manager-mcp/internal/instname"
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

// MaybeLazyPullFor runs ONE automatic pull for the given instance ("" = the
// default instance) when cache.bin is missing / older than maxAge and a
// persisted cache.auth.json exists. maxAge<=0 disables entirely — INCLUDING the
// missing-cache case (the first pull stays a deliberate manual step). Errors
// are returned for the caller to log; never fatal.
func MaybeLazyPullFor(instance string, maxAge time.Duration) error {
	if maxAge <= 0 {
		return nil
	}
	if cacheQuarantinedFlag {
		return nil // Plan 34: quarantined this process — no further auto-pulls
	}
	cred, err := ReadCacheCredFor(instance)
	if err != nil {
		return err
	}
	if cred == nil {
		return nil
	}
	_, bin, _, _, err := CachePathsFor(instance)
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
	_, err = DoPull(cred.URL, code, pin, PullOpts{Timeout: LazyPullTimeout, StatusOut: os.Stderr, Instance: instance})
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

// MaybeLazyPull runs the DEFAULT instance's lazy pull (zero-change wrapper).
func MaybeLazyPull(maxAge time.Duration) error { return MaybeLazyPullFor("", maxAge) }

// CachePathsFor resolves the cache directory for ONE instance ("" = the
// default instance — legacy single-instance machines keep byte-identical
// behavior). Priority: SSHMGR_CACHE_DIR (explicit full override — the
// CLI layer rejects combining it with --instance) > instances/<name> > the
// default dir. A named instance must pass the whitelist before Join.
func CachePathsFor(instance string) (dir, bin, meta, audit string, err error) {
	if instance != "" {
		if verr := instname.Valid(instance); verr != nil {
			return "", "", "", "", verr
		}
	}
	if dir = os.Getenv("SSHMGR_CACHE_DIR"); dir == "" {
		base, derr := os.UserConfigDir()
		if derr != nil {
			return "", "", "", "", derr
		}
		dir = filepath.Join(base, "ssh-manager")
		if instance != "" {
			dir = filepath.Join(dir, "instances", instance)
		}
	}
	return dir, filepath.Join(dir, "cache.bin"), filepath.Join(dir, "cache.meta.json"), filepath.Join(dir, "cache-audit.log"), nil
}

// CachePaths resolves the DEFAULT instance's paths (zero-change wrapper; every
// pre-Plan-40 caller — TUI client page, doctor, clear — keeps this view).
func CachePaths() (dir, bin, meta, audit string, err error) {
	return CachePathsFor("")
}

// InstancesRoot is where named instances live: "instances/" under the
// UserConfigDir base — deliberately NOT env-redirected: SSHMGR_CACHE_DIR is a
// single-slot full override (CachePathsFor ignores the instance when it is
// set), so following it here would create two competing instances/ roots.
func InstancesRoot() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "ssh-manager", "instances"), nil
}

// ListInstances returns the sorted directory names under InstancesRoot()
// (nil, nil when the root does not exist — an empty machine). A directory is
// an instance SLOT; presence of material inside is the caller's concern.
func ListInstances() ([]string, error) {
	root, err := InstancesRoot()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
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
	// Scoped records whether this cache was pulled from a Plan-39 serve
	// (X-Sshmgr-Snapshot-Scope: profile) — i.e. cropped to the device's bound
	// profile. A pre-Plan-39 whole-vault cache has the IDENTICAL snapshot
	// shape when the vault holds one profile, so the header at pull time is
	// the only honest discriminator. No omitempty, same zero-value semantics
	// as ServerAnchored (code-review #3).
	Scoped bool `json:"scoped"`
	// DeviceName records the pulling device code's name as asserted by the
	// pinned serve (X-Sshmgr-Device-Name). Empty on legacy metas (the zero
	// value — the §2.4 adopt-and-record branch) and on plaintext pulls (an
	// injectable header must never gate). No omitempty, same zero-value
	// semantics as ServerAnchored/Scoped.
	DeviceName string `json:"device_name"`
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

// CacheCredPathFor returns the path of cache.auth.json inside the given
// instance's cache dir ("" = the default instance).
func CacheCredPathFor(instance string) (string, error) {
	dir, _, _, _, err := CachePathsFor(instance)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "cache.auth.json"), nil
}

// CacheCredPath returns the DEFAULT instance's cache.auth.json path
// (zero-change wrapper).
func CacheCredPath() (string, error) { return CacheCredPathFor("") }

// ReadCacheCredFor returns the given instance's pull credential; nil, nil when
// the file is absent (never enrolled / not yet pulled). A present-but-corrupt
// file is an error: silently ignoring it would disable auto-refresh invisibly.
func ReadCacheCredFor(instance string) (*CacheCred, error) {
	p, err := CacheCredPathFor(instance)
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

// ReadCacheCred reads the DEFAULT instance's credential (zero-change wrapper).
func ReadCacheCred() (*CacheCred, error) { return ReadCacheCredFor("") }

// WriteCacheCredFor persists the given instance's credential atomically
// (unique temp + rename) and hardens the ACL on Windows (no-op on Unix where
// 0600 is the protection).
func WriteCacheCredFor(instance string, cred *CacheCred) error {
	p, err := CacheCredPathFor(instance)
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

// WriteCacheCred persists the DEFAULT instance's credential (zero-change
// wrapper).
func WriteCacheCred(cred *CacheCred) error { return WriteCacheCredFor("", cred) }

// LoadCacheSnapshotFor reads + DEK-decrypts + unmarshals the given instance's
// cache ("" = the default instance). Shared by `cache status` and `mcp --cache`.
// Returns an error if the cache is absent / corrupt / the DEK is missing.
//
// Plan 37 §3: with SSHMGR_CACHE_MAX_OFFLINE set, three gates run BEFORE the
// DEK is even touched (meta is plaintext): usable-anchor, provenance, and
// expiry/rollback. Destruction happens only on the expiry gate, only after a
// re-check confirms the anchor did not just advance (destructive-race guard).
func LoadCacheSnapshotFor(instance string) (*store.Snapshot, error) {
	dir, bin, metaPath, _, err := CachePathsFor(instance)
	if err != nil {
		return nil, err
	}
	maxOffline, err := resolveMaxOffline(dir)
	if err != nil {
		return nil, err
	}
	if maxOffline > 0 {
		meta, merr := readCacheMeta(metaPath)
		if merr != nil {
			// §3.2: no usable anchor — refuse, never destroy (includes the
			// never-pulled first run; a fresh machine has no meta by design).
			return nil, fmt.Errorf("the offline cap is set (SSHMGR_CACHE_MAX_OFFLINE or cache.config.json) but cache.meta.json is missing or corrupt (or this machine never pulled) — refusing cache (no time anchor); run cache pull")
		}
		if !meta.ServerAnchored {
			// §3.3 provenance gate: a local-clock anchor never passed any gate.
			return nil, fmt.Errorf("the offline cap is set (SSHMGR_CACHE_MAX_OFFLINE or cache.config.json) but cache.meta.json has no server-anchored time (pulled by an older client or without a pinned TLS server) — refusing cache; run cache pull to establish a server time anchor")
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
				return LoadCacheSnapshotFor(instance)
			}
			_, qerr := QuarantineCacheFor(instance, expiryReason)
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
	dek, err := loadDEK(instance)
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

// LoadCacheSnapshot loads the DEFAULT instance's cache (zero-change wrapper).
func LoadCacheSnapshot() (*store.Snapshot, error) { return LoadCacheSnapshotFor("") }

// PullOpts tunes DoPull for its caller.
type PullOpts struct {
	AllowPlain bool          // plaintext opt-in — manual CLI only; the lazy path NEVER sets this
	Timeout    time.Duration // >0 → overall http.Client timeout (lazy: spawn/tool-call path must be bounded)
	StatusOut  io.Writer     // status/warning sink (nil → silent); CLI passes cmd.ErrOrStderr()
	// Instance routes this pull to instances/<name>/ ("" = the default slot).
	// Validated by CachePathsFor; combined with SSHMGR_CACHE_DIR/SSHMGR_CACHE_DEK
	// it is rejected at the CLI layer (mutex, spec §2.2).
	Instance string
}

// PullResult reports where a pull's materials actually landed (Plan 40 batch 2
// §2): "" = the default slot; a name = instances/<name>/ (explicit --instance,
// or the §1.2 first-enroll auto-relocation). Callers that persist pull-side
// state (auth, config) MUST follow this slot — compiler-enforced completeness
// was the reason a return value beat an optional callback (spec §2).
type PullResult struct {
	Instance string
}

// DoPull fetches /snapshot from url with the device code and atomically writes
// cache.bin + cache.meta.json. pin=="" means no pin: AllowPlain must be true or
// the pull refuses (F4 hard-fail contract). pin!="" pins TLS to that SPKI
// fingerprint and requires an https:// URL (F8). Extracted from cachePullCmd so
// the spawn-lazy pull (mcp --cache) shares ONE implementation.
func DoPull(url, token, pin string, o PullOpts) (PullResult, error) {
	code := token
	// Plan 40 T5/T13: resolve the cache paths for THIS instance before anything
	// else disk-touching — the cap resolution below must read the instance's
	// own cache.config.json, and the later pre-write gates read the existing
	// meta before any bytes land on disk. Pure path computation: no HTTP, no
	// writes, so the fail-closed precheck below still precedes every request.
	dir, bin, metaPath, _, err := CachePathsFor(o.Instance)
	if err != nil {
		return PullResult{}, err
	}
	// rev5 §1.2-5: pull-side file validation is INDEPENDENT of env — applies to
	// the initially-resolved slot here, and to the relocation target after
	// retarget (T2).
	if verr := validateCapFileIndependent(dir); verr != nil {
		return PullResult{}, verr
	}
	// Plan 37 §1/§2 + Plan 40 T13: fail-closed cap precheck (env > file) — an
	// invalid cap from EITHER source refuses the pull before any HTTP (no
	// half-open "pulls but won't load" state; the env's error is never masked
	// by the file).
	maxOffline, err := resolveMaxOffline(dir)
	if err != nil {
		return PullResult{}, err
	}
	var client *http.Client
	if pin == "" {
		if maxOffline > 0 {
			// Plan 37 §2.1: the time anchor requires a pinned TLS server;
			// a plaintext response's Date is injectable and cannot anchor.
			return PullResult{}, fmt.Errorf("the offline cap is set (SSHMGR_CACHE_MAX_OFFLINE or cache.config.json): refusing plaintext pull — the time anchor requires a pinned TLS server (unset the cap or remove --allow-plaintext)")
		}
		if !o.AllowPlain {
			return PullResult{}, fmt.Errorf("no server pin provided: refusing to pull without TLS pin. " +
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
			return PullResult{}, fmt.Errorf("--url must be https:// when a server pin is set (got %q); "+
				"to pull plaintext instead, clear the pin (--pin/SSHMGR_SERVE_PIN) and pass --allow-plaintext", url)
		}
		tr, err := pinningTransport(pin)
		if err != nil {
			return PullResult{}, err
		}
		client = &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	if o.Timeout > 0 {
		client.Timeout = o.Timeout
	}

	// Plan 40 批2 §1.2-6: vacuum-candidate paths DEFER DEK creation to after
	// every gate — a refused relocation must leave zero writes, including no
	// freshly-created default DEK. Non-vacuum paths keep the batch-1 timing.
	vacuumCandidate := o.Instance == "" && !singleSlotOverrideEnvSet() && defaultSlotVacuum(dir)
	var dek []byte
	if !vacuumCandidate {
		dek, err = loadOrCreateDEK(o.Instance)
		if err != nil {
			return PullResult{}, err
		}
	}
	req, err := http.NewRequest(http.MethodGet, url+"/snapshot", nil)
	if err != nil {
		return PullResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+code)
	res, err := client.Do(req)
	if err != nil {
		return PullResult{}, fmt.Errorf("pull: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		// Read the (bounded) error body FIRST: the 403 branch discriminates the
		// serve's unbound-device-code refusal BY ITS BODY TEXT — a proxy/WAF 403
		// must not be misreported as "not bound" (code-review #6). Error bodies
		// are tiny; the 8 KiB cap only guards a pathological responder, and the
		// abandoned tail at worst drops one keep-alive socket.
		errBody, _ := io.ReadAll(io.LimitReader(res.Body, 8<<10))
		if res.StatusCode >= 300 && res.StatusCode < 400 {
			// Plan 37 §2.0: CheckRedirect returned ErrUseLastResponse, so a 3xx
			// surfaces here — never followed, on any client.
			return PullResult{}, fmt.Errorf("pull: server returned %d (redirects are not followed)", res.StatusCode)
		}
		if pin != "" && res.StatusCode == 401 {
			// Plan 34 rev4 §3 — the ONLY quarantine trigger: a PINNED server
			// rejected the device code (revoked server-side). Plaintext 401s,
			// network/TLS failures, and non-401 statuses never reach here.
			// QuarantineCacheFor returns nil on a clean/idempotent destruction —
			// the trigger itself raises the sentinel then; its DEGRADED error
			// already wraps ErrCacheQuarantined. Plan 40 T5: the destruction
			// targets THIS pull's instance slot, never the default one.
			_, qerr := QuarantineCacheFor(o.Instance, serverRejectedReason)
			if qerr == nil {
				qerr = ErrCacheQuarantined
			}
			return PullResult{}, fmt.Errorf("pull: %w — re-enroll with a fresh device code", qerr)
		}
		if res.StatusCode == 403 {
			// Plan 39: the serve refused an UNBOUND device code (legacy
			// pre-Plan-39 code, not yet `cache-tokens bind`-ed). Deliberately
			// NOT a quarantine face — the cache stays; the owner repairs the
			// binding server-side and the next pull succeeds. The discriminator
			// is the serve's own body text; anything else 403 (fail-closed
			// gate, proxy, WAF) gets the generic treatment with the body
			// excerpt, never the bind advice.
			if bytes.Contains(errBody, []byte("not bound to a profile")) {
				return PullResult{}, fmt.Errorf("pull: server returned 403 — device code not bound to a profile (owner: run `ssh-manager cache-tokens bind <name> <profile>` on the server)")
			}
			if detail := strings.TrimSpace(string(errBody)); detail != "" {
				return PullResult{}, fmt.Errorf("pull: server returned 403 — %.200s", detail)
			}
			return PullResult{}, fmt.Errorf("pull: server returned 403 (forbidden)")
		}
		return PullResult{}, fmt.Errorf("pull: server returned %d (is the authorization code valid/active?)", res.StatusCode)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return PullResult{}, err
	}
	// Plan 40 §1 (P0): the anchor is a FACT — "the pinned server said THIS at
	// this pull" — so recording it is gated on the SAFETY precondition
	// (pin != "" — a pinned TLS response's Date is uninjectable), NOT on the
	// pulling process's policy env. Production incident this closes: the env-less
	// TUI sync pull overwrote server_anchored=false, and every env-carrying
	// mcp --cache then refused to load (a self-inflicted loop — the third
	// "env must be on both sides" trap). Post-P0 the never-anchors form is
	// plaintext ONLY; the B-on plaintext refusal above stays policy-driven.
	pulledAt := time.Now().Unix()
	anchored := false
	if pin != "" {
		serverTime, perr := http.ParseTime(res.Header.Get("Date"))
		if perr != nil {
			return PullResult{}, fmt.Errorf("pull succeeded but the response has no valid Date header — refusing to anchor cache time (a pinned pull requires a trusted server clock)")
		}
		if skew := time.Since(serverTime); skew > cacheSkewTolerance || skew < -cacheSkewTolerance {
			return PullResult{}, fmt.Errorf("server clock skew too large (server %s vs local %s, cap 1h) — refusing pull: the cache time anchor depends on an accurate clock; fix system time sync",
				serverTime.Format(time.RFC3339), time.Now().Format(time.RFC3339))
		}
		pulledAt = serverTime.Unix()
		anchored = true
	}
	// Plan 40 T5: the paths were resolved at the TOP of DoPull (before the
	// cap precheck); the identity gates below consume them — a refusal leaves
	// the old cache byte-identical.
	// Plan 40 §2.4: the device identity comes from the pinned response ONLY
	// (plaintext headers are injectable). The default-instance identity gate
	// runs BEFORE any write (bin included) — a refusal leaves the old cache
	// byte-identical.
	deviceName := ""
	if pin != "" {
		deviceName = res.Header.Get("X-Sshmgr-Device-Name")
	}
	if o.Instance != "" {
		if err := gateNamedInstance(bin, metaPath, deviceName, o.Instance); err != nil {
			return PullResult{}, err
		}
	} else if pin != "" && deviceName != "" && instname.Valid(deviceName) == nil && vacuumCandidate {
		// Plan 40 批2 §1.2-3: first-enroll auto-relocation. Retarget by re-running
		// the WHOLE CachePathsFor resolution under the header name — the audit
		// sidecar & quarantine subtree follow the same resolution, no path
		// stragglers (spec §1.2-3, rev3 §2.2 全消费面纪律).
		ndir, nbin, nmeta, _, rerr := CachePathsFor(deviceName)
		if rerr != nil {
			return PullResult{}, rerr
		}
		dir, bin, metaPath = ndir, nbin, nmeta
		// §1.2-4: target-slot gate, EXACT identity (header==name trivially holds;
		// physical collision & half-write checks still run — relocation is not a
		// bypass. meta identity match = idempotent re-relocation, allowed).
		if gerr := gateNamedInstance(nbin, nmeta, deviceName, deviceName); gerr != nil {
			return PullResult{}, gerr
		}
		// §1.2-5: target-slot cap check — file validity INDEPENDENT of env (T3's
		// validateCapFileIndependent), then re-resolve the effective value for
		// the target slot.
		if verr := validateCapFileIndependent(ndir); verr != nil {
			return PullResult{}, verr
		}
		if maxOffline, err = resolveMaxOffline(ndir); err != nil {
			return PullResult{}, err
		}
		o.Instance = deviceName
		if o.StatusOut != nil {
			fmt.Fprintf(o.StatusOut, "first enroll located to instance %s — mcp --cache needs --instance %s in .mcp.json (bare cache pull re-locates idempotently; only the agent's cache-mode launch is affected)\n", deviceName, deviceName)
		}
	} else if err := gateDefaultInstance(bin, metaPath, deviceName, o); err != nil {
		return PullResult{}, err
	}
	if dek == nil {
		// Deferred load (vacuum-candidate path): post-gates per §1.2-6 — a
		// refused pull never created any DEK file.
		dek, err = loadOrCreateDEK(o.Instance)
		if err != nil {
			return PullResult{}, err
		}
	}
	blob, err := vaultio.EncryptWithKey(dek, body)
	if err != nil {
		return PullResult{}, err
	}
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
		return PullResult{}, err
	}
	// Plan 37 §2.4 commit order, pinned: bin first, meta LAST — a crash can
	// leave (new bin + old anchor) whose age error is bounded <= 2K, never
	// (new anchor + old bin). Meta-write failure stays the legacy WARNING.
	if err := atomicWriteUnique(bin, blob); err != nil {
		return PullResult{}, err
	}
	if failNextMetaWriteForTest {
		failNextMetaWriteForTest = false
		if o.StatusOut != nil {
			fmt.Fprintf(o.StatusOut, "WARNING: cache.meta.json write failed (source URL will show as unknown): test-injected failure\n")
		}
	} else {
		scoped := res.Header.Get("X-Sshmgr-Snapshot-Scope") == "profile"
		mb, _ := json.Marshal(cacheMeta{URL: url, PulledAt: pulledAt, ServerAnchored: anchored, Scoped: scoped, DeviceName: deviceName})
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
	return PullResult{Instance: o.Instance}, nil
}

// gateDefaultInstance enforces the §2.4 three-branch identity gate on the
// DEFAULT slot. Active only when the default cache.bin exists (auth.json alone
// is pull credentials, not material — §9.10). deviceName=="" means "no
// trustworthy name" (old serve, or plaintext): the gate is skipped with an
// upgrade hint (pre-existing exposure on old-serve topologies, not a new one).
func gateDefaultInstance(bin, metaPath, deviceName string, o PullOpts) error {
	if deviceName != "" {
		if verr := instname.Valid(deviceName); verr != nil {
			return fmt.Errorf("pull refused: %w — owner: revoke and re-add the device code with a valid name", verr)
		}
	}
	if _, serr := os.Stat(bin); serr != nil {
		if os.IsNotExist(serr) {
			return nil // vacuum / auth-only: no material to protect; the pull records identity
		}
		return serr
	}
	if deviceName == "" {
		if o.StatusOut != nil {
			fmt.Fprintf(o.StatusOut, "WARNING: serve did not send X-Sshmgr-Device-Name (pre-Plan-40 serve) — the default-cache identity gate is inactive until the serve is upgraded\n")
		}
		return nil
	}
	m, merr := readCacheMeta(metaPath)
	switch {
	case merr == nil && m.DeviceName != "":
		if m.DeviceName == deviceName {
			return nil // same device re-pulling its own cache
		}
		return fmt.Errorf("refusing pull: this cache belongs to device %q but the presented device code is %q — pick one:\n"+
			"  1. this is a SECOND device on this machine: re-run the pull with --instance %q\n"+
			"  2. replace the default instance's device code: delete cache.auth.json + cache.bin + cache.meta.json + the quarantine/ dir in this cache directory and re-enroll\n"+
			"  3. owner: verify which device this code was issued for (`cache-tokens ls` on the server)", m.DeviceName, deviceName, deviceName)
	case merr == nil:
		return nil // legacy unregistered meta: adopt — the write below backfills device_name (§5 zero-migration)
	default:
		return fmt.Errorf("refusing pull: cache.bin exists but cache.meta.json is missing or unreadable (%v) — inconsistent/interrupted cache; delete cache.bin + cache.meta.json + cache.auth.json + the quarantine/ dir in this cache directory and re-enroll", merr)
	}
}

// gateNamedInstance enforces §2.4 row 1 + §2.1 for an explicit --instance pull:
// the instance route REQUIRES a Plan-40 serve (header present), the header
// must name exactly the flagged instance (a mismatched code/flag pair would
// write one device's authorization into another's slot), and the physical slot
// must not hold a different identity or a half-written state.
func gateNamedInstance(bin, metaPath, deviceName, instance string) error {
	if deviceName == "" {
		return fmt.Errorf("refusing pull: --instance requires a Plan-40 serve (the response carries no X-Sshmgr-Device-Name) — upgrade the serve, or drop --instance to use the default cache slot")
	}
	if verr := instname.Valid(deviceName); verr != nil {
		return fmt.Errorf("pull refused: %w — owner: revoke and re-add the device code with a valid name", verr)
	}
	if deviceName != instance {
		return fmt.Errorf("refusing pull: --instance %q does not match the serve's device name %q — each device code pulls into its own instance; use --instance %q on the machine that code was issued for", instance, deviceName, deviceName)
	}
	if _, serr := os.Stat(bin); serr != nil {
		if !os.IsNotExist(serr) {
			return serr
		}
		return nil // fresh slot
	}
	// slot has a bin: its recorded identity must be this instance's (or blank).
	m, merr := readCacheMeta(metaPath)
	if merr != nil {
		return fmt.Errorf("refusing pull: instance directory %s holds cache.bin but no readable cache.meta.json (interrupted write?) — delete the instance directory and re-enroll", filepath.Dir(bin))
	}
	if m.DeviceName != "" && m.DeviceName != deviceName {
		return fmt.Errorf("refusing pull: instance directory %s already holds a different device identity (%q vs %q) — delete the instance directory and re-enroll", filepath.Dir(bin), m.DeviceName, deviceName)
	}
	return nil
}

// CacheScopeVerified reports whether the on-disk cache was pulled from a
// Plan-39 serve (X-Sshmgr-Snapshot-Scope: profile) — i.e. cropped to the
// device's bound profile. False for pre-Plan-39 whole-vault caches AND on any
// meta read failure: the honest "unverified" answer, never a guess from
// snapshot shape (a single-profile whole-vault snapshot is shape-identical to
// a cropped one — code-review #3).
func CacheScopeVerified() bool {
	_, _, metaPath, _, err := CachePaths()
	if err != nil {
		return false
	}
	m, err := readCacheMeta(metaPath)
	if err != nil {
		return false
	}
	return m.Scoped
}

// CacheScopeVerifiedFor is the per-instance form of CacheScopeVerified (Plan 40
// 批2 T5): same fail-closed rule — a bad instance name or an unreadable meta is
// "unverified", never a guess.
func CacheScopeVerifiedFor(instance string) bool {
	_, _, metaPath, _, err := CachePathsFor(instance)
	if err != nil {
		return false
	}
	m, err := readCacheMeta(metaPath)
	if err != nil {
		return false
	}
	return m.Scoped
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
	bin      string
	instance string
	maxAge   time.Duration
	sum      []byte // SHA-256 of the served cache.bin (nil until first successful load)
}

// NewCacheReloaderFor captures the given instance's current cache.bin hash as
// the reload baseline ("" = the default instance). When paths are unavailable
// the reloader is returned empty and Check surfaces the error.
func NewCacheReloaderFor(instance string, maxAge time.Duration) *CacheReloader {
	_, bin, _, _, err := CachePathsFor(instance)
	if err != nil {
		return &CacheReloader{instance: instance, maxAge: maxAge} // Check() surfaces the error
	}
	return &CacheReloader{bin: bin, instance: instance, maxAge: maxAge, sum: fileSumOf(bin)}
}

// NewCacheReloader captures the DEFAULT instance's baseline (zero-change
// wrapper).
func NewCacheReloader(maxAge time.Duration) *CacheReloader { return NewCacheReloaderFor("", maxAge) }

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
		if err := MaybeLazyPullFor(r.instance, r.maxAge); err != nil {
			fmt.Fprintf(os.Stderr, "ssh-manager: in-session cache refresh failed: %v\n", err)
		}
		return nil, false, nil
	}
	snap, err := LoadCacheSnapshotFor(r.instance)
	if err != nil {
		return nil, false, err // corrupt/undecryptable → keep the old store, baseline NOT advanced
	}
	r.sum = s[:] // advance only on a successful load
	return snap, true, nil
}
