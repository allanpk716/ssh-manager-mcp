package cli

import (
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vaultio"
)

// dekProvider returns the KeyProvider holding the cache DEK. It is a package
// seam (defined in cache_dek_windows.go / cache_dek_unix.go, build-tag selected)
// so tests inject MemKeyProvider instead of touching the real DEK file. Both
// platforms bind a FileKeyProvider at paths.CacheDekPath() (Plan 16 T4; spec
// §3.1 / §4.2). Was DpapiKeyProvider on Windows / KeyringKeyProvider on Unix
// before Plan 16 — same plaintext-at-fixed-path trust model as master.key.

// lazyPullTimeout bounds every automatic (lazy) pull: it runs on the spawn /
// tool-call critical path, so an unreachable broker must degrade within seconds,
// not hang MCP startup (xcheck pi#2/codex#3). Manual `cache pull` stays
// unbounded — the interactive user can Ctrl-C.
const lazyPullTimeout = 10 * time.Second

// lazyPullBackoff rate-limits automatic pulls to at most one attempt per TTL
// window (success or failure), so an offline machine doesn't retry — and block a
// tool call for up to lazyPullTimeout — on every call. The cache.bin mtime only
// advances on SUCCESS, so without this backoff a failed pull would re-fire per call.
var lazyPullBackoff struct {
	mu          sync.Mutex
	lastAttempt time.Time
}

// maybeLazyPull runs ONE automatic pull when cache.bin is missing / older than
// maxAge and a persisted cache.auth.json exists. maxAge<=0 disables entirely —
// INCLUDING the missing-cache case (the first pull stays a deliberate manual
// step). Errors are returned for the caller to log; never fatal.
func maybeLazyPull(maxAge time.Duration) error {
	if maxAge <= 0 {
		return nil
	}
	cred, err := readCacheCred()
	if err != nil {
		return err
	}
	if cred == nil {
		return nil
	}
	_, bin, _, _, err := cachePaths()
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
		if c, p, ok := stripEmbeddedPin(cred.Token); ok {
			code, pin = c, p
		}
	}
	if pin == "" {
		return fmt.Errorf("cache.auth.json has no pin; refusing plaintext auto-pull")
	}
	return doPull(cred.URL, code, pin, pullOpts{timeout: lazyPullTimeout, statusOut: os.Stderr})
}

// cachePaths resolves the cache directory (SSHMGR_CACHE_DIR override, else UserConfigDir/
// ssh-manager) and the three files within it: the encrypted snapshot, the meta sidecar, and
// the offline-audit sidecar (the audit sidecar is owned by T8; T7 only resolves the path).
func cachePaths() (dir, bin, meta, audit string, err error) {
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

// cacheCred persists the pull credential (cache.auth.json) so `mcp --cache` can
// lazy-pull without env/flags. Pin is the RESOLVED effective pin from the last
// successful pull (env > flag > token-embedded) stored bare; the lazy path
// prefers it over any pin still embedded in Token (cert rotation: a manual
// --pin re-pull must override a stale embedded pin). The device code grants
// FUTURE snapshot pulls — cache.bin alone only holds the past — so this file is
// bearer-token-grade: 0600 + HardenACL on Windows, revoke on theft (threat-model §1.1).
type cacheCred struct {
	URL   string `json:"url"`
	Token string `json:"token"`         // bare device code
	Pin   string `json:"pin,omitempty"` // resolved effective pin at last successful pull
}

func cacheCredPath() (string, error) {
	dir, _, _, _, err := cachePaths()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "cache.auth.json"), nil
}

// readCacheCred returns nil, nil when the file is absent (never enrolled / not
// yet pulled). A present-but-corrupt file is an error: silently ignoring it
// would disable auto-refresh invisibly.
func readCacheCred() (*cacheCred, error) {
	p, err := cacheCredPath()
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
	var c cacheCred
	if err := json.Unmarshal(blob, &c); err != nil {
		return nil, fmt.Errorf("cache.auth.json corrupt: %w", err)
	}
	if c.URL == "" || c.Token == "" {
		return nil, fmt.Errorf("cache.auth.json incomplete (missing url/token)")
	}
	return &c, nil
}

