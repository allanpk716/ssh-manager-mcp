package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/mcpserver"
)

// The pull/pin/cred/lazy/reload implementation moved to internal/clientops
// (zero-behavior extraction, Stream B Task 1) so the upcoming TUI can reuse it
// without importing internal/cli. This file keeps only the cobra wrappers.

// cachePresentFor reports whether the given instance's cache.bin currently
// exists (used by mcp --cache spawn logging to say "serving stale cache" only
// when there IS a cache). "" = the default instance.
func cachePresentFor(instance string) bool {
	_, bin, _, _, err := clientops.CachePathsFor(instance)
	if err != nil {
		return false
	}
	_, err = os.Stat(bin)
	return err == nil
}

func newCacheCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "cache", Short: "Offline read-only cache (pull from a serve broker)"}
	cmd.AddCommand(cachePullCmd(), cacheStatusCmd())
	return cmd
}

func cachePullCmd() *cobra.Command {
	var url, token, instance string
	c := &cobra.Command{
		Use:   "pull",
		Short: "Pull the whole vault from a serve broker into the local encrypted cache",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkInstanceFlag(instance); err != nil {
				return err
			}
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
			fp, plain := clientops.ResolvePin(os.Getenv("SSHMGR_SERVE_PIN"), pinFlag, token)
			if plain {
				allowPlain, _ := cmd.Flags().GetBool("allow-plaintext")
				if !allowPlain {
					return fmt.Errorf("no server pin provided: refusing to pull without TLS pin. " +
						"Set --pin/SSHMGR_SERVE_PIN (from `serve cert-info`), or pass --allow-plaintext for an insecure plaintext pull")
				}
				if err := clientops.DoPull(url, token, "", clientops.PullOpts{AllowPlain: true, StatusOut: cmd.ErrOrStderr(), Instance: instance}); err != nil {
					return err
				}
				return nil // plaintext pulls NEVER persist a credential (no auto-plaintext path)
			}
			if err := clientops.DoPull(url, token, fp, clientops.PullOpts{StatusOut: cmd.ErrOrStderr(), Instance: instance}); err != nil {
				if errors.Is(err, clientops.ErrCacheQuarantined) {
					// Plan 34 rev4 §3 — pinned 401: the local cache was destroyed.
					// SilenceUsage: this is a server-side rejection, not a flag typo.
					cmd.SilenceUsage = true
					return fmt.Errorf("cache was QUARANTINED: the server rejected this device code (revoked?).\nRe-enroll: obtain a fresh device code and run cache pull again.\n(detail: %v)", err)
				}
				return err
			}
			// Persist the credential for the lazy pull. Write failure is a WARNING,
			// not an error: the pull itself succeeded, but auto-refresh won't work
			// until a later successful pull — the user must hear about that.
			code := token
			if c, _, ok := clientops.SplitTokenPin(token); ok {
				code = c
			}
			if err := clientops.WriteCacheCredFor(instance, &clientops.CacheCred{URL: url, Token: code, Pin: fp}); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "WARNING: could not persist cache.auth.json (automatic refresh disabled until a successful pull): %v\n", err)
			}
			return nil
		},
	}
	c.Flags().StringVar(&url, "url", "", "serve broker URL (https://host:7878)")
	c.Flags().StringVar(&token, "token", "", "device authorization code (from `cache-tokens add`)")
	c.Flags().StringVar(&instance, "instance", "", "route this pull to instances/<name> (default slot when omitted; mutually exclusive with SSHMGR_CACHE_DIR/SSHMGR_CACHE_DEK)")
	c.Flags().String("pin", "", "server SPKI fingerprint sha256:... (or set SSHMGR_SERVE_PIN); hard-fails without it unless --allow-plaintext")
	c.Flags().Bool("allow-plaintext", false, "opt into plaintext HTTP pull when no server pin is set (insecure; default is to refuse)")
	return c
}

func cacheStatusCmd() *cobra.Command {
	var instance string
	c := &cobra.Command{
		Use:   "status",
		Short: "Show cache presence, freshness, and counts",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkInstanceFlag(instance); err != nil {
				return err
			}
			_, bin, metaPath, _, err := clientops.CachePathsFor(instance)
			if err != nil {
				return err
			}
			snap, err := clientops.LoadCacheSnapshotFor(instance)
			if err != nil {
				// Plan 34 rev4 §4: attribute a server-rejection quarantine when
				// the on-disk manifest says so; otherwise the original error.
				if msg, ok := clientops.QuarantineReport(err); ok {
					return errors.New(msg)
				}
				return err
			}
			info, _ := os.Stat(bin)
			var age string
			if info != nil {
				age = time.Since(info.ModTime()).Round(time.Second).String()
			}
			url := "(unknown)"
			scoped := false
			if mb, err := os.ReadFile(metaPath); err == nil {
				// anonymous twin of clientops' private cacheMeta: status reads
				// url + the Plan-39 scope provenance
				var m struct {
					URL    string `json:"url"`
					Scoped bool   `json:"scoped"`
				}
				if json.Unmarshal(mb, &m) == nil && m.URL != "" {
					url = m.URL
				}
				scoped = m.Scoped
			}
			// Plan 39 provenance display (code-review #3): a single-profile
			// WHOLE-VAULT cache is shape-identical to a cropped one, so the
			// profile line is only honest when the pull itself recorded the
			// serve's scope header. Otherwise say "unverified" — never let a
			// legacy cache pass as cropped.
			profile := "unverified — re-pull to crop (pre-Plan-39 snapshot or old serve)"
			if scoped {
				switch len(snap.Profiles) {
				case 1:
					profile = snap.Profiles[0].Name
				case 0:
					profile = "(none)"
				default:
					profile = "(multiple — pre-Plan-39 whole-vault snapshot)"
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "cache:    %s\nage:      %s\nservers:  %d\ncreds:    %d\nscope:    %s\nsource:   %s\n",
				bin, age, len(snap.Servers), len(snap.Credentials), profile, url)
			return nil
		},
	}
	c.Flags().StringVar(&instance, "instance", "", "show this named instance's detail (default slot when omitted; mutually exclusive with SSHMGR_CACHE_DIR/SSHMGR_CACHE_DEK)")
	return c
}
