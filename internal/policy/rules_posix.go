package policy

import "strings"

// hasFlagArg 报告简单命令的参数里是否出现给定标志之一。
func hasFlagArg(c *posixCall, flags ...string) bool {
	for _, a := range c.args {
		if a.dynamic {
			continue
		}
		for _, f := range flags {
			if a.text == f {
				return true
			}
		}
	}
	return false
}

// hasNonListingOption 报告是否出现任一不在只读集合里的选项参数
// (--info=1 这类自带取值的长选项按去值后的名字比较)。
func hasNonListingOption(c *posixCall, listing ...string) bool {
	for _, a := range c.args {
		if a.dynamic || !strings.HasPrefix(a.text, "-") {
			continue
		}
		opt := a.text
		if i := strings.IndexByte(opt, '='); i >= 0 {
			opt = opt[:i]
		}
		listed := false
		for _, l := range listing {
			if opt == l {
				listed = true
				break
			}
		}
		if !listed {
			return true
		}
	}
	return false
}

// partedDestructiveSubcommands 是 parted 会改写分区表的子命令;
// print 只是打印。
var partedDestructiveSubcommands = map[string]bool{
	"mklabel": true, "mktable": true, "mkpart": true, "mkparts": true,
	"rm": true, "resizepart": true, "move": true, "resize": true,
	"set": true, "toggle": true, "name": true,
	"disk_set": true, "disk_toggle": true, "journal": true,
}

// rmInvocation 把一条 rm 调用拆成「是否递归」与「目标参数序列」。
// -- 之后一律是目标;目标可能是含变量的动态参数,由调用方决定怎么对待。
func rmInvocation(c *posixCall) (recursive bool, targets []posixArg) {
	targetsOnly := false
	for _, a := range c.args {
		if a.dynamic {
			if targetsOnly {
				targets = append(targets, a)
			}
			continue
		}
		if targetsOnly {
			targets = append(targets, a)
			continue
		}
		if a.text == "--" {
			targetsOnly = true
			continue
		}
		if strings.HasPrefix(a.text, "-") && len(a.text) > 1 {
			if a.text == "--recursive" {
				recursive = true
				continue
			}
			if strings.HasPrefix(a.text, "--") {
				continue
			}
			// 短选项簇,如 -rf、-fr;含 r 或 R 即递归。
			if strings.ContainsAny(a.text, "rR") {
				recursive = true
			}
			continue
		}
		targets = append(targets, a)
	}
	return recursive, targets
}

// posixRule 是一条 POSIX shell 的默认规则(拦截或需确认):稳定编号、
// 处置、一行风险说明、更安全的替代做法,以及「是否命中」的判定闭包。
// 编号一经发布不可更改(owner 按编号停用,审计按编号追查);
// 自定义规则(命令策略后续的 GitHub Issue)会以结构化字段编译成同样的形态,不改 Evaluate 接口。
type posixRule struct {
	id          string
	disposition Disposition
	explanation string
	alternative string
	matches     func(ev *posixEvidence, call *posixCall) bool
}

