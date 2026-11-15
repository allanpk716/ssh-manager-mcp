//go:build windows

package cli

import (
	"os"
	"os/exec"
)

// serveProcessRunningWindows reports whether another sshmgr.exe process is
// currently running on Windows. Uses tasklist's CSV output (opencode #7 fix
// carried forward from the old serve_install_windows.go): match the process
// image name exactly and ignore this command's own process number.
func serveProcessRunningWindows() bool {
	out, err := exec.Command("tasklist.exe", "/FI", "IMAGENAME eq sshmgr.exe", "/FO", "CSV", "/NH").CombinedOutput()
	if err != nil {
		// tasklist prints "INFO: No tasks are running ..." (non-zero exit) when
		// nothing matches; that is the not-running case, not an error.
		return false
	}
	return windowsTaskListHasServeProcess(out, os.Getpid())
}

// serveProcessRunningPOSIX is unreachable on Windows builds; the untagged
// serve_service.go dispatches via runtime.GOOS, so the linker needs a symbol
// with this name but it is never called on Windows. Keep a panic stub so a
// future code path that accidentally calls it surfaces clearly.
func serveProcessRunningPOSIX() bool { panic("unreachable on windows") }
