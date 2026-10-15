package policy

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// 本文件实现嵌套执行的递归判定:识别把一段
// 命令文本交给另一个 shell 执行的写法——bash -c、sh -c、zsh -c(dash、
// ksh 同形)、pwsh -c / -Command、powershell -Command / -c、cmd 的 /c
// 与 /k、以及 wsl(含 wsl -e、wsl --exec、wsl -- 后接命令)。内层是
// 写死的字符串时,按内层 shell 的判定规则递归,跨 shell 同样成立
// (POSIX 命令里的 pwsh -c、Windows 命令里的 wsl 都按各自的内层 shell
// 判定);内层是拼出来的且文本带危险命令名、或嵌套超过层数上限时,
// 判需确认并给出稳定编号 NESTED-DYNAMIC / NESTED-TOO-DEEP。

// maxNestingDepth 是嵌套执行的层数上限:最外层命令算第 0 层,每识别出
// 一层嵌套执行就把内层文本加深一层,最多判到第 3 层;要拆第 4 层时
// 不再递归,内层文本含危险命令名判需确认,否则放行。
const maxNestingDepth = 3

// nestedArg 是嵌套执行识别用的一个词:文字与「是否含执行前无法确定
// 的动态成分(变量、命令替换等)」。动态词的文字保留原文,供危险
// 命令名的兜底探测使用。
type nestedArg struct {
	text    string
	dynamic bool
}

// nestedInner 是从嵌套执行调用里取出的内层命令文本。
type nestedInner struct {
	quoted  string // 内层逐字确定时,可交给内层 shell 再拆解判定的文本
	raw     string // 兜底探测危险命令名用的原文(含变量等动态写法)
	dynamic bool   // 内层含动态成分时为真,此时 quoted 不可用
}

// nestedLauncherKind 是一类嵌套执行启动器的形态:决定内层文本怎么从
// 参数里取、内层按哪种 shell 类型判定。
type nestedLauncherKind int

const (
	// launcherPOSIXShell:bash / sh / zsh / dash / ksh,-c 后跟单个参数,
	// 参数就是内层命令(再往后的参数是内层的位置参数,不算命令)。
	launcherPOSIXShell nestedLauncherKind = iota
	// launcherWindowsPowerShell:pwsh / powershell,-c 或 -Command 是
	// 最后一个带名参数,其后全部参数按空格拼回一条内层命令。
	launcherWindowsPowerShell
	// launcherWindowsCmd:cmd,/c 或 /k 之后全部参数按空格拼回一条
	// 内层命令。
	launcherWindowsCmd
	// launcherWSL:wsl,剥掉自身选项(-d、-u、--、-e 等)之后的参数
	// 拼成内层命令,交给 Linux 侧 shell,按 POSIX 判定。
	launcherWSL
)

// innerShell 返回这类启动器的内层命令按哪种 shell 类型判定。
func (k nestedLauncherKind) innerShell() ShellType {
	switch k {
	case launcherWindowsPowerShell:
		return ShellPowerShell
	case launcherWindowsCmd:
		return ShellWindowsCmd
	default:
		return ShellPOSIX
	}
}

// posixNestedLaunchers 是 POSIX 判定侧认识的嵌套执行启动器(命令名已
// 剥掉 sudo 等前缀、取过基础名之后查这张表)。
var posixNestedLaunchers = map[string]nestedLauncherKind{
	"bash": launcherPOSIXShell,
	"sh":   launcherPOSIXShell,
	"zsh":  launcherPOSIXShell,
	"dash": launcherPOSIXShell,
	"ksh":  launcherPOSIXShell,
	// su -c 的取值同样交给目标用户的 shell 执行,形态与 bash -c 一致。
	"su": launcherPOSIXShell,

	"pwsh":       launcherWindowsPowerShell,
	"powershell": launcherWindowsPowerShell,
	"cmd":        launcherWindowsCmd,
	"wsl":        launcherWSL,
}

