package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/ops"
)

func newRoleCmd() *cobra.Command {
	c := &cobra.Command{Use: "role", Short: "Show this machine's role file"}
	c.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Show the role, setup state and role.json path",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			role, err := ops.ReadRole()
			if err != nil {
				return err
			}
			roleName := role.Role
			if roleName == "" {
				roleName = "尚未初始化"
			}
			setup := "未完成"
			if role.SetupComplete {
				setup = "已完成"
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "角色=%s\n初始化向导=%s\n文件路径=%s\n", roleName, setup, role.Path)
			return nil
		},
	})
	return c
}
