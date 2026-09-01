//go:build windows

package clientops

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// blockDekDelete / blockDirRemove inject REAL filesystem deletion failures
// for the Plan 46 T2 partial-injection matrix. The pure-stdlib candidates
// (read-only attribute) do NOT work on this toolchain: modern Go's
// os.Remove/RemoveAll clears the read-only attribute and retries (probed
// 2026-09-01). A handle opened with share-mode 0 (no FILE_SHARE_DELETE) is
// the deterministic Windows blocker: DeleteFile answers sharing violation.
// Both return the undo func; after undo a re-run of RemoveInstance must
// finish idempotently.

func blockDekDelete(t *testing.T, dekPath string) func() {
	t.Helper()
	h, err := windows.CreateFile(windows.StringToUTF16Ptr(dekPath),
		windows.GENERIC_READ, 0 /* no sharing at all */, nil,
		windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatalf("blockDekDelete: open %s: %v", dekPath, err)
	}
	return func() { _ = windows.CloseHandle(h) }
}

func blockDirRemove(t *testing.T, slotDir string) func() {
	t.Helper()
	// A file INSIDE the slot dir held open with no sharing blocks the
	// RemoveAll of the whole tree.
	p := filepath.Join(slotDir, "cache.bin")
	h, err := windows.CreateFile(windows.StringToUTF16Ptr(p),
		windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatalf("blockDirRemove: open %s: %v", p, err)
	}
	return func() { _ = windows.CloseHandle(h) }
}
