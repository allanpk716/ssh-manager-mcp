package policy

import "strings"

// windowsRule 是一条 Windows 默认规则(PowerShell 与 cmd 共用一套):
// 稳定编号、处置、一行风险说明、更安全的替代做法,以及「是否命中」的
// 判定闭包。编号一经发布不可更改(owner 按编号停用,审计按编号追查)。
type windowsRule struct {
	id          string
	disposition Disposition
	explanation string
	alternative string
	matches     func(ev *winEvidence, seg *winSegment) bool
}

// applyWindowsRules 按声明顺序过一遍 Windows 默认规则表,返回第一条
// 命中的结论;没有命中则放行。
func applyWindowsRules(ev *winEvidence, seg *winSegment, rules *RuleSet) Decision {
	for _, r := range defaultWindowsRules {
		if rules.isDisabled(r.id) {
			continue
		}
		if r.matches(ev, seg) {
			return Decision{
				Disposition: r.disposition,
				RuleID:      r.id,
				Explanation: r.explanation,
				Alternative: r.alternative,
			}
		}
	}
	return Decision{Disposition: DispositionAllow}
}

// deleteCommandNames 是删除类命令名的规范化集合:PowerShell 的
// Remove-Item 与它的全部内置别名(rm、del、erase、rd、rmdir、ri),
// 以及 cmd 的 del、erase、rd、rmdir——两类 shell 共用一张别名表,
// 匹配不区分大小写(名字已在分段时小写)。
var deleteCommandNames = map[string]bool{
	"remove-item": true,
	"rm":          true,
	"del":         true, "erase": true,
	"rd": true, "rmdir": true, "ri": true,
}

// winDeleteInvocation 把一条删除类调用拆成「是否递归」与「目标序列」。
// PowerShell 的 -Recurse(含唯一前缀缩写 -r)与 cmd 的 /s 都是递归
// 标记;-Path:C:\x 这类冒号带值写法把取值计入目标;斜杠开头的长词
// (/Windows 这种根定位路径)不当开关,当作目标。
func winDeleteInvocation(s *winSegment) (recursive bool, targets []winToken) {
	for _, a := range s.args {
		if a.dynamic {
			targets = append(targets, a)
			continue
		}
		low := strings.ToLower(a.text)
		if strings.HasPrefix(low, "-") {
			// 选项参数:冒号带值写法先把取值提出来。
			if opt, value, ok := strings.Cut(low, ":"); ok && value != "" {
				flag := strings.TrimPrefix(opt, "-")
				if strings.HasPrefix("path", flag) || strings.HasPrefix("literalpath", flag) {
					targets = append(targets, winToken{text: value})
				}
				continue
			}
			flag := strings.TrimPrefix(low, "-")
			// Remove-Item 的 -Recurse 没有同名冲突参数,-r 起的唯一前缀
			// 缩写都算递归。
			if flag != "" && strings.HasPrefix("recurse", flag) {
				recursive = true
			}
			continue
		}
		if strings.HasPrefix(low, "/") {
			// cmd 的短开关(/s、/q、/a:h 这类斜杠后不超过两字母的)
			// 当选项;含 s 即递归删除,其余长词当路径目标。
			flag := low[1:]
			if name, _, ok := strings.Cut(flag, ":"); ok {
				flag = name
			}
			if flag != "" && len(flag) <= 2 {
				if strings.ContainsRune(flag, 's') {
					recursive = true
				}
				continue
			}
		}
		targets = append(targets, a)
	}
	return recursive, targets
}

