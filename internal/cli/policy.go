package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/policy"
	"ssh-manager-mcp/internal/policymanage"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
)

// `sshmgr policy` 命令组(owner 管理命令策略的入口,ticket #15):列出、
// 增加自定义规则、删除自定义规则、按编号停用/恢复默认规则、试算。
// 管理操作经可复用层 internal/policymanage 落库并写审计(后续 TUI 页
// 调同一层,ADR 0003);本层只做姿态判定与命令行接线,不做规则判定。
//
// 姿态(权威端 / 客户端):
//   - 本机有可打开的 vault:权威端(含单机部署),全功能。
//   - 本机无 vault 但已登记为客户端(缓存凭据或命名实例在场):只读——
//     列出与试算读本地缓存快照,修改命令被明确拒绝(规则在权威端维护,
//     随 cache pull 下发,ADR 0007)。
//   - 两者皆无:无策略数据,列出与试算退回内置默认规则。
//   - vault 存在但锁定/不可读:报错不降级(与 roles.ResolveMode 同一
//     哲学:绝不把锁定着的权威机静默降成客户端)。

// policyPosture 是本机对命令策略的姿态。
type policyPosture int

const (
	policyPostureVault  policyPosture = iota // 权威端(或单机):全功能
	policyPostureClient                      // 客户端:只读(缓存快照)
	policyPostureBare                        // 无 vault 无缓存:仅内置默认规则
)

// resolvePolicyPosture 按上面的规则判定本机姿态。vault 优先:即使残留
// 的客户端登记(缓存凭据)也在,vault 在场就以权威端对待。vault 文件
// 还不存在时再尝试开一次库——权威机的第一条 vault 命令才会创建库文件
// (与 `servers add` 的既有行为一致),主键可用即按权威端对待;开不了
// (无主键)才继续看客户端登记。
func resolvePolicyPosture() (policyPosture, error) {
	if roles.VaultExists() {
		if oerr := roles.VaultOpenErr(); oerr != nil {
			return 0, fmt.Errorf("this machine has a vault but it is locked or unreadable: %w — unlock it (`sshmgr unlock`) before managing command policy; never silently degraded to client mode", oerr)
		}
		return policyPostureVault, nil
	}
	if st, err := openUnlockedStore(); err == nil {
		st.Close()
		return policyPostureVault, nil
	}
	if roles.CachePresent() {
		return policyPostureClient, nil
	}
	return policyPostureBare, nil
}

// errPolicyClientRefused 是客户端上修改命令的统一拒绝文案。
var errPolicyClientRefused = fmt.Errorf("this machine is a cache client: command policy rules are managed on the authoritative machine and arrive here with `cache pull` — this command cannot modify them locally (listing (`policy ls`) and testing (`policy test`) work from the pulled snapshot)")

// loadEffectiveRulesForJudge 取试算用的生效规则集:
//   - 权威端:打开 vault 组装(读表失败如实报错,不静默丢自定义规则);
//   - 客户端:读本地缓存快照(离线即最后一次拉到的形态);读不到时退回
//     内置默认规则并在标准错误提示一句;
//   - 无数据机器:内置默认规则。
func loadEffectiveRulesForJudge(stderr io.Writer) (*policy.RuleSet, error) {
	posture, err := resolvePolicyPosture()
	if err != nil {
		return nil, err
	}
	switch posture {
	case policyPostureVault:
		st, err := openUnlockedStore()
		if err != nil {
			return nil, err
		}
		defer st.Close()
		return policymanage.EffectiveRuleSet(st)
	case policyPostureClient:
		snap, err := clientops.LoadCacheSnapshotFor("")
		if err != nil {
			fmt.Fprintf(stderr, "note: local cache snapshot unreadable (%v) — judging with the built-in default rules only; run `cache pull` to receive the owner's rules\n", err)
			return nil, nil
		}
		return policymanage.EffectiveRuleSetFromSnapshot(snap), nil
	default:
		return nil, nil
	}
}

func newPolicyCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "policy",
		Short: "Command policy: manage custom rules, disable default rules, test a command",
	}
	c.AddCommand(
		newPolicyLsCmd(),
		newPolicyAddCmd(),
		newPolicyRmCmd(),
		newPolicyDisableCmd(),
		newPolicyEnableCmd(),
		newPolicyTestCmd(),
	)
	return c
}