// windowsNestedLaunchers 是 PowerShell 与 cmd 判定侧认识的嵌套执行
// 启动器(命令名已小写、剥掉路径与可执行扩展名之后查这张表)。
var windowsNestedLaunchers = map[string]nestedLauncherKind{
	"pwsh":       launcherWindowsPowerShell,
	"powershell": launcherWindowsPowerShell,
	"cmd":        launcherWindowsCmd,
	"wsl":        launcherWSL,
}

// judgeAtNestingDepth 在给定嵌套层数上判定一段命令文本:最外层命令
// 是第 0 层,每识别出一层嵌套执行(bash -c、cmd /c 等)就把内层文本
// 加深一层交给本函数。Evaluate 是第 0 层的入口。
func judgeAtNestingDepth(shell ShellType, command string, rules *RuleSet, depth int) Decision {
	switch shell {
	case ShellPOSIX:
		return evaluatePOSIXAt(command, rules, depth)
	case ShellPowerShell, ShellWindowsCmd:
		// PowerShell 与 cmd 没有可用的语法解析库,按分隔符粗略分段后判定,
		// 两类 shell 共用同一套 Windows 默认规则。
		return evaluateWindowsAt(shell, command, rules, depth)
	default:
		return Decision{
			Disposition: DispositionConfirmRequired,
			Explanation: "unknown shell type; cannot judge the command, ask the owner to confirm manually",
		}
	}
}

// nestedPOSIXDecision 对 POSIX 拆出的一条简单命令做嵌套执行识别:
// 命令名是嵌套执行启动器时,把内层文本交给对应 shell 类型递归判定,
// 结论与该命令自身的规则结论合并(由调用方取更严重的一档)。
func nestedPOSIXDecision(call *posixCall, depth int, rules *RuleSet) Decision {
	kind, ok := posixNestedLaunchers[call.name]
	if !ok {
		return Decision{Disposition: DispositionAllow}
	}
	args := make([]nestedArg, len(call.args))
	for i, a := range call.args {
		text := a.text
		if a.dynamic {
			text = a.raw
		}
		args[i] = nestedArg{text: text, dynamic: a.dynamic}
	}
	return nestedDecision(kind, args, depth, rules)
}

// nestedWindowsDecision 对 Windows 分段的一段简单命令做嵌套执行识别,
// 语义与 POSIX 侧的 nestedPOSIXDecision 相同。
func nestedWindowsDecision(seg *winSegment, depth int, rules *RuleSet) Decision {
	kind, ok := windowsNestedLaunchers[seg.name]
	if !ok {
		return Decision{Disposition: DispositionAllow}
	}
	args := make([]nestedArg, len(seg.args))
	for i, a := range seg.args {
		args[i] = nestedArg{text: a.text, dynamic: a.dynamic}
	}
	return nestedDecision(kind, args, depth, rules)
}

// nestedDecision 对一条识别出的嵌套执行调用给出结论:内层逐字确定
// 且未超层数上限时按内层 shell 递归判定(命中返回内层规则的编号);
// 内层是拼出来的、或要拆超过层数上限的一层时,内层文本含危险命令名
// 才判需确认,否则放行。取不到内层文本(没有对应旗标或旗标后面没有
// 内容)时按放行对待,整条调用当作普通命令交规则判定。
func nestedDecision(kind nestedLauncherKind, args []nestedArg, depth int, rules *RuleSet) Decision {
	inner, ok := extractNestedInner(kind, args)
	if !ok {
		return Decision{Disposition: DispositionAllow}
	}
	shell := kind.innerShell()
	if inner.dynamic {
		if nestedMentionsDangerous(shell, inner.raw) {
			return Decision{
				Disposition: DispositionConfirmRequired,
				RuleID:      "NESTED-DYNAMIC",
				Explanation: "the nested command is built at runtime, so what it will actually run cannot be judged in advance, and its text mentions a destructive command name",
				Alternative: "write the inner command literally instead of building it at runtime, or ask the owner to confirm the exact operation first",
			}
		}
		return Decision{Disposition: DispositionAllow}
	}
	if depth >= maxNestingDepth {
		if nestedMentionsDangerous(shell, inner.raw) {
			return Decision{
				Disposition: DispositionConfirmRequired,
				RuleID:      "NESTED-TOO-DEEP",
				Explanation: "the command nests more than three shells deep, beyond what the guardrail unpacks, and the inner text mentions a destructive command name",
				Alternative: "flatten the command into fewer nesting levels, or ask the owner to confirm the exact operation first",
			}
		}
		return Decision{Disposition: DispositionAllow}
	}
	return judgeAtNestingDepth(shell, inner.quoted, rules, depth+1)
}

