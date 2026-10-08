//go:build windows

package hotswap_test

import (
	"os/exec"
	"syscall"
)

// hideTestChildWindow 给测试直接拉起的子进程加隐藏窗口属性(零闪窗铁律,
// 生产路径的同类处理在库内 adopt_windows.go 的 applyHiddenWindow)。
func hideTestChildWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}
