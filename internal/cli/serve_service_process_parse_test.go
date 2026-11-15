package cli

import (
	"errors"
	"testing"
)

func TestServeCommandLineHasServeSubcommand(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "serve", args: []string{"/usr/bin/sshmgr", "serve"}, want: true},
		{name: "serve run arguments", args: []string{"/opt/sshmgr", "serve", "--addr", "127.0.0.1:7878"}, want: true},
		{name: "global option with value", args: []string{"/usr/bin/sshmgr", "--log-file", "/tmp/log", "serve"}, want: true},
		{name: "global option with embedded value", args: []string{"/usr/bin/sshmgr", "--log-file=/tmp/log", "serve"}, want: true},
		{name: "mcp bridge", args: []string{"/usr/bin/sshmgr", "mcp", "--cache"}, want: false},
		{name: "other command", args: []string{"/usr/bin/sshmgr", "cache", "pull"}, want: false},
		{name: "serve after another command", args: []string{"/usr/bin/sshmgr", "cache", "serve"}, want: false},
		{name: "program only", args: []string{"/usr/bin/sshmgr"}, want: false},
		{name: "other program", args: []string{"/usr/bin/not-sshmgr", "serve"}, want: false},
		{name: "windows image", args: []string{`C:\Tools\sshmgr.exe`, "serve"}, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := serveCommandLineHasServeSubcommand(tc.args); got != tc.want {
				t.Fatalf("serveCommandLineHasServeSubcommand(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

func TestWindowsServeServiceProcessRunningUsesServiceProcess(t *testing.T) {
	if !windowsServeServiceProcessRunning(func() (uint32, error) { return 200, nil }, func(id uint32) bool {
		return id == 200
	}) {
		t.Fatal("a live sshmgr-serve service process must be detected")
	}
	if windowsServeServiceProcessRunning(func() (uint32, error) { return 0, nil }, func(uint32) bool { return true }) {
		t.Fatal("a service without a process number must not be detected")
	}
	if windowsServeServiceProcessRunning(func() (uint32, error) { return 200, nil }, func(uint32) bool { return false }) {
		t.Fatal("a service process number whose process is gone must not be detected")
	}
	if windowsServeServiceProcessRunning(func() (uint32, error) { return 0, errors.New("service not installed") }, func(uint32) bool { return true }) {
		t.Fatal("a service query error must not be detected as running")
	}
}
