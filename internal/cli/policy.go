package cli

import (
	"fmt"

	"ssh-manager-mcp/internal/policy"

	"github.com/spf13/cobra"
)

// newPolicyCmd:`sshmgr policy` 命令组。本票只有试算子命令 test;
// 规则的列出、增删与停用(权威端)由命令策略后续的 GitHub Issue 在同一命令组下加入。
func newPolicyCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "policy",
		Short: "Command policy: see how a command would be judged (test)",
	}
	c.AddCommand(newPolicyTestCmd())
	return c
}

// newPolicyTestCmd:`sshmgr policy test --shell posix "<command>"` 是
// owner 的试算命令:不做任何连接,只调用策略判定内核,打印处置、命中
// 规则编号、风险说明与更安全的替代做法。
func newPolicyTestCmd() *cobra.Command {
	var shell string
	c := &cobra.Command{
		Use:   "test [--shell posix|powershell|cmd] <command>",
		Short: "Show the disposition a command would get, with the rule that fires",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			shellType, err := policy.ParseShellType(shell)
			if err != nil {
				return err
			}
			decision := policy.Evaluate(shellType, args[0], nil)
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "disposition: %s\n", decision.Disposition)
			if decision.RuleID != "" {
				fmt.Fprintf(out, "rule:         %s\n", decision.RuleID)
			}
			if decision.Explanation != "" {
				fmt.Fprintf(out, "why:          %s\n", decision.Explanation)
			}
			if decision.Alternative != "" {
				fmt.Fprintf(out, "instead:      %s\n", decision.Alternative)
			}
			return nil
		},
	}
	c.Flags().StringVar(&shell, "shell", "posix", "shell type the command will run under (posix, powershell, cmd)")
	return c
}
