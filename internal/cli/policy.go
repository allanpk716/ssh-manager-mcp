package cli

import (
	"fmt"
	"io"
	"os"
	"ssh-manager-mcp/internal/models"
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
// 调同一层,ADR 0003);本层只做本机角色判定与命令行接线,不做规则判定。
//
// 本机角色(权威端 / 客户端):
//   - 本机有可打开的 vault:权威端(含单机部署),全功能。
//   - 本机无 vault 但已登记为客户端(缓存凭据或命名实例在场):只读——
//     列出与试算读本地缓存快照,修改命令被明确拒绝(规则在权威端维护,
//     随 cache pull 下发,ADR 0007)。
//   - 两者皆无:无策略数据,列出与试算退回内置默认规则。
//   - vault 存在但锁定/不可读:报错不降级(与 roles.ResolveMode 同一
//     哲学:绝不把锁定着的权威机静默降成客户端)。

// policyRole 是本机在命令策略上的角色;未登记的机器单列一档。
type policyRole int

const (
	policyRoleAuthority  policyRole = iota // 权威端(或单机):全功能
	policyRoleClient                       // 客户端:只读(缓存快照)
	policyRoleUnenrolled                   // 无 vault 无缓存:仅内置默认规则
)

// resolvePolicyRole 按上面的规则判定本机角色。vault 优先:即使残留
// 的客户端登记(缓存凭据)也在,vault 在场就以权威端对待。vault 文件
// 还不存在时再尝试开一次库——权威机的第一条 vault 命令才会创建库文件
// (与 `servers add` 的既有行为一致),主键可用即按权威端对待;开不了
// (无主键)才继续看客户端登记。
func resolvePolicyRole() (policyRole, error) {
	if roles.VaultExists() {
		if oerr := roles.VaultOpenErr(); oerr != nil {
			return 0, fmt.Errorf("this machine has a vault but it is locked or unreadable: %w — unlock it (`sshmgr unlock`) before managing command policy; never silently degraded to client mode", oerr)
		}
		return policyRoleAuthority, nil
	}
	if st, err := openUnlockedStore(); err == nil {
		st.Close()
		return policyRoleAuthority, nil
	}
	if roles.CachePresent() {
		return policyRoleClient, nil
	}
	return policyRoleUnenrolled, nil
}

// errPolicyClientRefused 是客户端上修改命令的统一拒绝文案。
var errPolicyClientRefused = fmt.Errorf("this machine is a cache client: command policy rules are managed on the authoritative machine and arrive here with `cache pull` — this command cannot modify them locally (listing (`policy ls`) and testing (`policy test`) work from the pulled snapshot)")

// loadEffectiveRulesForJudge 取试算用的生效规则集:
//   - 权威端:打开 vault 组装(读表失败如实报错,不静默丢自定义规则);
//   - 客户端:读本地缓存快照(离线即最后一次拉到的形态),缓存槽的选法
//     见 loadPolicyClientSnapshot;
//   - 无数据机器:内置默认规则。
func loadEffectiveRulesForJudge(stderr io.Writer, instance string) (*policy.RuleSet, error) {
	role, err := resolvePolicyRoleFor(instance)
	if err != nil {
		return nil, err
	}
	switch role {
	case policyRoleAuthority:
		st, err := openUnlockedStore()
		if err != nil {
			return nil, err
		}
		defer st.Close()
		return policymanage.EffectiveRuleSet(st)
	case policyRoleClient:
		snap, err := loadPolicyClientSnapshot(stderr, instance, "judging")
		if err != nil {
			return nil, err
		}
		if snap == nil {
			return nil, nil
		}
		return policymanage.EffectiveRuleSetFromSnapshot(snap), nil
	default:
		if instance != "" {
			return nil, errPolicyNoSuchInstance(instance)
		}
		return nil, nil
	}
}

// errPolicyNoSuchInstance:未登记的机器上点名了缓存实例。
func errPolicyNoSuchInstance(instance string) error {
	return fmt.Errorf("cache instance %q not found — this machine has no cache instances (`cache status` lists them)", instance)
}

// resolvePolicyRoleFor 在 resolvePolicyRole 之上校验 --instance:它只在
// 客户端上有意义(权威端读 vault,给了就报错,不静默忽略),并与
// SSHMGR_CACHE_DIR / SSHMGR_CACHE_DEK 互斥、名字过白名单。
func resolvePolicyRoleFor(instance string) (policyRole, error) {
	if err := checkInstanceFlag(instance); err != nil {
		return 0, err
	}
	role, err := resolvePolicyRole()
	if err != nil {
		return 0, err
	}
	if instance != "" && role == policyRoleAuthority {
		return 0, fmt.Errorf("--instance selects a cache instance on a cache client, but this machine holds the vault (authoritative machine or single-machine use) — its rules are read from the vault; drop --instance")
	}
	return role, nil
}

// loadPolicyClientSnapshot 读客户端的缓存快照给列出与试算用。what 是
// 退回提示里的动词(listing / judging)。
//   - 给了 --instance:读该命名实例;读不到如实报错,不退回内置默认规则。
//   - 没给:读默认槽。默认槽没有缓存而本机有命名实例时报错并列出实例名
//     ——静默退回内置默认规则会漏掉权威端下发的自定义规则,试算与 agent
//     的真实执行不一致;即使只有一个命名实例也不替用户选(与 `mcp --cache`
//     一致)。SSHMGR_CACHE_DIR 整体改写了默认槽,设了它就不看命名实例。
//   - 默认槽有缓存但读不了:提示一句,返回 nil(调用方按内置默认规则)。
func loadPolicyClientSnapshot(stderr io.Writer, instance, what string) (*store.Snapshot, error) {
	if instance != "" {
		snap, err := clientops.LoadCacheSnapshotFor(instance)
		if err != nil {
			return nil, fmt.Errorf("cache instance %q: %w", instance, err)
		}
		return snap, nil
	}
	if os.Getenv("SSHMGR_CACHE_DIR") == "" && !cachePresentFor("") {
		if names, lerr := clientops.ListInstances(); lerr == nil && len(names) > 0 {
			return nil, fmt.Errorf("no cache in the default slot, but %d named instance(s) exist: %s — pass --instance <name> to read that instance's rules (the instance is never guessed)", len(names), strings.Join(names, ", "))
		}
	}
	snap, err := clientops.LoadCacheSnapshotFor("")
	if err != nil {
		fmt.Fprintf(stderr, "note: local cache snapshot unreadable (%v) — %s with the built-in default rules only; run `cache pull` to receive the owner's rules\n", err, what)
		return nil, nil
	}
	return snap, nil
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
	role, err := resolvePolicyRole()
	if err != nil {
		return nil, err
	}
	switch role {
	case policyRoleClient:
		return nil, errPolicyClientRefused
	case policyRoleUnenrolled:
		return nil, fmt.Errorf("no vault on this machine — command policy rules are managed where the vault lives (the authoritative machine, or this machine in single-machine use); run the setup wizard (`sshmgr` with no arguments) first")
	}
	return openUnlockedStore()
}

// newPolicyLsCmd:`sshmgr policy ls`——列出当前生效的全部规则:内置默认
// 规则(标注已停用)与自定义规则。权威端读 vault,客户端读缓存快照
// (--instance 选命名实例),无数据机器退回内置默认规则并提示。
func newPolicyLsCmd() *cobra.Command {
	var instance string
	c := &cobra.Command{
		Use:   "ls [--instance <name>]",
		Short: "List all rules in effect: built-in defaults (marking disabled ones) + custom rules",
		RunE: func(cmd *cobra.Command, args []string) error {
			role, err := resolvePolicyRoleFor(instance)
			if err != nil {
				return err
			}
			var views []policymanage.RuleView
			switch role {
			case policyRoleAuthority:
				st, err := openUnlockedStore()
				if err != nil {
					return err
				}
				defer st.Close()
				if views, err = policymanage.ListRules(st); err != nil {
					return err
				}
			case policyRoleClient:
				snap, err := loadPolicyClientSnapshot(cmd.ErrOrStderr(), instance, "listing")
				if err != nil {
					return err
				}
				views = policymanage.ListRulesFromSnapshot(snap)
			default:
				if instance != "" {
					return errPolicyNoSuchInstance(instance)
				}
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
	c.Flags().StringVar(&instance, "instance", "", policyInstanceFlagHelp)
	return c
}

// policyInstanceFlagHelp 是 policy ls / test 的 --instance 说明。
const policyInstanceFlagHelp = "on a cache client, read the rules from the named instance instances/<name> (default slot when omitted; required when only named instances exist; mutually exclusive with SSHMGR_CACHE_DIR/SSHMGR_CACHE_DEK)"

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
			shells := make([]models.ShellType, 0, len(shellStrs))
			for _, sh := range shellStrs {
				sht, perr := models.ParseShellType(sh)
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
// owner 的试算命令:不做任何连接,按本机角色取生效规则集(权威端读
// vault,客户端读缓存快照,无数据机器用内置默认规则)后调用策略判定
// 内核,打印处置、命中规则编号、风险说明与更安全的替代做法。
func newPolicyTestCmd() *cobra.Command {
	var shell, instance string
	c := &cobra.Command{
		Use:   "test [--shell posix|powershell|cmd] [--instance <name>] <command>",
		Short: "Show the disposition a command would get, with the rule that fires",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			shellType, err := models.ParseShellType(shell)
			if err != nil {
				return err
			}
			rules, err := loadEffectiveRulesForJudge(cmd.ErrOrStderr(), instance)
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
	c.Flags().StringVar(&instance, "instance", "", policyInstanceFlagHelp)
	return c
}
