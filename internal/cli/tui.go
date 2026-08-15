package cli

import (
	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/tui"
)

func newTUICmd() *cobra.Command {
	var mode string
	c := &cobra.Command{
		Use:   "tui",
		Short: "Interactive console (first run: role wizard; then broker or client per role.json)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return tui.Run(mode)
		},
	}
	c.Flags().StringVar(&mode, "mode", "", "force mode: client (default: resolve via role.json + machine probes)")
	return c
}
