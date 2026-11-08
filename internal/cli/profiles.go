package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/access"
)

// profiles 命令族是访问管理模块（internal/access）的薄命令行面：解析参
// 数、打印结果；业务规则、存储事务与审计写入都由模块统一处理。
func newProfilesCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "profiles", Short: "Manage server profiles (groups)"}
	cmd.AddCommand(profilesAddCmd(), profilesLsCmd(), profilesGrantCmd(), profilesRevokeCmd(), profilesRemoveCmd())
	return cmd
}

// profilesRemoveCmd 按名字删除 profile。仍被未吊销项目令牌或活跃设备码
// 引用时，访问管理模块拒绝并点名引用者——静默解绑会困住这些 agent。授
// 权行随 profile 一并删除。
func profilesRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove [name]",
		Args:  cobra.ExactArgs(1),
		Short: "Delete a profile (refuses while projects or device codes reference it)",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			if err := access.New(s).DeleteProfile(args[0], access.CLISource()); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted profile %s\n", args[0])
			return nil
		},
	}
}

func profilesAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add [name]",
		Args:  cobra.ExactArgs(1),
		Short: "Create a profile",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			id, err := access.New(s).AddProfile(args[0], access.CLISource())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "created profile %s id=%s\n", args[0], id)
			return nil
		},
	}
}

func profilesLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List profiles",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			profiles, err := access.New(s).ListProfiles()
			if err != nil {
				return err
			}
			for _, p := range profiles {
				fmt.Fprintf(cmd.OutOrStdout(), "%-16s %-20s servers=%d\n", p.Name, p.ID, len(p.Servers))
			}
			return nil
		},
	}
}

func profilesGrantCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "grant [profile] [server1 server2 ...]",
		Args:  cobra.MinimumNArgs(2),
		Short: "Grant servers to a profile (by name)",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			granted, err := access.New(s).GrantServers(args[0], args[1:], access.CLISource())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "granted %d server(s) to %s\n", len(granted), args[0])
			return nil
		},
	}
}

// profilesRevokeCmd 从 profile 的授权集合里摘掉服务器。服务器按名字或标
// 识符解析；本来就没授权的提示「未授权，跳过」不算错误，清理脚本可以安
// 心重复执行。
func profilesRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke [profile] [server1 server2 ...]",
		Args:  cobra.MinimumNArgs(2),
		Short: "Revoke servers from a profile (by name)",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			result, err := access.New(s).RevokeServers(args[0], args[1:], access.CLISource())
			if err != nil {
				return err
			}
			for _, srv := range result.Skipped {
				fmt.Fprintf(cmd.OutOrStdout(), "%s: 未授权，跳过\n", srv.Name)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "revoked %d server(s) from %s\n", len(result.Revoked), args[0])
			return nil
		},
	}
}
