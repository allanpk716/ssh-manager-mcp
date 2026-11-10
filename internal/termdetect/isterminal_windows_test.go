//go:build windows

package termdetect

import (
	"os"
	"testing"
)

// TestIsTerminalNUL pins the Windows NUL trap: NUL is a character device,
// so a ModeCharDevice stat check would pass for a redirect from NUL and the
// caller could then hang on a stdin that can never deliver input.
// GetConsoleMode must reject it.
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
