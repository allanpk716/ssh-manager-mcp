package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"ssh-manager-mcp/internal/buildinfo"
	"ssh-manager-mcp/internal/updater"

	"github.com/spf13/cobra"
)

// rootCommand 在 Cobra 完成错误输出后执行根命令收尾动作。包装类型必须
// 覆盖 Execute 系列 方法；直接把收尾放在 PersistentPostRunE 无法覆盖
// RunE 返回错误的情况。
type rootCommand struct {
	*cobra.Command
}

func NewRootCmd() *rootCommand {
	root := &cobra.Command{
		Use:   "sshmgr",
		Short: "Encrypted SSH credential vault and broker (MCP)",
	}
	root.AddCommand(versionCmd, newServersCmd(), newProfilesCmd(), newProjectsCmd(), newSecretsCmd(), newSitesCmd(), newNetworksCmd(), newCacheTokensCmd(), newCacheCmd(), newPairCmd(), newGCCmd(), newAuditCmd(), newUnlockCmd(), newLockCmd(), newSSHCmd(), newMCPCmd(), newServeCmd(), newTunnelsCmd(), newExportCmd(), newImportCmd(), newBackupCmd(), newMigratePathCmd(), newTUICmd(), newClearCmd(), newDoctorCmd(), newUpdateCmd(), newPolicyCmd())
	return &rootCommand{Command: root}
}

func (r *rootCommand) Execute() error {
	_, err := r.ExecuteC()
	return err
}

func (r *rootCommand) ExecuteContext(ctx context.Context) error {
	_, err := r.ExecuteContextC(ctx)
	return err
}

func (r *rootCommand) ExecuteContextC(ctx context.Context) (*cobra.Command, error) {
	cmd, err := r.Command.ExecuteContextC(ctx)
	r.finish(cmd, err)
	return cmd, err
}

func (r *rootCommand) ExecuteC() (*cobra.Command, error) {
	cmd, err := r.Command.ExecuteC()
	r.finish(cmd, err)
	return cmd, err
}

func (r *rootCommand) finish(cmd *cobra.Command, err error) {
	// 与改动前 main 的输出保持一致:错误原样打印一行,版本提示排在它之后。
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(r.Command.ErrOrStderr(), err)
	}
	writeClientVersionHint(cmd)
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
	if cmd == nil {
		return
	}
	if name := cmd.Name(); name == "update" || name == "doctor" {
		return
	}
	instance := ""
	if flag := cmd.Flags().Lookup("instance"); flag != nil {
		instance = flag.Value.String()
	}
	state, err := readClientVersionState(instance, io.Discard)
	if err != nil || !state.IsClient || state.Snapshot == nil {
		return
	}
	snap := state.Snapshot
	if hint := updater.VersionMismatchHint(buildinfo.Version, snap.RecommendedVersion); hint != "" {
		fmt.Fprintf(cmd.ErrOrStderr(), "%s\n", hint)
	}
}
