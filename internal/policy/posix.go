package policy

import (
	"ssh-manager-mcp/internal/models"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// posixArg 是简单命令的一个参数:能逐字确定时 text 是它的文字;
// 含变量、命令替换、通配起点不确定的展开等动态成分时 dynamic 为真,
// text 为空、raw 保留它在原命令文本里的源码片段(含引号与变量写法,
// 供嵌套执行的危险命令名兜底探测)——这类参数在执行前无法知道会
// 指向哪里,交给具体规则决定如何对待。
type posixArg struct {
	text    string
	dynamic bool
	raw     string
}

// posixRedirect 是附在一条简单命令上的一个重定向:操作符种类(只区分
// 「写入类」与「读取类」)与目标词。目标词同样只保留逐字可确定的部分。
type posixRedirect struct {
	write     bool
	target    string
	targetDyn bool
}

// posixCall 是从命令文本拆出的一条「简单命令」:剥掉 sudo、env、nohup 等
// 前缀后的命令名、参数,以及附在它身上的重定向。命令名本身含动态成分
// (如 $CMD)时 name 为空——没有任何按命令名匹配的规则能命中它。
type posixCall struct {
	name      string
	args      []posixArg
	redirects []posixRedirect
}

// firstPositional 返回第一个不以 - 开头的参数的文字(子命令名约定),
// 以及它是否逐字可确定。
func (c *posixCall) firstPositional() (string, bool) {
	for _, a := range c.args {
		if a.dynamic {
			return "", false
		}
		if !strings.HasPrefix(a.text, "-") {
			return a.text, true
		}
	}
	return "", true
}

// evaluatePOSIXAt 把命令文本按 POSIX shell 语法拆解并逐条应用生效
// 规则集(默认规则的结论与自定义规则的结论取更严重的一档),返回
// 最严重的结论。depth 是当前文本的嵌套层数(最外层为 0);每条简单
// 命令除规则结论外,还会做嵌套执行识别(bash -c、wsl 等把一段命令
// 文本交给另一个 shell 执行的写法),内层结论一并参与取最严重。
func evaluatePOSIXAt(command string, rules *RuleSet, depth int) Decision {
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		// 拆解失败:文本里出现危险命令名的,判需确认交 owner 把关;
		// 其余放行(与 GitHub Issue #8「命令策略」的实现决策一致)。
		if containsDangerousName(command) {
			return Decision{
				Disposition: DispositionConfirmRequired,
				Explanation: "the command could not be parsed as shell syntax and it mentions a destructive command name; ask the owner to review it",
			}
		}
		return Decision{Disposition: DispositionAllow}
	}
	ev := collectEvidence(file, command)
	var worst Decision
	for i := range ev.calls {
		worst = merge(worst, applyRules(ev, &ev.calls[i], command, rules))
		worst = merge(worst, nestedPOSIXDecision(&ev.calls[i], depth, rules))
	}
	return worst
}

// posixEvidence 是一次判定能看到的全部事实:拆出的简单命令序列,
// 以及函数定义里是否出现把自身接进管道的自我复制形态(fork 炸弹)。
type posixEvidence struct {
	calls    []posixCall
	forkBomb bool
}

// applyRules 先在默认规则表里取第一条命中(拦截在前、需确认在后,
// 语义上拦截更严重、优先给出),再在适用的自定义规则里取第一条命中,
// 两个结论取更严重的一档返回——owner 加的自定义规则能把默认的需确认
// 收紧成拦截,但不能把内置拦截放松(默认规则只能按编号停用,不能被
// 自定义改写)。两边都没命中则放行。command 是整条命令原文,自定义
// 规则的正则条件对它匹配。
func applyRules(ev *posixEvidence, call *posixCall, command string, rules *RuleSet) Decision {
	def := firstDefaultMatch(ev, call, rules)
	for _, cr := range rules.customsFor(models.ShellPOSIX) {
		if matchCustomPOSIX(cr, command, call) {
			return merge(def, Decision{
				Disposition: cr.Disposition,
				RuleID:      cr.ID,
				Explanation: cr.Explanation,
				Alternative: cr.Alternative,
			})
		}
	}
	return def
}

