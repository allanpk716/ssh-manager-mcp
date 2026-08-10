package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Version is the build version. Defaults to "dev" for local `go build` /
// `go install`; overridden at release time via ldflags:
//
//	go build -ldflags "-X ssh-manager-mcp/internal/cli.Version=<version>"
//
// GoReleaser sets this to the tag-derived semver (tag v1.0.0 -> "1.0.0").
var Version = "dev"

func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "ssh-manager",
		Short: "Encrypted SSH credential vault and broker (MCP)",
	}
	root.AddCommand(versionCmd, newServersCmd(), newProfilesCmd(), newProjectsCmd(), newUnlockCmd(), newLockCmd(), newSSHCmd(), newMCPCmd())
	return root
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print build version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Fprintln(cmd.OutOrStdout(), Version)
	},
}