// writeCacheCred persists the credential atomically (unique temp + rename) and
// hardens the ACL on Windows (no-op on Unix where 0600 is the protection).
func writeCacheCred(cred *cacheCred) error {
	p, err := cacheCredPath()
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

// loadOrCreateDEK returns the cache DEK from the keychain, generating + storing it on first pull.
// On subsequent pulls the existing DEK is reused, so cache.bin stays decryptable across pulls.
func loadOrCreateDEK() ([]byte, error) {
	kp := dekProvider()
	dek, err := kp.Get()
	if err == nil {
		return dek, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	dek, err = store.GenerateMasterKey()
	if err != nil {
		return nil, err
	}
	if err := kp.Set(dek); err != nil {
		return nil, err
	}
	return dek, nil
}

// loadDEK returns the cache DEK without creating it (status / mcp --cache). A missing DEK
// surfaces as store.ErrNotFound — the caller reports "run cache pull first".
func loadDEK() ([]byte, error) {
	return dekProvider().Get()
}

// loadCacheSnapshot reads + DEK-decrypts + unmarshals the cache. Shared by `cache status` and
// `mcp --cache`. Returns an error if the cache is absent / corrupt / the DEK is missing.
func loadCacheSnapshot() (*store.Snapshot, error) {
	_, bin, _, _, err := cachePaths()
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

// pullOpts tunes doPull for its caller.
type pullOpts struct {
	allowPlain bool          // plaintext opt-in — manual CLI only; the lazy path NEVER sets this
	timeout    time.Duration // >0 → overall http.Client timeout (lazy: spawn/tool-call path must be bounded)
	statusOut  io.Writer     // status/warning sink (nil → silent); CLI passes cmd.ErrOrStderr()
}

// doPull fetches /snapshot from url with the device code and atomically writes
// cache.bin + cache.meta.json. pin=="" means no pin: allowPlain must be true or
// the pull refuses (F4 hard-fail contract). pin!="" pins TLS to that SPKI
// fingerprint and requires an https:// URL (F8). Extracted from cachePullCmd so
// the spawn-lazy pull (mcp --cache) shares ONE implementation.
func doPull(url, token, pin string, o pullOpts) error {
	code := token
	var client *http.Client
	if pin == "" {
		if !o.allowPlain {
			return fmt.Errorf("no server pin provided: refusing to pull without TLS pin. " +
				"Set --pin/SSHMGR_SERVE_PIN (from `serve cert-info`), or pass --allow-plaintext for an insecure plaintext pull")
		}
		if o.statusOut != nil {
			fmt.Fprintf(o.statusOut, "WARNING: --allow-plaintext set: pulling over unverified HTTP (no TLS pin). /snapshot credentials travel in cleartext.\n")
		}
		client = &http.Client{}
	} else {
		// The device code goes to the Authorization header; the pin is for TLS only.
		// If the token is "<code>:<pin>", strip so the header carries just the code.
		if c, _, ok := stripEmbeddedPin(token); ok {
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
	if o.timeout > 0 {
		client.Timeout = o.timeout
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
	_, bin, metaPath, _, err := cachePaths()
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
	// risk under concurrent pulls, xcheck codex#1).
	mb, _ := json.Marshal(cacheMeta{URL: url, PulledAt: time.Now().Unix()})
	if err := atomicWriteUnique(metaPath, mb); err != nil {
		return err
	}
	var snap store.Snapshot
	_ = json.Unmarshal(body, &snap) // for the status line only
	if o.statusOut != nil {
		fmt.Fprintf(o.statusOut, "pulled %d servers / %d credentials into %s\n", len(snap.Servers), len(snap.Credentials), bin)
	}
	return nil
}

func newCacheCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "cache", Short: "Offline read-only cache (pull from a serve broker)"}
	cmd.AddCommand(cachePullCmd(), cacheStatusCmd())
	return cmd
}

func cachePullCmd() *cobra.Command {
	var url, token string
	c := &cobra.Command{
		Use:   "pull",
		Short: "Pull the whole vault from a serve broker into the local encrypted cache",
		RunE: func(cmd *cobra.Command, args []string) error {
			if url == "" {
				url = os.Getenv("SSHMGR_CACHE_URL")
			}
			if token == "" {
				token = os.Getenv("SSHMGR_CACHE_TOKEN")
			}
			if url == "" || token == "" {
				return fmt.Errorf("--url and --token are required (or SSHMGR_CACHE_URL / SSHMGR_CACHE_TOKEN)")
			}
			// Pin resolution priority: env SSHMGR_SERVE_PIN > --pin flag > token-embedded "<code>:<pin>".
			// plain=true means no pin anywhere. Per xcheck F4 the no-pin path hard-fails by
			// default (no silent plaintext); --allow-plaintext opts back into the legacy path.
			pinFlag, _ := cmd.Flags().GetString("pin")
			// F7: a pin-shaped but INVALID env/flag value is a hard error. We must NOT let it
			// silently fall through to plaintext (a typo in SSHMGR_SERVE_PIN must not remove
			// TLS protection). Only a fully-ABSENT pin is allowed to enter the plain branch.
			if raw := strings.TrimSpace(os.Getenv("SSHMGR_SERVE_PIN")); raw != "" {
				if _, ok := mcpserver.ParsePin(raw); !ok {
					return fmt.Errorf("SSHMGR_SERVE_PIN is set but not a valid sha256:<64hex> fingerprint: %q", raw)
				}
			}
			if raw := strings.TrimSpace(pinFlag); raw != "" {
				if _, ok := mcpserver.ParsePin(raw); !ok {
					return fmt.Errorf("--pin is not a valid sha256:<64hex> fingerprint: %q", raw)
				}
			}
			fp, plain := resolvePin(os.Getenv("SSHMGR_SERVE_PIN"), pinFlag, token)
			if plain {
				allowPlain, _ := cmd.Flags().GetBool("allow-plaintext")
				if !allowPlain {
					return fmt.Errorf("no server pin provided: refusing to pull without TLS pin. " +
						"Set --pin/SSHMGR_SERVE_PIN (from `serve cert-info`), or pass --allow-plaintext for an insecure plaintext pull")
				}
				if err := doPull(url, token, "", pullOpts{allowPlain: true, statusOut: cmd.ErrOrStderr()}); err != nil {
					return err
				}
				return nil // plaintext pulls NEVER persist a credential (no auto-plaintext path)
			}
			if err := doPull(url, token, fp, pullOpts{statusOut: cmd.ErrOrStderr()}); err != nil {
				return err
			}
			// Persist the credential for the lazy pull. Write failure is a WARNING,
			// not an error: the pull itself succeeded, but auto-refresh won't work
			// until a later successful pull — the user must hear about that.
			code := token
			if c, _, ok := stripEmbeddedPin(token); ok {
				code = c
			}
			if err := writeCacheCred(&cacheCred{URL: url, Token: code, Pin: fp}); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "WARNING: could not persist cache.auth.json (automatic refresh disabled until a successful pull): %v\n", err)
			}
			return nil
		},
	}
	c.Flags().StringVar(&url, "url", "", "serve broker URL (https://host:7878)")
	c.Flags().StringVar(&token, "token", "", "device authorization code (from `cache-tokens add`)")
	c.Flags().String("pin", "", "server SPKI fingerprint sha256:... (or set SSHMGR_SERVE_PIN); hard-fails without it unless --allow-plaintext")
	c.Flags().Bool("allow-plaintext", false, "opt into plaintext HTTP pull when no server pin is set (insecure; default is to refuse)")
	return c
}

func cacheStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show cache presence, freshness, and counts",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, bin, metaPath, _, err := cachePaths()
			if err != nil {
				return err
			}
			snap, err := loadCacheSnapshot()
			if err != nil {
				return err
			}
			info, _ := os.Stat(bin)
			var age string
			if info != nil {
				age = time.Since(info.ModTime()).Round(time.Second).String()
			}
			url := "(unknown)"
			if mb, err := os.ReadFile(metaPath); err == nil {
				var m cacheMeta
				if json.Unmarshal(mb, &m) == nil && m.URL != "" {
					url = m.URL
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "cache:    %s\nage:      %s\nservers:  %d\ncreds:    %d\nsource:   %s\n",
				bin, age, len(snap.Servers), len(snap.Credentials), url)
			return nil
		},
	}
}

