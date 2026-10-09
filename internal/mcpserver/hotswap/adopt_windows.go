//go:build windows

package hotswap

import (
	"os/exec"
	"syscall"
)

// applyHiddenWindow 给继任进程加隐藏窗口属性(零闪窗):继任是控制台程序
// 而本进程没有控制台时,Windows 会为它新建可见控制台窗口;显式隐藏后,
// 即便新建也不可见。标准输入输出句柄的传递不受影响。
func applyHiddenWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}
