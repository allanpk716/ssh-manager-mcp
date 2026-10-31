package cli

import (
	"fmt"

	"ssh-manager-mcp/internal/buildinfo"
	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/updater"

	"github.com/spf13/cobra"
)

func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "sshmgr",
		Short: "Encrypted SSH credential vault and broker (MCP)",
		PersistentPostRunE: func(cmd *cobra.Command, _ []string) error {
			writeClientVersionHint(cmd)
			return nil
		},
	}
	root.AddCommand(versionCmd, newServersCmd(), newProfilesCmd(), newProjectsCmd(), newCacheTokensCmd(), newCacheCmd(), newPairCmd(), newGCCmd(), newAuditCmd(), newUnlockCmd(), newLockCmd(), newSSHCmd(), newMCPCmd(), newServeCmd(), newTunnelsCmd(), newExportCmd(), newImportCmd(), newBackupCmd(), newMigratePathCmd(), newTUICmd(), newClearCmd(), newDoctorCmd(), newUpdateCmd(), newPolicyCmd())
	return root
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print build version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Fprintln(cmd.OutOrStdout(), buildinfo.Version)
	},
}

// writeClientVersionHint 是根命令收尾时的客户端版本提示。它只读取本地加密
// 缓存；update 与 doctor 已经展示两边版本，不再重复提示。
func writeClientVersionHint(cmd *cobra.Command) {
	if name := cmd.Name(); name == "update" || name == "doctor" {
		return
	}
	state, err := roles.Load()
	if err != nil || state == nil || state.Role != roles.RoleClient {
		return
	}
	instance := ""
	if flag := cmd.Flags().Lookup("instance"); flag != nil {
		instance = flag.Value.String()
	}
	snap, err := clientops.LoadCacheSnapshotFor(instance)
	if err != nil || snap == nil {
		return
	}
	if hint := updater.VersionMismatchHint(buildinfo.Version, snap.RecommendedVersion); hint != "" {
		fmt.Fprintf(cmd.ErrOrStderr(), "%s\n", hint)
	}
}
