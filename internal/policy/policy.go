// Package policy 实现命令策略的判定内核(领域定义见仓库根目录 CONTEXT.md
// 「命令策略」一节):输入 shell 类型、命令文本与生效规则集,输出处置、
// 命中的规则编号、风险说明与更安全的替代做法。
//
// 它是纯函数:不做任何网络或磁盘读写。owner 的试算命令与 agent 执行工具
// 都只调用 Evaluate,不各自实现判定。
package policy

import (
	"fmt"
	"ssh-manager-mcp/internal/models"
)

// Disposition 是命令策略对一条命令给出的结论,三档之一:拦截、需确认、
// 放行(领域定义见 CONTEXT.md「处置」)。零值是放行;数值越大越严重,
// 组合命令取最严重的那一档。
type Disposition int

const (
	DispositionAllow           Disposition = iota // 放行
	DispositionConfirmRequired                    // 需确认
	DispositionBlock                              // 拦截
)

// String 返回处置的展示文字,与 owner 在试算命令输出里看到的一致。
func (d Disposition) String() string {
	switch d {
	case DispositionConfirmRequired:
		return "需确认"
	case DispositionBlock:
		return "拦截"
	default:
		return "放行"
	}
}

// Code 返回处置的存储稳定代号("allow" / "confirm" / "block"),用于
// vault 表与快照 JSON。它与展示文字是两回事:代号永不随语言或版本变。
func (d Disposition) Code() string {
	switch d {
	case DispositionConfirmRequired:
		return "confirm"
	case DispositionBlock:
		return "block"
	default:
		return "allow"
	}
}

// ParseDisposition 把存储代号解析回处置;未知代号报错。
func ParseDisposition(code string) (Disposition, error) {
	switch code {
	case "allow":
		return DispositionAllow, nil
	case "confirm":
		return DispositionConfirmRequired, nil
	case "block":
		return DispositionBlock, nil
	default:
		return DispositionAllow, fmt.Errorf("unknown disposition code %q (want allow, confirm or block)", code)
	}
}

// moreSevereThan 报告本处置是否比另一档更严重(拦截 > 需确认 > 放行)。
func (d Disposition) moreSevereThan(other Disposition) bool {
	return d > other
}

// Decision 是 Evaluate 的结论:处置,以及命中规则时的一行风险说明与
// 更安全的替代做法。放行时后三个字段为空。
type Decision struct {
	Disposition Disposition
	RuleID      string // 命中的规则编号;放行时为空
	Explanation string // 为什么危险;放行时为空
	Alternative string // 更安全的替代做法;放行时为空
}

// RuleSet 是 Evaluate 的生效规则集输入。默认规则内置在程序里随版本升级,
// 这里只携带权威端对它的调整与补充:被停用的默认规则编号,加上 owner
// 在权威端增加的自定义规则(随快照下发,客户端只读镜像同一形态)。
// nil 指针表示默认规则全部生效、没有自定义规则。
type RuleSet struct {
	// DisabledDefaultRuleIDs 是被 owner 按编号停用的默认规则。
	DisabledDefaultRuleIDs []string
	// CustomRules 是 owner 增加的自定义规则(结构化字段 + 可选正则,
	// 见 CustomRule)。判定时先过默认规则,再按声明顺序过自定义规则,
	// 每条简单命令取第一条命中的结论。
	CustomRules []CustomRule
}

func (r *RuleSet) isDisabled(id string) bool {
	if r == nil {
		return false
	}
	for _, d := range r.DisabledDefaultRuleIDs {
		if d == id {
			return true
		}
	}
	return false
}

// customsFor 返回适用于给定 shell 类型的自定义规则(nil 指针与空集
// 都归为无自定义规则)。
func (r *RuleSet) customsFor(shell models.ShellType) []CustomRule {
	if r == nil {
		return nil
	}
	var out []CustomRule
	for _, cr := range r.CustomRules {
		if cr.appliesToShell(shell) {
			out = append(out, cr)
		}
	}
	return out
}

// Evaluate 是命令策略的判定入口:对一条按 shell 类型拆解的命令文本,
// 依据生效规则集给出处置结论。组合命令(分号、&&、||、管道、子 shell)
// 拆成一条条简单命令分别判定,整体取最严重的处置,规则编号对应最严重
// 的那条;嵌套执行(bash -c、cmd /c、wsl 等把一段命令文本交给另一个
// shell 执行的写法)按内层 shell 递归判定,层数上限见 nested.go。
//
// 顶层还有一道 shell 标注不匹配的试判:按标注判为放行的命令,再用别的
// shell 家族语法试判同一文本,另一侧判出拦截或需确认时改判需确认
// (SHELL-MISMATCH)——服务器条目上标注错的 shell 类型不会让危险命令
// 被悄悄放行。只升到需确认、不升到拦截,也不改写标注侧本就更严重的
// 结论;只在顶层做,嵌套递归的内层 shell 是从写法里认出来的,不做试判。
func Evaluate(shell models.ShellType, command string, rules *RuleSet) Decision {
	d := judgeAtNestingDepth(shell, command, rules, 0)
	if d.Disposition != DispositionAllow || rules.isDisabled(RuleIDShellMismatch) {
		return d
	}
	for _, other := range crossJudgementShells(shell) {
		if judgeAtNestingDepth(other, command, rules, 0).Disposition != DispositionAllow {
			return Decision{
				Disposition: DispositionConfirmRequired,
				RuleID:      RuleIDShellMismatch,
				Explanation: "the command text reads as a destructive command in another shell's syntax, which usually means the server's shell annotation is wrong — judged as needing confirmation instead of allowed silently",
				Alternative: "ask the owner to check the server's shell annotation with `sshmgr servers probe-shell`, correct it if needed, then retry the command",
			}
		}
	}
	return d
}

// RuleIDShellMismatch 是 shell 标注不匹配试判的稳定规则编号:按标注
// 放行、但按别的 shell 家族语法试判危险时改判需确认。它像内置默认
// 规则一样注册在 DefaultRuleInfos 里,可按编号停用。
const RuleIDShellMismatch = "SHELL-MISMATCH"

// crossJudgementShells 返回试判要用的其它 shell 家族:POSIX 标注时用
// PowerShell 与 cmd 各试一次(PowerShell 与 cmd 共用一套 Windows 判定,
// 但自定义规则可只适用其一);PowerShell 或 cmd 标注时用 POSIX 试一次。
// 两类 Windows 之间不互试——同一套规则同一套拆解,试了也同结论。
func crossJudgementShells(shell models.ShellType) []models.ShellType {
	switch shell {
	case models.ShellPOSIX:
		return []models.ShellType{models.ShellPowerShell, models.ShellWindowsCmd}
	case models.ShellPowerShell, models.ShellWindowsCmd:
		return []models.ShellType{models.ShellPOSIX}
	default:
		return nil
	}
}

// merge 取两条结论里更严重的一条;同严重度时保留先出现的那条,
// 保证组合命令的规则编号稳定可预期。
func merge(a, b Decision) Decision {
	if b.Disposition.moreSevereThan(a.Disposition) {
		return b
	}
	return a
}
