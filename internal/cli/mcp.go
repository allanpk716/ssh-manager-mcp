package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
)

// resolveToken: flag wins, SSHMGR_TOKEN env is the fallback (identical
// semantics & downstream parsing — same name, same meaning; Plan 20 B2).
// env keeps the token OUT of the process argv (ps visibility); the flag
// path still shows it in argv.
func resolveToken(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	return os.Getenv("SSHMGR_TOKEN")
}

func newMCPCmd() *cobra.Command {
	var token string
	var useCache bool
	var instance string
	var cacheMaxAge time.Duration
	c := &cobra.Command{
		Use:   "mcp",
		Short: "Run the SSH MCP server (stdio) for an AI agent",
		RunE: func(cmd *cobra.Command, args []string) error {
			token = resolveToken(token)
			if token == "" {
				return fmt.Errorf("--token or SSHMGR_TOKEN is required")
			}
			if useCache {
				if err := checkInstanceFlag(instance); err != nil {
					return err
				}
				if instance == "" {
					// §2.5: reading an instance is ALWAYS explicit — never guess.
					if _, bin, _, _, perr := clientops.CachePaths(); perr == nil {
						if _, berr := os.Stat(bin); os.IsNotExist(berr) {
							if names, lerr := clientops.ListInstances(); lerr == nil && len(names) > 0 {
								return fmt.Errorf("no cache in the default slot, but %d named instance(s) exist: %s — pass --instance <name> (the default slot is never auto-guessed)", len(names), strings.Join(names, ", "))
							}
						}
					}
				}
				// ① spawn-time freshness (failure degrades to the existing cache)
				if err := clientops.MaybeLazyPullFor(instance, cacheMaxAge); err != nil {
					// Plan 34 final review (Minor 3): a quarantine already logged
					// two lines (QuarantineCache's step verdict + MaybeLazyPull's
					// session-disable notice) — skip a redundant third here.
					if !errors.Is(err, clientops.ErrCacheQuarantined) {
						// "serving stale cache" is only true when a cache EXISTS — with no
						// cache.bin the upcoming LoadCacheSnapshot hard-fails instead, so
						// don't promise a degradation that isn't happening.
						if cachePresentFor(instance) {
							fmt.Fprintf(os.Stderr, "lazy cache pull failed (serving stale cache): %v\n", err)
						} else {
							fmt.Fprintf(os.Stderr, "lazy cache pull failed: %v\n", err)
						}
					}
				}
				// ② hot-reload baseline BEFORE the initial load (see clientops.CacheReloader)
				rel := clientops.NewCacheReloaderFor(instance, cacheMaxAge)
				snap, err := clientops.LoadCacheSnapshotFor(instance)
				if err != nil {
					// Plan 34 rev4 §4: attribute a server-rejection quarantine when
					// the on-disk manifest says so; otherwise the original error.
					if msg, ok := clientops.QuarantineReport(err); ok {
						return errors.New(msg)
					}
					return err
				}
				_, _, _, auditPath, err := clientops.CachePathsFor(instance)
				if err != nil {
					return err
				}
				return mcpserver.RunStdioCache(token, snap, auditPath, rel.Check)
			}
			// Residual-key guardrail: warn to STDERR only (stdout is the MCP channel).
			if st, err := vault.OpenStore(store.FileKeyProvider{}); err == nil {
				if found, _ := store.CheckResidualKeys(); len(found) > 0 {
					fmt.Fprintf(os.Stderr, "WARNING: ssh credential files detected at %v — hard enforcement can be bypassed by an agent that reads them directly. Remove them for full isolation.\n", found)
				}
				st.Close()
			}
			if err := mcpserver.RunStdio(token, store.FileKeyProvider{}); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			return nil
		},
	}
	c.Flags().StringVar(&token, "token", "", "project token from `projects add` (or env SSHMGR_TOKEN)")
	c.Flags().BoolVar(&useCache, "cache", false, "serve from the local offline cache (read-only; pulled via `cache pull`)")
	c.Flags().StringVar(&instance, "instance", "", "serve the named cache instance instances/<name> (default slot when omitted; mutually exclusive with SSHMGR_CACHE_DIR/SSHMGR_CACHE_DEK)")
	c.Flags().DurationVar(&cacheMaxAge, "cache-max-age", 30*time.Minute,
		"auto-pull the offline cache when older than this (0 disables automatic pulls entirely)")
	return c
}
