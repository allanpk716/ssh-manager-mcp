//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// serveProcessRunningWindows is unreachable on POSIX builds; the untagged
// serve_service.go dispatches via runtime.GOOS, so the linker needs a symbol
// with this name but it is never called on POSIX. Keep a panic stub so a
// future code path that accidentally calls it surfaces clearly.
func serveProcessRunningWindows() bool { panic("unreachable on posix") }

// serveProcessRunningPOSIX reports whether another `sshmgr serve` process is
// currently running on Linux/macOS/etc. Linux scans /proc/<process
// number>/cmdline and ignores this command's own process number; on macOS
// /proc does not exist and this returns false (best-effort; the other three
// status signals compensate).
//
// Why /proc/cmdline and not pgrep / ps: command-line arguments distinguish a
// serve process from an MCP bridge or other sshmgr subcommand. pgrep is not
// always installed (minimal containers, scratch images); `ps` exists
// everywhere but spawning it per status call is heavier than walking /proc.
// /proc is Linux-only; macOS intentionally keeps its historical best-effort
// behavior.
func serveProcessRunningPOSIX() bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		// macOS has no /proc — return false (best-effort).
		return false
	}
	return serveProcessRunningFromProcEntries("/proc", os.Getpid(), entries, os.ReadFile)
}

// serveProcessRunningFromProcEntries is the testable /proc enumeration seam.
// The caller supplies the directory entries and the file reader; only numeric
// directories are considered, matching the /proc layout on Linux.
func serveProcessRunningFromProcEntries(root string, currentProcessID int, entries []os.DirEntry, readCmdline func(string) ([]byte, error)) bool {
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// /proc entries are process numbers; skip non-numeric names such as
		// /proc/self and /proc/buddyinfo.
		name := e.Name()
		processID, err := strconv.Atoi(name)
		if err != nil {
			continue
		}
		if processID == currentProcessID {
			continue
		}
		rawCmdline, err := readCmdline(filepath.Join(root, name, "cmdline"))
		if err != nil {
			continue
		}
		commandLine := strings.Split(strings.TrimRight(string(rawCmdline), "\x00"), "\x00")
		if serveCommandLineHasServeSubcommand(commandLine) {
			return true
		}
	}
	return false
}
