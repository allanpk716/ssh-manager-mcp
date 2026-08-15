//go:build !windows

package tui

import "os"

// IsTerminal reports whether fd is a character device. Unix terminals are
// char devices, so the pre-existing ModeCharDevice check is kept here
// (Plan 20 A4 fixes the Windows NUL hang only — the project's primary
// platform; `< /dev/null` on Unix is still accepted by this check, as it
// always was). os.Stat needs an *os.File, and wrapping an arbitrary fd in
// os.NewFile would attach a finalizer that may close it, so the fd is
// matched against the standard streams this package probes; any other fd
// conservatively reports false.
func IsTerminal(fd uintptr) bool {
	for _, f := range []*os.File{os.Stdin, os.Stdout, os.Stderr} {
		if f.Fd() == fd {
			fi, err := f.Stat()
			return err == nil && fi.Mode()&os.ModeCharDevice != 0
		}
	}
	return false
}
