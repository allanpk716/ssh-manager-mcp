package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newGCCmd: `sshmgr gc [--apply]`. Dry-run (default) reports how many
// credential rows no server references via EITHER reference column; --apply
// deletes exactly those rows. host_keys / cache_tokens are never touched.
func newGCCmd() *cobra.Command {
	apply := false
	c := &cobra.Command{
		Use:   "gc",
		Short: "Find (and with --apply, delete) credential rows no server references",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			n, err := s.CountOrphanCredentials()
			if err != nil {
				return err
			}
			if !apply {
				fmt.Fprintf(cmd.OutOrStdout(), "%d orphan credential(s); rerun with --apply to delete (servers, host keys, cache tokens are never touched)\n", n)
				return nil
			}
			deleted, err := s.DeleteOrphanCredentials()
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted %d orphan credential(s)\n", deleted)
			return nil
		},
	}
	c.Flags().BoolVar(&apply, "apply", false, "actually delete (default: dry-run)")
	return c
}