// defaultWindowsRules 是 PowerShell 与 cmd 共用的内置默认规则,按声明
// 顺序尝试,每段简单命令返回第一条命中的结论。同一段里可能同时命中
// 拦截档与需确认档的目标时,把拦截档规则排在前面,保证最严重者优先。
var defaultWindowsRules = []windowsRule{
	{
		id:          "WIN-FORMAT",
		disposition: DispositionBlock,
		explanation: "formatting erases everything on the target disk or volume; the data cannot be recovered afterwards",
		alternative: "let the owner format disks in person; the agent should not run formatting commands",
		matches: func(_ *winEvidence, s *winSegment) bool {
			// cmd 的 format(format.com)与 PowerShell 的 Format-Volume;
			// Format-Table 这类格式化输出的命令名字不同,不会命中。
			return s.name == "format" || s.name == "format-volume"
		},
	},
	{
		id:          "WIN-PARTITION",
		disposition: DispositionBlock,
		explanation: "diskpart, Clear-Disk and Initialize-Disk wipe the disk contents or rewrite its partition table; the existing partitions and all data on them become unreachable",
		alternative: "if disk layout truly must change, let the owner do it in person",
		matches: func(_ *winEvidence, s *winSegment) bool {
			// diskpart 是交互式分区编辑器(脚本模式同样破坏);Clear-Disk 清空
			// 整盘,Initialize-Disk 抹掉分区表重新初始化,都没有只读形态可列。
			switch s.name {
			case "diskpart", "clear-disk", "initialize-disk":
				return true
			}
			return false
		},
	},
	{
		id:          "WIN-CRITICAL-DELETE",
		disposition: DispositionBlock,
		explanation: "recursively deleting the drive root or a system-critical path (C:\\, C:\\Windows, C:\\Program Files, C:\\Users) leaves the system unbootable and the data unrecoverable",
		alternative: "check the path character by character before deleting; cleaning system directories is the owner's call, not the agent's",
		matches: func(_ *winEvidence, s *winSegment) bool {
			if !deleteCommandNames[s.name] {
				return false
			}
			recursive, targets := winDeleteInvocation(s)
			if !recursive {
				return false
			}
			for _, t := range targets {
				if !t.dynamic && criticalWindowsPaths[normalizeWindowsPath(t.text)] {
					return true
				}
			}
			return false
		},
	},
	{
		id:          "WIN-DELETE-TARGET-UNKNOWN",
		disposition: DispositionConfirmRequired,
		explanation: "this recursive delete targets a variable or the current directory, so what it will erase cannot be known in advance; an empty variable deletes the wrong place",
		alternative: "write the exact path literally, or list the directory first and ask the owner to confirm before deleting recursively",
		matches: func(_ *winEvidence, s *winSegment) bool {
			if !deleteCommandNames[s.name] {
				return false
			}
			recursive, targets := winDeleteInvocation(s)
			if !recursive {
				return false
			}
			for _, t := range targets {
				if t.dynamic {
					return true
				}
				// 裸 . 或 .. 指向哪里取决于当前目录,同样无法预判;
				// 规范化后为空即是这类写法(./、.\\、.. 等变体同归)。
				if normalizeWindowsPath(t.text) == "" {
					return true
				}
			}
			return false
		},
	},
	{
		id:          "WIN-LOCKOUT",
		disposition: DispositionConfirmRequired,
		explanation: "changing the firewall, stopping the SSH service or disabling a network adapter can cut the connection to the server, after which nobody can reach it remotely",
		alternative: "let the owner run it in person, or ask the owner to confirm this specific change first",
		matches: func(_ *winEvidence, s *winSegment) bool {
			switch s.name {
			case "netsh":
				// netsh advfirewall 子树改防火墙配置;show 是只读查询。
				var adv, show bool
				for _, a := range s.args {
					if a.dynamic {
						continue
					}
					switch strings.ToLower(a.text) {
					case "advfirewall":
						adv = true
					case "show":
						show = true
					}
				}
				return adv && !show
			case "set-netfirewallprofile":
				return true
			case "stop-service", "spsv":
				// 只有停 sshd 会把远程通道本身停掉;spsv 是 Stop-Service
				// 的内置别名,别名表同样适用。
				for _, a := range s.args {
					if a.dynamic {
						continue
					}
					if strings.TrimPrefix(strings.ToLower(a.text), "-name:") == "sshd" {
						return true
					}
				}
				return false
			case "net":
				// cmd 形态:net stop sshd。
				var stop, sshd bool
				for _, a := range s.args {
					if a.dynamic {
						continue
					}
					switch strings.ToLower(a.text) {
					case "stop":
						stop = true
					case "sshd":
						sshd = true
					}
				}
				return stop && sshd
			case "disable-netadapter":
				return true
			}
			return false
		},
	},
	{
		id:          "WIN-DATA-WIPE",
		disposition: DispositionBlock,
		explanation: "cipher /w overwrites all free space on the volume and vssadmin delete shadows removes the restore points; deleted data and previous file states cannot be recovered afterwards",
		alternative: "let the owner wipe free space or manage restore points in person",
		matches: func(_ *winEvidence, s *winSegment) bool {
			switch s.name {
			case "cipher":
				// /w 擦除空闲空间;/w:C 与 /w: 带空格都是它的写法;
				// /e、/d 加解密单个文件是日常用法,不命中。
				for _, a := range s.args {
					if a.dynamic {
						continue
					}
					low := strings.ToLower(a.text)
					if low == "/w" || strings.HasPrefix(low, "/w:") {
						return true
					}
				}
			case "vssadmin":
				// delete shadows 删除系统还原点;list 子命令只读。
				var del, shadows bool
				for _, a := range s.args {
					if a.dynamic {
						continue
					}
					switch strings.ToLower(a.text) {
					case "delete":
						del = true
					case "shadows":
						shadows = true
					}
				}
				return del && shadows
			}
			return false
		},
	},
	{
		id:          "WIN-DYNAMIC-EXEC",
		disposition: DispositionConfirmRequired,
		explanation: "the command builds code at runtime (iex / Invoke-Expression / -EncodedCommand) and the text mentions a destructive command name, so what will actually run cannot be judged in advance",
		alternative: "write the commands literally instead of building them at runtime, or ask the owner to confirm the exact operation first",
		matches: func(ev *winEvidence, s *winSegment) bool {
			// 动态执行写法出现且全文含危险命令名才触发;没有危险名的
			// 动态执行照常放行(与嵌套执行票同一取舍)。
			if !containsDangerousWinName(ev.rawCommand) {
				return false
			}
			if s.name == "iex" || s.name == "invoke-expression" {
				return true
			}
			// powershell -EncodedCommand 这类携带编码命令的调用。
			for _, a := range s.args {
				if !a.dynamic && strings.Contains(strings.ToLower(a.text), "-encodedcommand") {
					return true
				}
			}
			return false
		},
	},
}