// firstDefaultMatch 按声明顺序过一遍默认规则表(拦截在前、需确认在
// 后),返回第一条命中的结论;没有命中则放行。
func firstDefaultMatch(ev *posixEvidence, call *posixCall, rules *RuleSet) Decision {
	for _, list := range [][]posixRule{defaultPOSIXRules, defaultPOSIXConfirmRules} {
		for _, r := range list {
			if rules.isDisabled(r.id) {
				continue
			}
			if r.matches(ev, call) {
				return Decision{
					Disposition: r.disposition,
					RuleID:      r.id,
					Explanation: r.explanation,
					Alternative: r.alternative,
				}
			}
		}
	}
	return Decision{Disposition: DispositionAllow}
}

// collectEvidence 遍历语法树,收集每条语句里的简单命令(含重定向),
// 并识别 fork 炸弹形态的函数定义。command 是整条命令原文,动态参数
// 的源码片段从它身上切取。
func collectEvidence(file *syntax.File, command string) *posixEvidence {
	ev := &posixEvidence{}
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.Stmt:
			ev.calls = append(ev.calls, stmtCall(n, command)...)
		case *syntax.FuncDecl:
			if funcDeclIsForkBomb(n) {
				ev.forkBomb = true
			}
		}
		return true
	})
	return ev
}

// stmtCall 从一条语句提取简单命令事实:命令体是简单命令时取命令名、
// 参数与重定向;命令体为空(纯重定向,如 `> /dev/sda`)时只带重定向;
// 其余形态(子 shell、if、for 等)由语法树遍历继续下钻,语句自身的
// 重定向单独成一条事实。
func stmtCall(stmt *syntax.Stmt, command string) []posixCall {
	var call posixCall
	if expr, ok := stmt.Cmd.(*syntax.CallExpr); ok {
		call = callExprToCall(expr, command)
	}
	for _, r := range stmt.Redirs {
		if red, ok := redirectFact(r); ok {
			call.redirects = append(call.redirects, red)
		}
	}
	if call.name == "" && call.args == nil && len(call.redirects) == 0 {
		return nil
	}
	return []posixCall{call}
}

// callExprToCall 把语法树的简单命令转成事实:第一个词是命令名(取基础名,
// 兼容 /sbin/mkfs.ext4 这类全路径写法),其余词是参数;然后剥掉
// sudo、env、nohup 等前缀。
func callExprToCall(expr *syntax.CallExpr, command string) posixCall {
	var call posixCall
	words := expr.Args // 命令前的环境变量赋值(expr.Assigns)不属于命令名与参数
	if len(words) > 0 {
		name, ok := wordLiteral(words[0])
		if ok {
			call.name = commandBaseName(name)
		}
		words = words[1:]
	}
	for _, w := range words {
		if text, ok := wordLiteral(w); ok {
			call.args = append(call.args, posixArg{text: text})
		} else {
			call.args = append(call.args, posixArg{dynamic: true, raw: wordSourceText(w, command)})
		}
	}
	return stripPrefix(call)
}

// redirectFact 把一个重定向转成事实;只保留目标可确定的重定向,
// 读取类重定向不参与「直接写磁盘设备」的判定。
func redirectFact(r *syntax.Redirect) (posixRedirect, bool) {
	var fact posixRedirect
	switch r.Op {
	case syntax.RdrOut, syntax.AppOut, syntax.ClbOut, syntax.RdrInOut, syntax.RdrAll, syntax.AppAll, syntax.DplOut:
		fact.write = true
	default:
		// <、<&、heredoc 等读取类重定向不写入目标。
		return fact, false
	}
	if r.Word != nil {
		if text, ok := wordLiteral(r.Word); ok {
			fact.target = text
			return fact, true
		}
		fact.targetDyn = true
		return fact, true
	}
	return fact, false
}

