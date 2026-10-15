package policy

import "strings"

// safeDeviceNames 是 /dev/ 下「不是磁盘设备」的知名名字:写它们要么
// 是日常写法(/dev/null),要么写入内容本来就被丢弃。判定「直接写磁盘
// 设备」时排除它们,避免日常命令被误拦。
var safeDeviceNames = map[string]bool{
	"null": true, "zero": true, "full": true,
	"random": true, "urandom": true,
	"stdin": true, "stdout": true, "stderr": true,
	"console": true, "ptmx": true,
}

// safeDevicePrefixes 同上,按前缀放行(tty*、fd/*、shm/*)。
var safeDevicePrefixes = []string{"tty", "fd/", "shm/"}

// isDiskDevicePath 报告一个路径是否指向 /dev/ 下的磁盘设备
// (块设备、分区、映射卷等)。/dev/null 这类非磁盘设备不算。
func isDiskDevicePath(path string) bool {
	rest, ok := strings.CutPrefix(path, "/dev/")
	if !ok {
		return false
	}
	if safeDeviceNames[rest] {
		return false
	}
	for _, p := range safeDevicePrefixes {
		if strings.HasPrefix(rest, p) {
			return false
		}
	}
	return true
}

// criticalPOSIXPaths 是删除判定用的系统关键路径集合:根目录、
// Linux 的 /etc、/usr、/boot、/var/lib、/home 本身,macOS 的 /System、
// /Library、/Applications、/Users 本身。判定只认路径本身,不认它
// 下面的子路径。
var criticalPOSIXPaths = map[string]bool{
	"/":             true,
	"/etc":          true,
	"/usr":          true,
	"/boot":         true,
	"/var/lib":      true,
	"/home":         true,
	"/System":       true,
	"/Library":      true,
	"/Applications": true,
	"/Users":        true,
}

// criticalWindowsPaths 是删除判定用的 Windows 系统关键路径集合的
// 规范化形态:盘符写死的四个路径本身,加上不带盘符的根定位写法
// (\Windows 这类指当前盘根下的同名目录,ssh 会话几乎总在 C 盘)。
// 判定只认路径本身,不认它下面的子路径。
var criticalWindowsPaths = map[string]bool{
	`c:\`:              true,
	`c:\windows`:       true,
	`c:\program files`: true,
	`c:\users`:         true,
	`\`:                true,
	`\windows`:         true,
	`\program files`:   true,
	`\users`:           true,
}

// normalizeWindowsPath 把路径规范化到可比较形态:统一小写、正斜杠一律
// 当反斜杠、去多余斜杠与 . 段、按字面消化 .. 段(升出根时停在根),
// 结尾的通配符段按其父路径对待(C:\* 与 C:\ 同等,C:\Windows\*.*
// 与 C:\Windows 同等;路径中段的通配符保留原样,与 POSIX 拆解同一
// 取舍)。它只做文字层面的整理,不碰文件系统,也不展开变量。
func normalizeWindowsPath(path string) string {
	p := strings.ToLower(path)
	p = strings.ReplaceAll(p, "/", "\\")

	drive := ""
	absolute := false
	if len(p) >= 2 && p[1] == ':' && p[0] >= 'a' && p[0] <= 'z' {
		drive = p[:2]
		p = p[2:]
	}
	if strings.HasPrefix(p, "\\") {
		absolute = true
	}

	// 结尾的通配符段剥掉;剥到没有斜杠分隔就停(纯相对通配如 *.log
	// 不是盘根,保留相对形态)。
	for {
		i := strings.LastIndexByte(p, '\\')
		if i < 0 {
			break
		}
		last := p[i+1:]
		if last == "" || !strings.ContainsAny(last, "*?") {
			break
		}
		p = p[:i]
	}

	var segs []string
	for _, seg := range strings.Split(p, "\\") {
		switch seg {
		case "", ".":
			// 空段(多余斜杠)与当前目录段跳过。
		case "..":
			// 按字面消化;升出根时停在根。
			if len(segs) > 0 {
				segs = segs[:len(segs)-1]
			}
		default:
			segs = append(segs, seg)
		}
	}
	rest := strings.Join(segs, "\\")
	switch {
	case drive != "":
		return drive + "\\" + rest
	case absolute:
		return "\\" + rest
	default:
		return rest
	}
}

// normalizePOSIXPath 把路径规范化到可比较形态:去掉多余斜杠、/./ 段与
// 结尾斜杠,按字面消化 .. 段,结尾的通配符段按其父路径对待(/* 与 /
// 同等)。它只做文字层面的整理,不碰文件系统。
func normalizePOSIXPath(path string) string {
	// 结尾的通配符段去掉:/etc/* 与 /etc 同等对待;全剥完后只剩根。
	for strings.HasSuffix(path, "*") {
		path = strings.TrimSuffix(path, "*")
		path = strings.TrimSuffix(path, "/")
	}
	if path == "" {
		return "/"
	}
	if !strings.HasPrefix(path, "/") {
		return path
	}
	var segs []string
	for _, seg := range strings.Split(path, "/") {
		switch seg {
		case "", ".":
			// 空段(多余斜杠)与当前目录段跳过。
		case "..":
			// 按字面消化;升出根时停在根。
			if len(segs) > 0 {
				segs = segs[:len(segs)-1]
			}
		default:
			segs = append(segs, seg)
		}
	}
	return "/" + strings.Join(segs, "/")
}