// requirePolicyVault 断言本机是权威端并返回已解锁的 vault;客户端上被
// errPolicyClientRefused 拒绝,无数据机器上给出初始化指引。
func requirePolicyVault() (*store.Store, error) {
	posture, err := resolvePolicyPosture()
	if err != nil {
		return nil, err
	}
	switch posture {
	case policyPostureClient:
		return nil, errPolicyClientRefused
	case policyPostureBare:
		return nil, fmt.Errorf("no vault on this machine — command policy rules are managed where the vault lives (the authoritative machine, or this machine in single-machine use); run the setup wizard (`sshmgr` with no arguments) first")
	}
	return openUnlockedStore()
}

// newPolicyLsCmd:`sshmgr policy ls`——列出当前生效的全部规则:内置默认
// 规则(标注已停用)与自定义规则。权威端读 vault,客户端读缓存快照,
// 无数据机器退回内置默认规则并提示。
func newPolicyLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List all rules in effect: built-in defaults (marking disabled ones) + custom rules",
		RunE: func(cmd *cobra.Command, args []string) error {
			posture, err := resolvePolicyPosture()
			if err != nil {
				return err
			}
			var views []policymanage.RuleView
			switch posture {
			case policyPostureVault:
				st, err := openUnlockedStore()
				if err != nil {
					return err
				}
				defer st.Close()
				if views, err = policymanage.ListRules(st); err != nil {
					return err
				}
			case policyPostureClient:
				snap, err := clientops.LoadCacheSnapshotFor("")
				if err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "note: local cache snapshot unreadable (%v) — listing the built-in default rules only; run `cache pull` to receive the owner's rules\n", err)
				}
				views = policymanage.ListRulesFromSnapshot(snap)
			default:
				fmt.Fprintf(cmd.ErrOrStderr(), "note: no policy data on this machine — the built-in default rules are the whole rule set\n")
				views = policymanage.ListRulesFromSnapshot(nil)
			}
			out := cmd.OutOrStdout()
			for _, v := range views {
				printPolicyRuleLine(out, v)
			}
			return nil
		},
	}
}

// printPolicyRuleLine 打一行规则:种类、编号、处置、风险说明,以及附加
// 条件摘要与停用标注。
func printPolicyRuleLine(out io.Writer, v policymanage.RuleView) {
	kind := "default"
	if v.Custom {
		kind = "custom"
	}
	line := fmt.Sprintf("%-8s %-28s %-4s %s", kind, v.ID, v.Disposition.String(), v.Explanation)
	if v.Custom {
		var conds []string
		if len(v.Commands) > 0 {
			conds = append(conds, "commands: "+strings.Join(v.Commands, ", "))
		}
		if len(v.Args) > 0 {
			conds = append(conds, "args: "+strings.Join(v.Args, ", "))
		}
		if v.PathCategory != "" {
			conds = append(conds, "path: "+v.PathCategory)
		}
		if v.Regex != "" {
			conds = append(conds, "regex: "+v.Regex)
		}
		if len(conds) > 0 {
			line += "  (" + strings.Join(conds, "; ") + ")"
		}
	} else if v.Disabled {
		line += "  [已停用]"
	}
	fmt.Fprintln(out, line)
}

