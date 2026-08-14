package cli

import (
	"encoding/json"
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

// cachePresent reports whether cache.bin currently exists (used by mcp --cache
// spawn logging to say "serving stale cache" only when there IS a cache).
func cachePresent() bool {
	_, bin, _, _, err := clientops.CachePaths()
	if err != nil {
		return false
	}
	_, err = os.Stat(bin)
	return err == nil
}

// stripEmbeddedPin splits "<code>:<pin>" into (code, pin, ok). When the token
// has no valid embedded pin, returns the token unchanged with ok=false so the
// full token goes to the Authorization header as the device code. Uses the
// FIRST colon for the split (the pin "sha256:<hex>" contains its own colon).
//
// Local twin of clientops' private stripEmbeddedPin: the pinning/DoPull side
// lives in clientops (used internally by DoPull/MaybeLazyPull), while the CLI
// needs the split once more to persist the bare device code in cache.auth.json.
// Both copies are pinned end-to-end by TestCachePull_TokenEmbeddedPin_Succeeds
// and TestCachePull_PersistsCred_PinPathOnly.
func stripEmbeddedPin(token string) (code string, pin string, ok bool) {
	if i := strings.Index(token, ":"); i >= 0 {
		if v, parsed := mcpserver.ParsePin(token[i+1:]); parsed {
			return token[:i], v, true
		}
	}
	return token, "", false
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
			fp, plain := clientops.ResolvePin(os.Getenv("SSHMGR_SERVE_PIN"), pinFlag, token)
			if plain {
				allowPlain, _ := cmd.Flags().GetBool("allow-plaintext")
				if !allowPlain {
					return fmt.Errorf("no server pin provided: refusing to pull without TLS pin. " +
						"Set --pin/SSHMGR_SERVE_PIN (from `serve cert-info`), or pass --allow-plaintext for an insecure plaintext pull")
				}
				if err := clientops.DoPull(url, token, "", clientops.PullOpts{AllowPlain: true, StatusOut: cmd.ErrOrStderr()}); err != nil {
					return err
				}
				return nil // plaintext pulls NEVER persist a credential (no auto-plaintext path)
			}
			if err := clientops.DoPull(url, token, fp, clientops.PullOpts{StatusOut: cmd.ErrOrStderr()}); err != nil {
				return err
			}
			// Persist the credential for the lazy pull. Write failure is a WARNING,
			// not an error: the pull itself succeeded, but auto-refresh won't work
			// until a later successful pull — the user must hear about that.
			code := token
			if c, _, ok := stripEmbeddedPin(token); ok {
				code = c
			}
			if err := clientops.WriteCacheCred(&clientops.CacheCred{URL: url, Token: code, Pin: fp}); err != nil {
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
			_, bin, metaPath, _, err := clientops.CachePaths()
			if err != nil {
				return err
			}
			snap, err := clientops.LoadCacheSnapshot()
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
				// anonymous twin of clientops' private cacheMeta: status only reads url
				var m struct {
					URL string `json:"url"`
				}
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
