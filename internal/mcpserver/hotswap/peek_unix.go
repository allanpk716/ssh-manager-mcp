//go:build unix

package hotswap

import (
	"os"

	"golang.org/x/sys/unix"
)

// peek_unix.go — 窥视读端的平台实现(POSIX:poll 的零超时探测)。

// peekReadable 窥视读端:POLLIN/POLLHUP/POLLERR 任一置位即「发起读会
// 立即返回」(数据或 EOF)。ok=false 表示窥视不可用(调用方退回阻塞读)。
func peekReadable(f *os.File) (readable, ok bool) {
	rc, err := f.SyscallConn()
	if err != nil {
		return false, false
	}
	var pollFd int32 = -1
	if err := rc.Control(func(fd uintptr) { pollFd = int32(fd) }); err != nil || pollFd < 0 {
		return false, false
	}
	n, err := unix.Poll([]unix.PollFd{{Fd: pollFd, Events: unix.POLLIN}}, 0)
	if err != nil {
		return false, false
	}
	return n > 0, true
}