// resolvePin resolves the server SPKI fingerprint by priority:
// env (SSHMGR_SERVE_PIN) > --pin flag > token-embedded "<code>:<pin>".
// Returns plain=true when no pin is available anywhere. Per xcheck F4 the
// caller now hard-fails on plain=true by default (no silent plaintext); the
// caller gates the plaintext fallback behind --allow-plaintext. Malformed
// env/flag pins are rejected earlier by cachePullCmd (F7), so by the time
// resolvePin runs an env/flag value is either empty or well-formed. A
// malformed token-embedded pin still falls through to plain here (fail-safe
// against a hand-typed token), which then hits the --allow-plaintext gate.
//
// The embedded-pin split uses the FIRST colon, not the last: the pin itself is
// "sha256:<hex>" and contains a colon, so LastIndex would split inside the pin
// and yield a bare "<hex>" that ParsePin rejects. The device code is specified
// to contain no colon, so first-colon split is unambiguous.
func resolvePin(envVal, flagVal, token string) (fp string, plain bool) {
	if v, ok := mcpserver.ParsePin(strings.TrimSpace(envVal)); ok {
		return v, false
	}
	if v, ok := mcpserver.ParsePin(strings.TrimSpace(flagVal)); ok {
		return v, false
	}
	// token-embedded: "<code>:sha256:..."
	if i := strings.Index(token, ":"); i >= 0 {
		if v, ok := mcpserver.ParsePin(token[i+1:]); ok {
			return v, false
		}
	}
	return "", true
}

