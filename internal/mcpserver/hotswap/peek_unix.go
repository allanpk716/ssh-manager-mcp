//go:build unix

package hotswap

import (
	"errors"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// peek_unix.go — 窥视读端的平台实现(POSIX:poll 的零超时探测)。

// pollPipeReadableFunc 是可替换的 poll 调用,测试用它注入「先被信号中断、
// 后成功」的返回序列;锁保护它与其他测试遗留的输入转发循环并发访问。
var (
	pollPipeReadableMu   sync.RWMutex
	pollPipeReadableFunc = func(fds []unix.PollFd) (int, error) {
		return unix.Poll(fds, 0)
	}
)

func pollPipeReadable(fds []unix.PollFd) (int, error) {
	pollPipeReadableMu.RLock()
	call := pollPipeReadableFunc
	pollPipeReadableMu.RUnlock()
	return call(fds)
}

func setPollPipeReadable(call func(fds []unix.PollFd) (int, error)) {
	pollPipeReadableMu.Lock()
	defer pollPipeReadableMu.Unlock()
	pollPipeReadableFunc = call
}

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
	for {
		n, err := pollPipeReadable([]unix.PollFd{{Fd: pollFd, Events: unix.POLLIN}})
		if err == nil {
			return n > 0, true
		}
		if !errors.Is(err, unix.EINTR) {
			return false, false
		}
		// poll 被信号打断是瞬时状态;重试同一句柄,不把可窥视误判成不可窥视。
	}
}