// extractNestedInner 从嵌套执行调用的参数里取出内层命令文本。ok 为假
// 表示没取到(没有对应旗标、或旗标后面没有内容),整条调用按普通命令
// 对待。
func extractNestedInner(kind nestedLauncherKind, args []nestedArg) (nestedInner, bool) {
	switch kind {
	case launcherPOSIXShell:
		i := indexPOSIXCommandFlag(args)
		if i < 0 || i+1 >= len(args) {
			return nestedInner{}, false
		}
		a := args[i+1]
		return nestedInner{quoted: a.text, raw: a.text, dynamic: a.dynamic}, true
	case launcherWindowsPowerShell:
		i := indexFlagFold(args, "-", "-c", "-command")
		if i < 0 || i+1 >= len(args) {
			return nestedInner{}, false
		}
		return joinNestedArgs(ShellPowerShell, args[i+1:]), true
	case launcherWindowsCmd:
		i := indexFlagFold(args, "/", "/c", "/k")
		if i < 0 || i+1 >= len(args) {
			return nestedInner{}, false
		}
		return joinNestedArgs(ShellWindowsCmd, args[i+1:]), true
	default: // launcherWSL
		rest := wslCommandStart(args)
		if len(rest) == 0 {
			return nestedInner{}, false
		}
		return joinNestedArgs(ShellPOSIX, rest), true
	}
}

// indexPOSIXCommandFlag 在参数开头的选项序列里找「后跟内层命令」的
// 旗标,返回它的下标:精确的 -c,或含 c 的短旗标簇(bash -lc 这类,c
// 出现在簇里就代表命令串跟在后面)。第一个非选项参数之后的 -c 只是
// 位置参数(如 `bash script.sh -c x` 里传给脚本的参数),不算;没找到
// 返回 -1。
func indexPOSIXCommandFlag(args []nestedArg) int {
	for i, a := range args {
		if a.dynamic || !strings.HasPrefix(a.text, "-") || a.text == "-" {
			return -1
		}
		if a.text == "-c" || (!strings.HasPrefix(a.text, "--") && strings.ContainsRune(a.text, 'c')) {
			return i
		}
	}
	return -1
}

// indexFlagFold 在参数开头的选项序列里按小写化的文字找旗标(Windows
// 侧旗标不区分大小写):选项序列由所有以 optPrefix 开头的参数组成,
// 第一个不带被缀的参数起就不是启动器自己的旗标了(那之后的内容要么
// 是脚本文件名要么是内层命令本身)。没找到返回 -1;动态词不参与匹配。
func indexFlagFold(args []nestedArg, optPrefix string, flags ...string) int {
	for i, a := range args {
		if a.dynamic || !strings.HasPrefix(a.text, optPrefix) {
			return -1
		}
		low := strings.ToLower(a.text)
		for _, f := range flags {
			if low == f {
				return i
			}
		}
	}
	return -1
}

// joinNestedArgs 把旗标后面的多个参数拼回一条内层命令文本:恰好一个
// 参数时它本身就是完整的内层命令(pwsh -Command "…" 这类写法),原样
// 使用;多个参数时按空格拼接,含空白或影响分词的字符的参数重新包一层
// 引号,保证内层再拆解时分词不散。任一参数含动态成分则整层视为拼
// 出来的,原文按空格连接供兜底探测。
func joinNestedArgs(innerShell ShellType, args []nestedArg) nestedInner {
	if len(args) == 1 {
		return nestedInner{quoted: args[0].text, raw: args[0].text, dynamic: args[0].dynamic}
	}
	var raws, quoted []string
	dynamic := false
	for _, a := range args {
		raws = append(raws, a.text)
		if a.dynamic {
			dynamic = true
			continue
		}
		quoted = append(quoted, requoteNestedArg(a.text, innerShell))
	}
	return nestedInner{
		quoted:  strings.Join(quoted, " "),
		raw:     strings.Join(raws, " "),
		dynamic: dynamic,
	}
}

