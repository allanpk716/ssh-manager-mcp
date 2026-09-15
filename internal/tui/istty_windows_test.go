//go:build windows

package tui

import (
	"os"
	"testing"
)

// TestIsTerminalNUL pins the Windows NUL trap that motivated the
// GetConsoleMode-based check (Plan 20 A4): NUL is a character device, so
// the old ModeCharDevice stat check passed for `tui < NUL` and the TUI then
// hung on a stdin that can never deliver input. GetConsoleMode must reject
// it. (Manual acceptance note in the plan: `sshmgr tui < NUL` must now
// fail fast with the "tui requires a terminal" error instead of hanging.)
func TestIsTerminalNUL(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	defer f.Close()
	if IsTerminal(f.Fd()) {
		t.Fatal("NUL must not be treated as a terminal")
	}
}
