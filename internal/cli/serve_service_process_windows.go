//go:build windows

package cli

import (
	"os/exec"
	"strings"
)

// serveProcessRunningWindows reports whether sshmgr.exe is currently
// running on Windows. Uses tasklist's CSV output (opencode #7 fix carried
// forward from the old serve_install_windows.go): match the FIRST CSV field
// EXACTLY so a substring match does not false-positive on processes whose
// name merely contains "sshmgr.exe" (e.g. my-sshmgr.exe).
func serveProcessRunningWindows() bool {
	out, err := exec.Command("tasklist.exe", "/FI", "IMAGENAME eq sshmgr.exe", "/FO", "CSV", "/NH").CombinedOutput()
	if err != nil {
		// tasklist prints "INFO: No tasks are running ..." (non-zero exit) when
		// nothing matches; that is the not-running case, not an error.
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		// CSV rows look like: "sshmgr.exe","1234","Console","1","12,345 K"
		// The first field is the image name; match it EXACTLY (case-insensitive).
		fields := strings.Split(line, ",")
		if len(fields) >= 1 {
			name := strings.Trim(strings.TrimSpace(fields[0]), `"`)
			if strings.EqualFold(name, "sshmgr.exe") {
				return true
			}
		}
	}
	return false
}

// serveProcessRunningPOSIX is unreachable on Windows builds; the untagged
// serve_service.go dispatches via runtime.GOOS, so the linker needs a symbol
// with this name but it is never called on Windows. Keep a panic stub so a
// future code path that accidentally calls it surfaces clearly.
func serveProcessRunningPOSIX() bool { panic("unreachable on windows") }
