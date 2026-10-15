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