// wordLiteral 返回一个词逐字拼接后的文字;词里含变量、命令替换等
// 动态成分时第二个返回值为 false。
func wordLiteral(w *syntax.Word) (string, bool) {
	var b strings.Builder
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, q := range p.Parts {
				if lit, ok := q.(*syntax.Lit); ok {
					b.WriteString(unescapeDblQuoted(lit.Value))
				} else {
					return "", false
				}
			}
		default:
			return "", false
		}
	}
	return b.String(), true
}

// unescapeDblQuoted 消化双引号内字面量里的转义:POSIX 规定双引号内
// 反斜杠只对美元符、反引号、双引号、反斜杠、换行五个字符有转义义
// (语法树里的字面量节点原样保留了反斜杠),其余场合反斜杠原样保留。
// 剥掉转义后的文字才是双引号词的真正字面值——嵌套执行把内层文本交给
// 下一层拆解时必须用剥过转义的形态,否则内层的引号还带着上一层的
// 转义反斜杠,在下一层拆解时对不上号。
func unescapeDblQuoted(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case '$', '`', '"', '\\':
				b.WriteByte(s[i+1])
				i++
				continue
			case '\n':
				i++ // 行尾续行反斜杠,双引号内同样去掉
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// commandBaseName 取命令名的基础名,让 /usr/sbin/mkfs.ext4 与
// mkfs.ext4 按同一个名字匹配。
func commandBaseName(name string) string {
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		return name[i+1:]
	}
	return name
}

// wrapperCommands 是会被剥掉的前缀包装命令及其「带值选项」表:这类
// 包装命令自身的选项与取值不属于真正的命令,例如 sudo -u root rm、
// env FOO=1 rm。键 "*" 表示该包装命令的所有选项都带值。
var wrapperCommands = map[string]map[string]bool{
	"sudo":    {"-u": true, "-g": true, "-p": true, "-t": true, "-C": true, "-D": true, "-r": true, "-T": true, "-U": true, "--user": true, "--group": true, "--prompt": true, "--type": true, "--close-from": true, "--chdir": true, "--roles": true, "--other-user": true},
	"env":     {"-u": true, "--unset": true},
	"nice":    {"-n": true, "--adjustment": true},
	"stdbuf":  {"*": true},
	"timeout": {"-k": true, "-s": true, "--signal": true, "--kill-after": true},
	"nohup":   {},
	"setsid":  {},
	// 同属前缀包装:command / exec / doas 自身的选项不是真正的命令
	// (exec -a 名字、doas -u 用户),busybox 后接的第一个位置参数才是
	// 要跑的小程序。command 的 -v / -V 查找形态在 stripPrefix 里另行
	// 按放行对待。
	"command": {},
	"exec":    {"-a": true}, // -l、-c 不带值
	"doas":    {"-u": true}, // -n、-s 不带值
	"busybox": {},
}

