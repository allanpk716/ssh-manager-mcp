package policy

import (
	"ssh-manager-mcp/internal/models"
	"strings"
)

// Windows 两类 shell(PowerShell 与 cmd)没有可用的语法解析库,按
// `;`、`&&`、`|`、`&` 与换行把命令文本粗略拆成一段段简单命令,再按
// 「命令名 + 参数」应用默认规则;命令名匹配不区分大小写并带别名表,
// 路径不区分大小写、正反斜杠等价(见 paths.go 的 Windows 规范化)。
// 精度低于 POSIX 拆解,这是如实承认的取舍(ADR 0007)。

// winToken 是粗略分段后一个词:能逐字确定时 text 是去掉引号后的文字;
// 含动态成分(PowerShell 的 $ 变量、cmd 的 % 变量%)时 dynamic 为真,
// 这类词在执行前无法知道会指向哪里,交给具体规则决定如何对待。
type winToken struct {
	text    string
	dynamic bool
}

// winSegment 是粗略拆出的一段简单命令:命令名(已小写、去引号、取基础名、
// 剥常见可执行扩展名)与参数序列。命令名本身含动态成分(如 $cmd)时
// name 为空——没有任何按命令名匹配的规则能命中它。
type winSegment struct {
	name string
	args []winToken
}

// winEvidence 是一次 Windows 判定能看到的全部事实:拆出的分段序列与
// 原始命令文本(动态执行写法与危险命令名的兜底探测要看全文)。
type winEvidence struct {
	segments   []winSegment
	rawCommand string
}

// evaluateWindowsAt 是 PowerShell 与 cmd 共用的判定入口:粗略分段后
// 逐段应用默认规则,再过适用的自定义规则,整体取最严重的处置,规则
// 编号对应最严重的那条。shell 参数用于自定义规则的适用 shell 集合
// 过滤;depth 是当前文本的嵌套层数(最外层为 0),嵌套执行
// (powershell -Command "…"、cmd /c …、wsl … 等)的递归判定在每段
// 上叠加,结论一并参与取最严重。
func evaluateWindowsAt(shell models.ShellType, command string, rules *RuleSet, depth int) Decision {
	ev := &winEvidence{rawCommand: command}
	for _, seg := range splitWindowsSegments(command) {
		ev.segments = append(ev.segments, parseWinSegment(seg))
	}
	var worst Decision
	for i := range ev.segments {
		worst = merge(worst, applyWindowsRules(ev, &ev.segments[i], shell, rules))
		worst = merge(worst, nestedWindowsDecision(&ev.segments[i], depth, rules))
	}
	return worst
}

// splitWindowsSegments 按 `;`、`&&`、`|`、`&` 与换行把命令文本拆成
// 一段段简单命令。引号内的分隔符不算;`&&`、`||`、`2>&1` 这类双字符
// 操作符会被切成两刀,中间的空段直接丢弃,两侧照常成段。
func splitWindowsSegments(command string) []string {
	var segs []string
	var cur strings.Builder
	var quote rune
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			segs = append(segs, s)
		}
		cur.Reset()
	}
	for _, r := range command {
		switch {
		case quote != 0:
			cur.WriteRune(r)
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
			cur.WriteRune(r)
		case r == ';' || r == '|' || r == '&' || r == '\n' || r == '\r':
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return segs
}

// parseWinSegment 把一段文本拆成「命令名 + 参数」:空白分词、引号剥掉、
// 动态成分标记;第一个词按命令名规范化(小写、去 @ 前缀、取基础名、
// 剥可执行扩展名)。
func parseWinSegment(seg string) winSegment {
	var tokens []winToken
	var cur strings.Builder
	var quote rune
	flushTok := func() {
		if cur.Len() == 0 {
			return
		}
		text := cur.String()
		tokens = append(tokens, winToken{text: text, dynamic: isDynamicWinToken(text)})
		cur.Reset()
	}
	for _, r := range seg {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			cur.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
		case r == ' ' || r == '\t':
			flushTok()
		default:
			cur.WriteRune(r)
		}
	}
	flushTok()
	if len(tokens) == 0 {
		return winSegment{}
	}
	if tokens[0].dynamic {
		// 命令名本身是变量($cmd、%CMD%)时无从按名匹配,参数照常保留。
		return winSegment{args: tokens[1:]}
	}
	return winSegment{name: normalizeWinCommandName(tokens[0].text), args: tokens[1:]}
}

// isDynamicWinToken 报告一个词是否含执行前无法确定的动态成分:
// PowerShell 的 $ 变量(含 $env: 与 $(…))或 cmd 的 % 变量%。
func isDynamicWinToken(text string) bool {
	return strings.ContainsAny(text, "$%")
}

// dangerousWinCommandNames 是动态执行兑底探测的危险命令名集合:
// 删除家族、格式化/分区/擦除家族与锁门户外家族的命令名(含别名)。
// 只在文本里出现动态执行写法时才用它做兑底,不影响正常分段判定。
var dangerousWinCommandNames = map[string]bool{
	"remove-item": true, "rm": true, "del": true, "erase": true,
	"rd": true, "rmdir": true, "ri": true,
	"format": true, "format-volume": true,
	"diskpart": true, "clear-disk": true, "initialize-disk": true,
	"cipher": true, "vssadmin": true,
	"netsh": true, "set-netfirewallprofile": true,
	"stop-service": true, "spsv": true, "disable-netadapter": true,
}

// containsDangerousWinName 报告文本里是否出现任何危险命令名。按词比对
// (分隔符切开、命令名规范化),避免 rm、ri 这类短名在普通单词里误中。
func containsDangerousWinName(text string) bool {
	isSep := func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\r', ';', '|', '&', '"', '\'', '(', ')', ',',
			'/', '\\', '<', '>', '[', ']', '{', '}', '+', '=', '*', '?', '!':
			return true
		}
		return false
	}
	for _, w := range strings.FieldsFunc(text, isSep) {
		if dangerousWinCommandNames[normalizeWinCommandName(w)] {
			return true
		}
	}
	return false
}

// normalizeWinCommandName 把命令名规范化到可比较形态:小写、剥 cmd 的
// @ 回显前缀、取基础名(正反斜杠都认,兼容 C:\Windows\System32\format.com
// 这类全路径写法)、剥常见可执行扩展名。
func normalizeWinCommandName(text string) string {
	name := strings.ToLower(text)
	name = strings.TrimPrefix(name, "@")
	if i := strings.LastIndexAny(name, `\/`); i >= 0 {
		name = name[i+1:]
	}
	for _, ext := range []string{".exe", ".com", ".bat", ".cmd"} {
		name = strings.TrimSuffix(name, ext)
	}
	return name
}