// pinningTransport builds an http.Transport whose TLS handshake is pinned to fp:
// the server leaf cert's SPKI fingerprint MUST equal fp or the handshake fails.
//
// Trust model: the serve cert is SELF-SIGNED (see generateServeCert) — there is
// no external CA to chain to, and on Windows the system verifier additionally
// chokes on ed25519 ("Invalid algorithm specified"). So we cannot rely on the
// default certificate verification (it would always fail before our pin check
// ran). Instead we set BOTH InsecureSkipVerify=true (skip CA/chain/name
// verification — which is impossible for a self-signed cert anyway) AND
// VerifyConnection (enforce the SPKI pin). Per Go's crypto/tls docs,
// InsecureSkipVerify skips the default verifier but does NOT disable
// VerifyConnection, which becomes the sole trust anchor. This is the standard
// HPKP / Tailscale pinning pattern: trust comes from the pin, not from a CA.
// The pin is compared in constant time to avoid an oracle.
func pinningTransport(fp string) (*http.Transport, error) {
	want, ok := mcpserver.ParsePin(fp)
	if !ok {
		return nil, fmt.Errorf("invalid server pin format %q (want sha256:<64hex>)", fp)
	}
	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // skip CA verification (self-signed serve cert); pin below is the trust anchor
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return fmt.Errorf("server presented no certificate")
			}
			got := mcpserver.SPKIFingerprint(cs.PeerCertificates[0])
			if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
				return fmt.Errorf("server fingerprint mismatch (expected %s, got %s)", want, got)
			}
			return nil
		},
	}
	return &http.Transport{TLSClientConfig: tlsCfg}, nil
}

// stripEmbeddedPin splits "<code>:<pin>" into (code, pin, ok). When the token
// has no valid embedded pin, returns the token unchanged with ok=false so the
// full token goes to the Authorization header as the device code. Uses the
// FIRST colon for the split (the pin "sha256:<hex>" contains its own colon).
func stripEmbeddedPin(token string) (code string, pin string, ok bool) {
	if i := strings.Index(token, ":"); i >= 0 {
		if v, parsed := mcpserver.ParsePin(token[i+1:]); parsed {
			return token[:i], v, true
		}
	}
	return token, "", false
}
