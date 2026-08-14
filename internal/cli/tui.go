package cli

import (
	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/tui"
)

func newTUICmd() *cobra.Command {
	var mode string
	c := &cobra.Command{
		Use:   "tui",
		Short: "Interactive console (broker: full vault management; client: connection + sync)",
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := tui.DetectMode(mode)
			if err != nil {
				return err
			}
			return tui.Run(m)
		},
	}
	c.Flags().StringVar(&mode, "mode", "", "force mode: broker|client (default: auto-detect)")
	return c
}