// defaultPOSIXRules 是内置的 POSIX 默认规则,按声明顺序尝试,
// 每条简单命令返回第一条命中的结论。
var defaultPOSIXRules = []posixRule{
	{
		id:          "POSIX-FORMAT",
		disposition: DispositionBlock,
		explanation: "formatting erases everything on the target disk or volume; the data cannot be recovered afterwards",
		alternative: "let the owner format disks in person; the agent should not run formatting commands",
		matches: func(_ *posixEvidence, c *posixCall) bool {
			switch {
			case c.name == "mkfs" || strings.HasPrefix(c.name, "mkfs."):
				return true
			case c.name == "wipefs":
				return true
			case c.name == "diskutil":
				sub, ok := c.firstPositional()
				return ok && (sub == "eraseDisk" || sub == "eraseVolume")
			default:
				return false
			}
		},
	},
	{
		id:          "POSIX-RAW-DISK-WRITE",
		disposition: DispositionBlock,
		explanation: "writing straight to a disk device under /dev/ bypasses the filesystem and can corrupt the whole disk",
		alternative: "write to a path inside a filesystem; if the disk truly must be overwritten, let the owner do it in person",
		matches: func(_ *posixEvidence, c *posixCall) bool {
			switch c.name {
			case "dd":
				// dd 的 of= 参数就是写入目标。
				for _, a := range c.args {
					if target, ok := strings.CutPrefix(a.text, "of="); ok && isDiskDevicePath(target) {
						return true
					}
				}
			case "shred", "tee":
				// shred 与 tee 的非选项参数就是写入目标;tee /dev/sda 与
				// 重定向写盘等价。
				for _, a := range c.args {
					if !a.dynamic && !strings.HasPrefix(a.text, "-") && isDiskDevicePath(a.text) {
						return true
					}
				}
			}
			// 输出重定向直指磁盘设备时,无论命令是什么都拦。
			for _, r := range c.redirects {
				if r.write && !r.targetDyn && isDiskDevicePath(r.target) {
					return true
				}
			}
			return false
		},
	},
	{
		id:          "POSIX-PARTITION",
		disposition: DispositionBlock,
		explanation: "rewriting the partition table makes the existing partitions and all data on them unreachable",
		alternative: "if partitions truly must change, let the owner do it in person",
		matches: func(_ *posixEvidence, c *posixCall) bool {
			switch c.name {
			case "fdisk", "sfdisk", "cfdisk":
				// 不带列表标志时,给设备就是打开写入编辑。
				return !hasFlagArg(c, "-l", "--list")
			case "gdisk", "cgdisk":
				// 交互式分区编辑器,除了只读形态都以拦截对待。
				return !hasFlagArg(c, "-p", "--print", "-i", "--info", "-v", "--verify")
			case "sgdisk":
				// 无操作参数时什么都不改;出现任一非只读选项即拦。
				return hasNonListingOption(c, "-p", "--print", "-i", "--info", "-v", "--verify")
			case "parted":
				// parted 的第一个位置参数是设备,子命令在后面,逐个扫。
				for _, a := range c.args {
					if !a.dynamic && !strings.HasPrefix(a.text, "-") && partedDestructiveSubcommands[a.text] {
						return true
					}
				}
				return false
			case "diskutil":
				sub, ok := c.firstPositional()
				return ok && sub == "partitionDisk"
			default:
				return false
			}
		},
	},
	{
		id:          "POSIX-CRITICAL-DELETE",
		disposition: DispositionBlock,
		explanation: "recursively deleting the root directory or a system-critical path leaves the system unbootable and the data unrecoverable",
		alternative: "check the path character by character before deleting; cleaning system directories is the owner's call, not the agent's",
		matches: func(_ *posixEvidence, c *posixCall) bool {
			if c.name != "rm" {
				return false
			}
			recursive, targets := rmInvocation(c)
			if !recursive {
				return false
			}
			for _, t := range targets {
				if !t.dynamic && criticalPOSIXPaths[normalizePOSIXPath(t.text)] {
					return true
				}
			}
			return false
		},
	},
	{
		id:          "POSIX-RECURSIVE-ROOT-PERM",
		disposition: DispositionBlock,
		explanation: "recursively changing permissions or ownership on the root directory breaks the whole system and is very hard to undo by hand",
		alternative: "apply recursive changes only to the specific directory that needs them, and re-check the path character by character",
		matches: func(_ *posixEvidence, c *posixCall) bool {
			if c.name != "chmod" && c.name != "chown" {
				return false
			}
			recursive := false
			for _, a := range c.args {
				if a.dynamic {
					continue
				}
				if a.text == "--recursive" {
					recursive = true
					continue
				}
				// chmod 的 -r 是去掉读位,只有大写 R 才是递归。
				if strings.HasPrefix(a.text, "-") && len(a.text) > 1 && !strings.HasPrefix(a.text, "--") && strings.ContainsRune(a.text, 'R') {
					recursive = true
				}
			}
			if !recursive {
				return false
			}
			for _, a := range c.args {
				if !a.dynamic && !strings.HasPrefix(a.text, "-") && normalizePOSIXPath(a.text) == "/" {
					return true
				}
			}
			return false
		},
	},
	{
		id:          "POSIX-FORK-BOMB",
		disposition: DispositionBlock,
		explanation: "a fork bomb keeps copying itself until the server runs out of processes and stops responding",
		alternative: "if this was meant as a stress test, use a bounded tool with an explicit concurrency limit instead",
		matches: func(ev *posixEvidence, _ *posixCall) bool {
			return ev.forkBomb
		},
	},
}