// requoteNestedArg 把逐字确定的参数重新包成内层 shell 里的一个词:
// 只含日常字符时原样返回;含空白、引号等会影响分词的字符时按内层
// shell 的引号惯例包一层。
func requoteNestedArg(text string, innerShell ShellType) string {
	if innerShell == ShellPowerShell || innerShell == ShellWindowsCmd {
		return requoteWindowsArg(text)
	}
	return requotePOSIXArg(text)
}

// requotePOSIXArg 用 POSIX 单引号惯例重包一个参数:内嵌的单引号写成
// 「先闭引号、再接反斜杠转义的单引号、再重开引号」的三段形式;空白、
// 引号、变量、替换、通配等字符都会触发包裹。
func requotePOSIXArg(text string) string {
	if text != "" && !strings.ContainsAny(text, " \t\n'\"$`\\;&|<>()*?[]{}~#") {
		return text
	}
	return "'" + strings.ReplaceAll(text, "'", `'\''`) + "'"
}

// requoteWindowsArg 用 Windows 引号惯例重包一个参数:默认双引号
// (判定侧的分词器对两种引号同样剥掉);参数本身含双引号时改用
// 单引号,避免提前闭合。
func requoteWindowsArg(text string) string {
	if text != "" && !strings.ContainsAny(text, " \t\n\r'\";&|") {
		return text
	}
	if strings.Contains(text, `"`) {
		return "'" + text + "'"
	}
	return `"` + text + `"`
}

// wslValueOptions 是 wsl 自身会带走一个取值的选项(发行版、用户、起始
// 目录、环境变量、shell 类型):它们的取值不属于内层命令。
var wslValueOptions = map[string]bool{
	"-d": true, "--distribution": true,
	"-u": true, "--user": true,
	"--cd": true, "--env": true, "--shell-type": true,
}

// wslCommandStart 跳过 wsl 自身的选项,返回内层命令的起始参数:
// -- 与 -e / --exec 之后的全部参数就是内层命令;其余带 - 前缀的选项
// 按是否带值跳过(带 = 的长选项取值自带);第一个不带 - 的参数起即
// 内层命令;遇到动态词也从它起算(它就是命令名,无从再拆)。
func wslCommandStart(args []nestedArg) []nestedArg {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a.dynamic {
			return args[i:]
		}
		low := strings.ToLower(a.text)
		switch {
		case low == "--" || low == "-e" || low == "--exec":
			return args[i+1:]
		case strings.HasPrefix(low, "--"):
			if !strings.Contains(low, "=") && wslValueOptions[low] && i+1 < len(args) {
				i++ // 带值选项连同取值一起跳过
			}
		case strings.HasPrefix(low, "-") && low != "-":
			if wslValueOptions[low] && i+1 < len(args) {
				i++
			}
		default:
			return args[i:]
		}
	}
	return nil
}

// nestedMentionsDangerous 报告内层文本是否出现危险命令名,按内层
// shell 类型选探测表:POSIX 用拆解失败兜底的标记表,PowerShell 与
// cmd 用 Windows 危险命令名集合(按词比对)。
func nestedMentionsDangerous(shell ShellType, text string) bool {
	switch shell {
	case ShellPowerShell, ShellWindowsCmd:
		return containsDangerousWinName(text)
	default:
		return containsDangerousName(text)
	}
}

// wordSourceText 返回一个动态词在原命令文本里的源码片段(含引号与
// 变量写法),供嵌套执行的危险命令名兜底探测;位置信息拿不到时给
// 空串。
func wordSourceText(w *syntax.Word, command string) string {
	if len(w.Parts) == 0 {
		return ""
	}
	start, end := int(w.Pos().Offset()), int(w.End().Offset())
	if start < 0 || end > len(command) || start >= end {
		return ""
	}
	return command[start:end]
}
