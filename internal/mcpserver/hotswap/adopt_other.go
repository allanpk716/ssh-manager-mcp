//go:build !windows

package hotswap

import "os/exec"

// applyHiddenWindow 在非 Windows 平台是空操作(无控制台窗口概念)。
func applyHiddenWindow(*exec.Cmd) {}
