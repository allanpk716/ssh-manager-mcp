package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
)

func newMCPCmd() *cobra.Command {
	var token string
	var useCache bool
	var cacheMaxAge time.Duration
	c := &cobra.Command{
		Use:   "mcp",
		Short: "Run the SSH MCP server (stdio) for an AI agent",
		RunE: func(cmd *cobra.Command, args []string) error {
			if token == "" {
				return fmt.Errorf("--token is required")
			}
			if useCache {
				// ① spawn-time freshness (failure degrades to the existing cache)
				if err := clientops.MaybeLazyPull(cacheMaxAge); err != nil {
					// "serving stale cache" is only true when a cache EXISTS — with no
					// cache.bin the upcoming LoadCacheSnapshot hard-fails instead, so
					// don't promise a degradation that isn't happening.
					if cachePresent() {
						fmt.Fprintf(os.Stderr, "lazy cache pull failed (serving stale cache): %v\n", err)
					} else {
						fmt.Fprintf(os.Stderr, "lazy cache pull failed: %v\n", err)
					}
				}
				// ② hot-reload baseline BEFORE the initial load (see clientops.CacheReloader)
				rel := clientops.NewCacheReloader(cacheMaxAge)
				snap, err := clientops.LoadCacheSnapshot()
				if err != nil {
					return err
				}
				_, _, _, auditPath, err := clientops.CachePaths()
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
	c.Flags().StringVar(&token, "token", "", "project token (from `projects add`)")
	c.Flags().BoolVar(&useCache, "cache", false, "serve from the local offline cache (read-only; pulled via `cache pull`)")
	c.Flags().DurationVar(&cacheMaxAge, "cache-max-age", 30*time.Minute,
		"auto-pull the offline cache when older than this (0 disables automatic pulls entirely)")
	_ = c.MarkFlagRequired("token")
	return c
}
