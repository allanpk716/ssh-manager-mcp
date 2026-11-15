//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func writeProcComm(t *testing.T, root, processDirectory, comm string) {
	t.Helper()
	dir := filepath.Join(root, processDirectory)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "comm"), []byte(comm), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestServeProcessRunningFromProcEntriesIgnoresCurrentProcess(t *testing.T) {
	root := t.TempDir()
	writeProcComm(t, root, "100", "sshmgr\n")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if serveProcessRunningFromProcEntries(root, 100, entries, os.ReadFile) {
		t.Fatal("the current process number must be ignored")
	}
}

func TestServeProcessRunningFromProcEntriesDetectsOtherServeProcess(t *testing.T) {
	root := t.TempDir()
	writeProcComm(t, root, "100", "sshmgr\n")
	writeProcComm(t, root, "200", "sshmgr\n")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if !serveProcessRunningFromProcEntries(root, 100, entries, os.ReadFile) {
		t.Fatal("another sshmgr process must be detected")
	}
}

func TestServeProcessRunningFromProcEntriesIgnoresOtherNames(t *testing.T) {
	root := t.TempDir()
	writeProcComm(t, root, "100", "sshmgr\n")
	writeProcComm(t, root, "200", "my-sshmgr\n")
	writeProcComm(t, root, "300", "sshd\n")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if serveProcessRunningFromProcEntries(root, 100, entries, os.ReadFile) {
		t.Fatal("only the exact sshmgr process name may be detected")
	}
}
