//go:build !windows

package hotswap_test

import "os/exec"

// hideTestChildWindow 在非 Windows 平台是空操作。
func hideTestChildWindow(*exec.Cmd) {}
