package cli

import (
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
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

type cacheMeta struct {
	URL      string `json:"url"`
	PulledAt int64  `json:"pulled_at"` // unix seconds of the local pull
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
			code := token
			var client *http.Client
			if plain {
				allowPlain, _ := cmd.Flags().GetBool("allow-plaintext")
				if !allowPlain {
					return fmt.Errorf("no server pin provided: refusing to pull without TLS pin. " +
						"Set --pin/SSHMGR_SERVE_PIN (from `serve cert-info`), or pass --allow-plaintext for an insecure plaintext pull")
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "WARNING: --allow-plaintext set: pulling over unverified HTTP (no TLS pin). /snapshot credentials travel in cleartext.\n")
				client = http.DefaultClient
			} else {
				// The device code goes to the Authorization header; the pin is for TLS only.
				// If the token is "<code>:<pin>", strip the pin so the header carries just the code.
				if c, _, ok := stripEmbeddedPin(token); ok {
					code = c
				}
				// pin is set (TLS path): the URL MUST be https://, else the TLSClientConfig
				// (with the pin) is silently never used — http:// doesn't negotiate TLS, so
				// the pin would be dead and the request would go in cleartext with no warning.
				// Hard-fail instead of silently downgrading. (xcheck F8)
				if u, perr := neturl.Parse(url); perr != nil || u.Scheme != "https" {
					return fmt.Errorf("--url must be https:// when a server pin is set (got %q); "+
						"use --allow-plaintext for an explicit plaintext pull", url)
				}
				tr, err := pinningTransport(fp)
				if err != nil {
					return err
				}
				client = &http.Client{Transport: tr}
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
				// Drain + close so the client can reuse the TCP connection
				// (a non-read response body keeps the keep-alive socket half-read).
				io.Copy(io.Discard, res.Body)
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
			// Atomic write: temp + rename. A failed/interrupted pull never corrupts the prior cache.
			tmp := bin + ".tmp"
			if err := os.WriteFile(tmp, blob, 0o600); err != nil {
				os.Remove(tmp)
				return err
			}
			if err := os.Rename(tmp, bin); err != nil {
				os.Remove(tmp)
				return err
			}
			// Best-effort meta (url + pulled_at). A failure here leaves the cache valid.
			mb, _ := json.Marshal(cacheMeta{URL: url, PulledAt: time.Now().Unix()})
			_ = os.WriteFile(metaPath, mb, 0o600)

			var snap store.Snapshot
			_ = json.Unmarshal(body, &snap) // for the status line only
			fmt.Fprintf(cmd.ErrOrStderr(), "pulled %d servers / %d credentials into %s\n", len(snap.Servers), len(snap.Credentials), bin)
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
