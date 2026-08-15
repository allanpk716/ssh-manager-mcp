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
			// Wire the install core into the role wizard's serve step. tui
			// cannot import cli (cli imports tui for this very command —
			// import cycle), so the hook is injected here instead (Plan 19 T4).
			tui.SetServeInstaller(installServeService)
			return tui.Run(mode)
		},
	}
	c.Flags().StringVar(&mode, "mode", "", "force mode: client (default: resolve via role.json + machine probes)")
	return c
}
