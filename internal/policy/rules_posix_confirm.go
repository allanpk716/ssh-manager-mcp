package policy

import "strings"

// POSIX 需确认默认规则(ticket #11 / ADR 0007):与拦截档不同,需确认
// 只兜三类严重后果——会把自己锁在门外、整卷或整池销毁、删除目标在
// 执行前无法确定的递归删除。规则形态与拦截规则共用 posixRule,
// 经 applyRules 在拦截规则之后依序判定。

// positionals 返回简单命令里逐字可确定、且不以 - 开头的位置参数序列
// (子命令名约定同 firstPositional)。
func positionals(c *posixCall) []string {
	var out []string
	for _, a := range c.args {
		if !a.dynamic && !strings.HasPrefix(a.text, "-") {
			out = append(out, a.text)
		}
	}
	return out
}

// isSSHServiceName 报告一个词是不是 sshd 服务的常见名字形态
// (systemctl stop 与 service … stop 两处共用)。
func isSSHServiceName(s string) bool {
	for _, n := range []string{"ssh", "sshd"} {
		for _, suffix := range []string{"", ".service", ".socket"} {
			if s == n+suffix {
				return true
			}
		}
	}
	return false
}

// isSSHDConfigPath 报告一个路径是否指向 sshd 的主配置文件,或其增量
// 配置目录 /etc/ssh/sshd_config.d/ 下的文件——改这两处都会改变 sshd
// 生效的行为。
func isSSHDConfigPath(p string) bool {
	n := normalizePOSIXPath(p)
	return n == "/etc/ssh/sshd_config" || strings.HasPrefix(n, "/etc/ssh/sshd_config.d/")
}

// writesSSHDConfig 报告一条简单命令是否会改写 sshd_config(或其增量
// 配置目录里的文件):输出重定向直指它,或编辑/就地改写/拷贝覆盖/删除它。
// 只认逐字可确定的目标——目标是变量时按「其他命令含变量一律放行」
// 的约定不触发。
func writesSSHDConfig(c *posixCall) bool {
	// 输出重定向直指 sshd_config:无论命令是什么都算修改。
	for _, r := range c.redirects {
		if r.write && !r.targetDyn && isSSHDConfigPath(r.target) {
			return true
		}
	}
	pos := positionals(c)
	anyConfig := func() bool {
		for _, p := range pos {
			if isSSHDConfigPath(p) {
				return true
			}
		}
		return false
	}
	switch c.name {
	case "vi", "vim", "nano", "pico", "ed", "emacs", "joe", "mcedit",
		"tee", "rm", "shred", "truncate":
		// 这些命令的文件参数就是被改写/删除的目标。
		return anyConfig()
	case "sed":
		// sed 只有带 -i 才就地改写;只打印的形态是读。
		return hasFlagArg(c, "-i", "--in-place") && anyConfig()
	case "cp", "mv", "install":
		// 写目标是最后一个位置参数;把 sshd_config 当来源读不算修改。
		return len(pos) > 0 && isSSHDConfigPath(pos[len(pos)-1])
	case "dd":
		for _, a := range c.args {
			if target, ok := strings.CutPrefix(a.text, "of="); ok && !a.dynamic && isSSHDConfigPath(target) {
				return true
			}
		}
	}
	return false
}

// callLocksOutPOSIX 报告一条简单命令是否会把自己锁在门外:清空防火墙
// 规则、停止 sshd、改写 sshd_config、关闭网卡、macOS 关闭远程登录。
// 一旦执行错,agent 和 owner 都可能连不上服务器,只能去现场处理。
func callLocksOutPOSIX(c *posixCall) bool {
	pos := positionals(c)
	switch c.name {
	case "iptables", "ip6tables":
		return hasFlagArg(c, "-F", "--flush")
	case "nft":
		return len(pos) >= 2 && pos[0] == "flush" && pos[1] == "ruleset"
	case "ufw":
		return len(pos) >= 1 && (pos[0] == "disable" || pos[0] == "reset")
	case "systemctl":
		return len(pos) >= 2 && pos[0] == "stop" && isSSHServiceName(pos[1])
	case "service":
		return len(pos) >= 2 && isSSHServiceName(pos[0]) && pos[1] == "stop"
	case "ifconfig":
		for _, p := range pos {
			if p == "down" {
				return true
			}
		}
		return false
	case "ip":
		if len(pos) < 2 || pos[0] != "link" {
			return false
		}
		for _, p := range pos {
			if p == "down" {
				return true
			}
		}
		return false
	case "ifdown":
		return true
	case "systemsetup":
		if !hasFlagArg(c, "-setremotelogin") {
			return false
		}
		for _, p := range pos {
			if p == "off" {
				return true
			}
		}
		return false
	}
	return writesSSHDConfig(c)
}

