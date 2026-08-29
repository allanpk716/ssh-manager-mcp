//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"strings"
)

// serveProcessRunningWindows is unreachable on POSIX builds; the untagged
// serve_service.go dispatches via runtime.GOOS, so the linker needs a symbol
// with this name but it is never called on POSIX. Keep a panic stub so a
// future code path that accidentally calls it surfaces clearly.
func serveProcessRunningWindows() bool { panic("unreachable on posix") }

// serveProcessRunningPOSIX reports whether a sshmgr process is currently
// running on Linux/macOS/etc. Scans /proc/<pid>/comm for an exact match on
// "sshmgr" (Linux) — on macOS /proc does not exist and this returns
// false (best-effort; the other three status signals compensate).
//
// Why /proc/comm and not pgrep / ps: keep the dependency surface to the
// stdlib + the filesystem. pgrep is not always installed (minimal containers,
// scratch images); `ps` exists everywhere but spawning it per status call is
// heavier than walking /proc. /proc is Linux-only; macOS falls through to
// the not-running answer and the process signal stays blank (the http +
// service signals still drive the overall verdict correctly).
func serveProcessRunningPOSIX() bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		// macOS has no /proc — return false (best-effort).
		return false
	}
	target := "sshmgr"
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// /proc entries are pids; skip non-numeric names (/proc/self, /proc/buddyinfo).
		name := e.Name()
		if name == "" || name[0] < '0' || name[0] > '9' {
			continue
		}
		// /proc/<pid>/comm is the executable's base name (truncated to 15 chars
		// by the kernel — TASK_COMM_LEN). "sshmgr" is 6 chars so it fits
		// untruncated.
		comm, err := os.ReadFile(filepath.Join("/proc", name, "comm"))
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(comm)) == target {
			return true
		}
	}
	return false
}
