//go:build windows

package tui

import (
	"syscall"
	"unsafe"
)

var kernel32 = syscall.NewLazyDLL("kernel32.dll")
var procGetConsoleMode = kernel32.NewProc("GetConsoleMode")

// IsTerminal reports whether fd is an actual console (not NUL or any other
// character device). GetConsoleMode succeeds only for real consoles. This
// replaced the old os.Stat ModeCharDevice check (Plan 20 A4): NUL is a
// character device too, so `tui < NUL` used to pass the gate and then hang
// on a stdin that can never deliver input.
func IsTerminal(fd uintptr) bool {
	var mode uint32
	r, _, _ := procGetConsoleMode.Call(fd, uintptr(unsafe.Pointer(&mode)))
	return r != 0
}