// newPolicyAddCmd:`sshmgr policy add`——用结构化字段增加一条自定义规则;
// 正则是高级选项。字段经 policymanage 校验、落库、写审计。
func newPolicyAddCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "add --id <id> --disposition block|confirm [--shell posix]... [--command <name>]... [--arg <text>]... [--path-category critical|disk] [--regex <pattern>] --why <text> [--instead <text>]",
		Short: "Add a custom rule described with structured fields (regex is the advanced option)",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := requirePolicyVault()
			if err != nil {
				return err
			}
			defer st.Close()
			id, _ := cmd.Flags().GetString("id")
			dispositionStr, _ := cmd.Flags().GetString("disposition")
			disposition, err := policy.ParseDisposition(dispositionStr)
			if err != nil {
				return err
			}
			shellStrs, _ := cmd.Flags().GetStringSlice("shell")
			shells := make([]policy.ShellType, 0, len(shellStrs))
			for _, sh := range shellStrs {
				sht, perr := policy.ParseShellType(sh)
				if perr != nil {
					return perr
				}
				shells = append(shells, sht)
			}
			commands, _ := cmd.Flags().GetStringSlice("command")
			argConds, _ := cmd.Flags().GetStringSlice("arg")
			pathCategory, _ := cmd.Flags().GetString("path-category")
			regex, _ := cmd.Flags().GetString("regex")
			why, _ := cmd.Flags().GetString("why")
			instead, _ := cmd.Flags().GetString("instead")
			rule := policy.CustomRule{
				ID:           id,
				Shells:       shells,
				Commands:     commands,
				Args:         argConds,
				PathCategory: pathCategory,
				Regex:        regex,
				Disposition:  disposition,
				Explanation:  why,
				Alternative:  instead,
			}
			if err := policymanage.AddCustomRule(st, rule); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "added custom rule %s (disposition %s)\n", rule.ID, rule.Disposition.Code())
			return nil
		},
	}
	c.Flags().String("id", "", "rule id (unique; must not collide with a built-in default rule id)")
	_ = c.MarkFlagRequired("id")
	c.Flags().StringSlice("shell", nil, "applicable shell types (posix, powershell, cmd); repeat or comma-separate; empty = all shells")
	c.Flags().StringSlice("command", nil, "command name or alias the rule applies to; repeat or comma-separate; empty = not limited by command name")
	c.Flags().StringSlice("arg", nil, "argument condition: must appear verbatim among the command's arguments; repeat or comma-separate")
	c.Flags().String("path-category", "", "target path category: critical (a system-critical path) or disk (a raw disk device under /dev/, POSIX shells only; it never matches on Windows); empty = any path")
	c.Flags().String("regex", "", "advanced option: regular expression matched against the whole command text (structured fields stay in effect alongside)")
	c.Flags().String("disposition", "", "what happens when the rule fires: block or confirm")
	_ = c.MarkFlagRequired("disposition")
	c.Flags().String("why", "", "why the command is dangerous (shown to the agent when the rule fires)")
	_ = c.MarkFlagRequired("why")
	c.Flags().String("instead", "", "safer alternative (shown to the agent when the rule fires)")
	return c
}

// newPolicyRmCmd:`sshmgr policy rm <id>`——删除一条自定义规则。
func newPolicyRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <id>",
		Args:  cobra.ExactArgs(1),
		Short: "Delete a custom rule by id",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := requirePolicyVault()
			if err != nil {
				return err
			}
			defer st.Close()
			if err := policymanage.DeleteCustomRule(st, args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted custom rule %s\n", args[0])
			return nil
		},
	}
}

// newPolicyDisableCmd:`sshmgr policy disable <id>`——按编号停用一条默认
// 规则(默认规则只能停用,不能改写;停用是 owner 自担风险的放开)。
func newPolicyDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disable <default-rule-id>",
		Args:  cobra.ExactArgs(1),
		Short: "Disable a built-in default rule by id (default rules can be disabled, never rewritten)",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := requirePolicyVault()
			if err != nil {
				return err
			}
			defer st.Close()
			inserted, err := policymanage.DisableDefaultRule(st, args[0])
			if err != nil {
				return err
			}
			if inserted {
				fmt.Fprintf(cmd.OutOrStdout(), "disabled default rule %s — it no longer matches; you carry the risk it was covering\n", args[0])
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "default rule %s was already disabled\n", args[0])
			}
			return nil
		},
	}
}

// newPolicyEnableCmd:`sshmgr policy enable <id>`——恢复一条已停用的默认规则。
func newPolicyEnableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "enable <default-rule-id>",
		Args:  cobra.ExactArgs(1),
		Short: "Re-enable a disabled built-in default rule by id",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := requirePolicyVault()
			if err != nil {
				return err
			}
			defer st.Close()
			removed, err := policymanage.EnableDefaultRule(st, args[0])
			if err != nil {
				return err
			}
			if removed {
				fmt.Fprintf(cmd.OutOrStdout(), "re-enabled default rule %s — it matches again\n", args[0])
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "default rule %s was not disabled\n", args[0])
			}
			return nil
		},
	}
}

// newPolicyTestCmd:`sshmgr policy test --shell posix "<command>"` 是
// owner 的试算命令:不做任何连接,按本机姿态取生效规则集(权威端读
// vault,客户端读缓存快照,无数据机器用内置默认规则)后调用策略判定
// 内核,打印处置、命中规则编号、风险说明与更安全的替代做法。
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
			rules, err := loadEffectiveRulesForJudge(cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			decision := policy.Evaluate(shellType, args[0], rules)
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
