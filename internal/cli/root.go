package cli

import (
	"fmt"

	"ssh-manager-mcp/internal/buildinfo"

	"github.com/spf13/cobra"
)

func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "ssh-manager",
		Short: "Encrypted SSH credential vault and broker (MCP)",
	}
	root.AddCommand(versionCmd, newServersCmd(), newProfilesCmd(), newProjectsCmd(), newCacheTokensCmd(), newCacheCmd(), newGCCmd(), newUnlockCmd(), newLockCmd(), newSSHCmd(), newMCPCmd(), newServeCmd(), newExportCmd(), newImportCmd(), newBackupCmd(), newMigratePathCmd(), newTUICmd(), newClearCmd())
	return root
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print build version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Fprintln(cmd.OutOrStdout(), buildinfo.Version)
	},
}
