//go:build windows

package hotswap

import (
	"os"
	"syscall"
	"unsafe"
)

// peek_windows.go — 窥视管道读端的平台实现(Windows:kernel32 的
// PeekNamedPipe;先例:internal/store/dpapi_windows.go 的 LazyDLL 用法)。

var procPeekNamedPipe = syscall.NewLazyDLL("kernel32.dll").NewProc("PeekNamedPipe")

// peekReadable 窥视读端:有无立即可读字节,或对端已断(发起读会立即
// 返回剩余字节或 EOF,按可读处理)。ok=false 表示句柄不支持窥视,调用方
// 退回阻塞读。
func peekReadable(f *os.File) (readable, ok bool) {
	var avail uint32
	r, _, e := procPeekNamedPipe.Call(uintptr(f.Fd()), 0, 0, 0,
		uintptr(unsafe.Pointer(&avail)), 0)
	if r != 0 {
		return avail > 0, true
	}
	const (
		errBrokenPipe = 109 // ERROR_BROKEN_PIPE:写端已关,读端还能排干剩余字节
		errNoData     = 232 // ERROR_NO_DATA
	)
	if code, _ := e.(syscall.Errno); code == errBrokenPipe || code == errNoData {
		return true, true
	}
	return false, false
}
