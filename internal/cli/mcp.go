package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
)

func newMCPCmd() *cobra.Command {
	var token string
	var useCache bool
	c := &cobra.Command{
		Use:   "mcp",
		Short: "Run the SSH MCP server (stdio) for an AI agent",
		RunE: func(cmd *cobra.Command, args []string) error {
			if token == "" {
				return fmt.Errorf("--token is required")
			}
			if useCache {
				// Offline read-only path: hydrate the pulled snapshot into a temp store, verify
				// the SAME project token against the cached projects, and run the broker
				// unchanged. Mutations are refused (ErrReadOnly); unknown host keys fail closed;
				// offline audit lands in the cache-audit.log sidecar. The residual-key guardrail
				// is irrelevant in cache mode (the agent already holds the cache.bin + DEK).
				snap, err := loadCacheSnapshot()
				if err != nil {
					return err
				}
				_, _, _, auditPath, err := cachePaths()
				if err != nil {
					return err
				}
				return mcpserver.RunStdioCache(token, snap, auditPath)
			}
			// Residual-key guardrail: warn to STDERR only (stdout is the MCP channel).
			if st, err := vault.OpenStore(keychain); err == nil {
				if found, _ := store.CheckResidualKeys(); len(found) > 0 {
					fmt.Fprintf(os.Stderr, "WARNING: ssh credential files detected at %v — hard enforcement can be bypassed by an agent that reads them directly. Remove them for full isolation.\n", found)
				}
				st.Close()
			}
			if err := mcpserver.RunStdio(token, keychain); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			return nil
		},
	}
	c.Flags().StringVar(&token, "token", "", "project token (from `projects add`)")
	c.Flags().BoolVar(&useCache, "cache", false, "serve from the local offline cache (read-only; pulled via `cache pull`)")
	_ = c.MarkFlagRequired("token")
	return c
}
