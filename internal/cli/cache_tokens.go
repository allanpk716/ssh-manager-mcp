package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/models"
)

func newCacheTokensCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cache-tokens",
		Short: "Manage device authorization codes for offline-cache pulls (owner)",
	}
	cmd.AddCommand(cacheTokensAddCmd(), cacheTokensLsCmd(), cacheTokensRevokeCmd())
	return cmd
}

func cacheTokensAddCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "add --name <device>",
		Short: "Issue a one-time device authorization code (printed once)",
		RunE: func(cmd *cobra.Command, args []string) error {
			name, _ := cmd.Flags().GetString("name")
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			_, code, err := s.AddCacheToken(name)
			if err != nil {
				return err
			}
			printCacheToken(cmd.OutOrStdout(), name, code)
			return nil
		},
	}
	c.Flags().String("name", "", "device name (e.g. laptop, desktop-2); reusable after revoke")
	_ = c.MarkFlagRequired("name")
	return c
}

func cacheTokensLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List device authorization codes (prefix + status + last pull; never the code)",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			tokens, err := s.ListCacheTokens()
			if err != nil {
				return err
			}
			for _, ct := range tokens {
				last := "never"
				if !ct.LastPullAt.IsZero() {
					last = ct.LastPullAt.Format("2006-01-02 15:04:05")
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%-16s %s prefix=%s… status=%s last_pull=%s\n",
					ct.Name, ct.ID, ct.TokenPrefix, ct.Status, last)
			}
			return nil
		},
	}
}

func cacheTokensRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke [name]",
		Args:  cobra.ExactArgs(1),
		Short: "Revoke a device authorization code (Lazy — its next pull is rejected)",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			if err := s.RevokeCacheToken(args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "revoked cache token %s (status=%s)\n", args[0], models.CacheTokenRevoked)
			return nil
		},
	}
}

// printCacheToken emits the one-time device code + the cache-pull invocation. Shown once.
func printCacheToken(out io.Writer, name, code string) {
	fmt.Fprintf(out, "Authorization code for %q (shown once): %s\n\n", name, code)
	fmt.Fprintln(out, "On the work machine:")
	fmt.Fprintf(out, "  ssh-manager cache pull --url https://<serve-host>:7878 --token %s\n", code)
}