// destroysWholeVolumePOSIX 报告一条简单命令是否销毁整卷或整池:
// zfs/zpool destroy、lvremove、docker volume rm、docker system prune -a。
// 销毁的是整块存储上的数据,不是单个文件,错了无法回退。
func destroysWholeVolumePOSIX(c *posixCall) bool {
	pos := positionals(c)
	switch c.name {
	case "zfs", "zpool":
		return len(pos) >= 1 && pos[0] == "destroy"
	case "lvremove":
		return true
	case "docker":
		if len(pos) >= 2 && pos[0] == "volume" && (pos[1] == "rm" || pos[1] == "remove") {
			return true
		}
		if len(pos) >= 2 && pos[0] == "system" && pos[1] == "prune" {
			// 只有 -a/--all 的 prune 才连未使用的镜像全部销毁;不带 -a 的
			// 日常清理不算整卷销毁。
			for _, a := range c.args {
				if a.dynamic {
					continue
				}
				if a.text == "--all" || (strings.HasPrefix(a.text, "-a") && !strings.HasPrefix(a.text, "--")) {
					return true
				}
			}
		}
	}
	return false
}

// deletesUnknownTargetPOSIX 报告一条简单命令是否是「删除目标在执行前
// 无法确定的递归删除」:rm 带递归标志,且参数里出现变量、命令替换等
// 动态成分。变量为空时 rm -rf $DIR/ 就是 rm -rf /,这是这一类全部
// 要凳住的那一下。rm 的选项都不带值,动态参数一律是潜在目标;非递归
// 的 rm $f、变量出现在别的命令里,都不触发。
func deletesUnknownTargetPOSIX(c *posixCall) bool {
	if c.name != "rm" {
		return false
	}
	recursive, _ := rmInvocation(c)
	if !recursive {
		return false
	}
	for _, a := range c.args {
		if a.dynamic {
			return true
		}
	}
	return false
}

// defaultPOSIXConfirmRules 是内置的 POSIX 需确认默认规则,按声明顺序
// 尝试(在拦截规则之后);编号一经发布不可更改。
var defaultPOSIXConfirmRules = []posixRule{
	{
		id:          "POSIX-LOCKOUT",
		disposition: DispositionConfirmRequired,
		explanation: "this can lock everyone out of the server — flushing firewall rules, stopping sshd, editing sshd_config, taking a network interface down or turning off remote login can cut off every way back in, including the owner's",
		alternative: "agree on it with the owner first and keep a second way in (console access or one surviving rule) while it runs; re-check the exact command before running it",
		matches:     func(_ *posixEvidence, c *posixCall) bool { return callLocksOutPOSIX(c) },
	},
	{
		id:          "POSIX-VOLUME-DESTROY",
		disposition: DispositionConfirmRequired,
		explanation: "this destroys a whole volume or pool (zfs/zpool destroy, lvremove, docker volume rm, docker system prune -a) — everything on it goes at once, and a wrong name destroys a different volume's data",
		alternative: "list the exact volume with the owner (zfs list / lvs / docker volume ls) and destroy by that literal name, never by a variable",
		matches:     func(_ *posixEvidence, c *posixCall) bool { return destroysWholeVolumePOSIX(c) },
	},
	{
		id:          "POSIX-DELETE-TARGET-UNKNOWN",
		disposition: DispositionConfirmRequired,
		explanation: "this is a recursive delete whose target is only known at run time (a variable or command substitution) — an empty value turns rm -rf $DIR/ into deleting / or another wrong place",
		alternative: "resolve the path first (echo it, check it is non-empty), then delete the literal path; or loop with a non-recursive rm over expanded names",
		matches:     func(_ *posixEvidence, c *posixCall) bool { return deletesUnknownTargetPOSIX(c) },
	},
}
