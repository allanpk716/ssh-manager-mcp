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

// MaybeLazyPull runs ONE automatic pull when cache.bin is missing / older than
// maxAge and a persisted cache.auth.json exists. maxAge<=0 disables entirely —
// INCLUDING the missing-cache case (the first pull stays a deliberate manual
// step). Errors are returned for the caller to log; never fatal.
func MaybeLazyPull(maxAge time.Duration) error {
	if maxAge <= 0 {
		return nil
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
	return DoPull(cred.URL, code, pin, PullOpts{Timeout: LazyPullTimeout, StatusOut: os.Stderr})
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
	PulledAt int64  `json:"pulled_at"` // unix seconds of the local pull
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
func LoadCacheSnapshot() (*store.Snapshot, error) {
	_, bin, _, _, err := CachePaths()
	if err != nil {
		return nil, err
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
	var client *http.Client
	if pin == "" {
		if !o.AllowPlain {
			return fmt.Errorf("no server pin provided: refusing to pull without TLS pin. " +
				"Set --pin/SSHMGR_SERVE_PIN (from `serve cert-info`), or pass --allow-plaintext for an insecure plaintext pull")
		}
		if o.StatusOut != nil {
			fmt.Fprintf(o.StatusOut, "WARNING: --allow-plaintext set: pulling over unverified HTTP (no TLS pin). /snapshot credentials travel in cleartext.\n")
		}
		client = &http.Client{}
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
		client = &http.Client{Transport: tr}
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
		return fmt.Errorf("pull: server returned %d (is the authorization code valid/active?)", res.StatusCode)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return err
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
	if err := atomicWriteUnique(bin, blob); err != nil {
		return err
	}
	// meta via the same unique-temp atomic write (was a bare os.WriteFile — torn-meta
	// risk under concurrent pulls, xcheck codex#1). A meta-write failure is a
	// WARNING, not an error: cache.bin is already atomically replaced at this
	// point, so the pull itself SUCCEEDED — returning an error would mislabel a
	// good pull as failed (only the status line's source URL is lost).
	mb, _ := json.Marshal(cacheMeta{URL: url, PulledAt: time.Now().Unix()})
	if err := atomicWriteUnique(metaPath, mb); err != nil && o.StatusOut != nil {
		fmt.Fprintf(o.StatusOut, "WARNING: cache.meta.json write failed (source URL will show as unknown): %v\n", err)
	}
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