// stripPrefix 剥掉 sudo、env、nohup 等前缀包装命令及其自身的选项,
// 返回真正的命令名与参数。sudo 之类的开关不改变危险与否,剥掉后按
// 内层命令判定。
func stripPrefix(call posixCall) posixCall {
	for {
		_, isWrapper := wrapperCommands[call.name]
		if !isWrapper {
			return call
		}
		valueOpts := wrapperCommands[call.name]
		args := call.args
		// 剥掉包装命令自身的选项;「带值选项」连同它的取值一起剥。
		var opts []string
		for len(args) > 0 && !args[0].dynamic && strings.HasPrefix(args[0].text, "-") {
			opt := args[0].text
			opts = append(opts, opt)
			args = args[1:]
			if strings.ContainsAny(opt, "=") {
				continue // --adjustment=5 这类自带取值的写法
			}
			if valueOpts["*"] || valueOpts[opt] {
				if len(args) > 0 {
					args = args[1:]
				}
			}
		}
		// env 特有:剥掉命令前的 NAME=value 环境变量赋值。
		// command 自有的 -v / -V 是按名查找(打印可执行文件路径),不执行
		// 任何命令——整条简单命令按无命令名对待,判放行。
		if call.name == "command" {
			for _, opt := range opts {
				if opt == "-v" || opt == "-V" {
					return posixCall{}
				}
			}
		}
		if call.name == "env" {
			for len(args) > 0 && !args[0].dynamic && strings.Contains(args[0].text, "=") {
				args = args[1:]
			}
		}
		// timeout 特有:取值时长是不带 - 的第一个位置参数。
		if call.name == "timeout" && len(args) > 0 && !args[0].dynamic && !strings.HasPrefix(args[0].text, "-") && !strings.Contains(args[0].text, "=") {
			args = args[1:]
		}
		if len(args) == 0 {
			return posixCall{}
		}
		if args[0].dynamic {
			return posixCall{args: args[1:]}
		}
		call = posixCall{name: commandBaseName(args[0].text), args: args[1:]}
	}
}

// funcDeclIsForkBomb 判断一个函数定义是不是 fork 炸弹形态:函数体里
// 出现把函数自身接进管道的自我复制(经典的 `:(){ :|:& };:` 及其改名
// 变体),管道两侧至少各有一次对自身的调用,才会指数级复制进程。
func funcDeclIsForkBomb(decl *syntax.FuncDecl) bool {
	found := false
	syntax.Walk(decl.Body, func(node syntax.Node) bool {
		bin, ok := node.(*syntax.BinaryCmd)
		if !ok || bin.Op != syntax.Pipe {
			return true
		}
		selfCalls := 0
		for _, side := range []syntax.Node{bin.X, bin.Y} {
			if pipelineSideCalls(side, decl.Name.Value) {
				selfCalls++
			}
		}
		if selfCalls >= 2 {
			found = true
			return false
		}
		return true
	})
	return found
}

// pipelineSideCalls 报告管道的一侧(可能是链式管道的一段)是否直接
// 调用了给定函数名。
func pipelineSideCalls(side syntax.Node, name string) bool {
	switch s := side.(type) {
	case *syntax.Stmt:
		if call, ok := s.Cmd.(*syntax.CallExpr); ok && len(call.Args) > 0 {
			if text, ok := wordLiteral(call.Args[0]); ok && text == name {
				return true
			}
		}
		return false
	case *syntax.BinaryCmd:
		return pipelineSideCalls(s.X, name) || pipelineSideCalls(s.Y, name)
	default:
		return false
	}
}

// dangerousNameMarkers 是拆解失败时的兜底探测:文本里出现这些危险
// 命令名时判需确认,而不是放行。
var dangerousNameMarkers = []string{
	"mkfs", "wipefs", "shred", "dd ", "dd\t", "> /dev/", "blkdiscard",
	"fdisk", "sfdisk", "cfdisk", "gdisk", "cgdisk", "sgdisk", "parted",
	"diskutil", "rm ", "rm\t", "chmod ", "chown ", "mkswap", "pvremove",
	"vgremove", "zfs destroy", "lvremove",
	// 需确认家族的命令名(同上,只在拆解失败时作兜底探测)。
	"iptables -F", "iptables --flush", "flush ruleset", "ufw disable",
	"ufw reset", "sshd_config", "zpool destroy", "docker volume rm",
	"docker system prune", "systemctl stop ssh", "-setremotelogin",
}

// containsDangerousName 报告文本里是否出现任何危险命令名标记。
func containsDangerousName(command string) bool {
	for _, m := range dangerousNameMarkers {
		if strings.Contains(command, m) {
			return true
		}
	}
	return false
}
