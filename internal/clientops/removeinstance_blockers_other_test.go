//go:build !windows

package clientops

import (
	"os"
	"path/filepath"
	"testing"
)

// blockDekDelete / blockDirRemove inject REAL filesystem deletion failures
// (Unix side): removing a file needs WRITE permission on its PARENT dir, so
// stripping it (0500 = r+x) makes os.Remove(dek) fail with EACCES while the
// file itself stays fully readable — a true partial-failure injection, not a
// simulation. Both return the undo func; after undo a re-run of
// RemoveInstance must finish idempotently.

func blockDekDelete(t *testing.T, dekPath string) func() {
	t.Helper()
	root := filepath.Dir(dekPath)
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatalf("blockDekDelete: chmod %s: %v", root, err)
	}
	return func() { _ = os.Chmod(root, 0o700) }
}

func blockDirRemove(t *testing.T, slotDir string) func() {
	t.Helper()
	if err := os.Chmod(slotDir, 0o500); err != nil {
		t.Fatalf("blockDirRemove: chmod %s: %v", slotDir, err)
	}
	return func() { _ = os.Chmod(slotDir, 0o700) }
}
