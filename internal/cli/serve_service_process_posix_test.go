//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeProcCmdline(t *testing.T, root, processDirectory string, args ...string) {
	t.Helper()
	dir := filepath.Join(root, processDirectory)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	commandLine := strings.Join(args, "\x00") + "\x00"
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(commandLine), 0o600); err != nil {
		t.Fatal(err)
	}
}

func procEntries(t *testing.T, root string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestServeProcessRunningFromProcEntriesIgnoresCurrentProcess(t *testing.T) {
	root := t.TempDir()
	writeProcCmdline(t, root, "100", "/usr/bin/sshmgr", "serve")
	if serveProcessRunningFromProcEntries(root, 100, procEntries(t, root), os.ReadFile) {
		t.Fatal("the current process number must be ignored")
	}
}

func TestServeProcessRunningFromProcEntriesDetectsServe(t *testing.T) {
	root := t.TempDir()
	writeProcCmdline(t, root, "100", "/usr/bin/sshmgr", "serve", "status")
	writeProcCmdline(t, root, "200", "/usr/bin/sshmgr", "serve", "--addr", "127.0.0.1:7878")
	if !serveProcessRunningFromProcEntries(root, 100, procEntries(t, root), os.ReadFile) {
		t.Fatal("another sshmgr serve process must be detected")
	}
}

func TestServeProcessRunningFromProcEntriesDetectsServeAfterGlobalOptions(t *testing.T) {
	root := t.TempDir()
	writeProcCmdline(t, root, "200", "/usr/bin/sshmgr", "--log-file", "/tmp/sshmgr.log", "serve")
	if !serveProcessRunningFromProcEntries(root, 100, procEntries(t, root), os.ReadFile) {
		t.Fatal("serve after a global option and its value must be detected")
	}
}

func TestServeProcessRunningFromProcEntriesIgnoresOtherSshmgrSubcommands(t *testing.T) {
	root := t.TempDir()
	writeProcCmdline(t, root, "200", "/usr/bin/sshmgr", "mcp", "--cache", "--instance", "work")
	writeProcCmdline(t, root, "300", "/usr/bin/sshmgr", "cache", "pull")
	writeProcCmdline(t, root, "400", "/usr/bin/my-sshmgr", "serve")
	writeProcCmdline(t, root, "500", "/usr/bin/not-sshmgr", "serve")
	if serveProcessRunningFromProcEntries(root, 100, procEntries(t, root), os.ReadFile) {
		t.Fatal("only another sshmgr process whose first non-option argument is serve may be detected")
	}
}
