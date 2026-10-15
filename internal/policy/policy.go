// Package policy 实现命令策略的判定内核(领域定义见仓库根目录 CONTEXT.md
// 「命令策略」一节):输入 shell 类型、命令文本与生效规则集,输出处置、
// 命中的规则编号、风险说明与更安全的替代做法。
//
// 它是纯函数:不做任何网络或磁盘读写。owner 的试算命令与 agent 执行工具
// 都只调用 Evaluate,不各自实现判定。
package policy

import (
	"fmt"
	"strconv"
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

// ShellType 是标注在服务器条目上的 shell 类型,决定命令按哪种语法拆解。
type ShellType string

const (
	// ShellPOSIX 覆盖 Linux 与 macOS 的 shell(bash / sh / zsh 等)。
	ShellPOSIX ShellType = "posix"
	// ShellPowerShell 与 ShellWindowsCmd 共用一套 Windows 默认规则
	// (粗略分段 + 不区分大小写的命令名匹配,见 windows.go 与 rules_windows.go)。
	ShellPowerShell ShellType = "powershell"
	ShellWindowsCmd ShellType = "cmd"
)

// ParseShellType 把 CLI 标志里的文字解析成 shell 类型。
func ParseShellType(s string) (ShellType, error) {
	switch ShellType(s) {
	case ShellPOSIX:
		return ShellPOSIX, nil
	case ShellPowerShell:
		return ShellPowerShell, nil
	case ShellWindowsCmd:
		return ShellWindowsCmd, nil
	default:
		return "", &InvalidShellTypeError{input: s}
	}
}

// InvalidShellTypeError 表示 --shell 标志不是三类已知 shell 之一。
type InvalidShellTypeError struct{ input string }

func (e *InvalidShellTypeError) Error() string {
	return "unknown shell type " + strconv.Quote(e.input) + " (want posix, powershell or cmd)"
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
func (r *RuleSet) customsFor(shell ShellType) []CustomRule {
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
// 的那条。
func Evaluate(shell ShellType, command string, rules *RuleSet) Decision {
	switch shell {
	case ShellPOSIX:
		return evaluatePOSIX(command, rules)
	case ShellPowerShell, ShellWindowsCmd:
		// PowerShell 与 cmd 没有可用的语法解析库,按分隔符粗略分段后判定,
		// 两类 shell 共用同一套 Windows 默认规则。
		return evaluateWindows(command, rules)
	default:
		return Decision{
			Disposition: DispositionConfirmRequired,
			Explanation: "unknown shell type; cannot judge the command, ask the owner to confirm manually",
		}
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
