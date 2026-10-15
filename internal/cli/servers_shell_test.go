package cli

// 命令策略·服务器 shell 类型字段(ticket #14)的 CLI 面:`servers add
// --shell` 落库、`servers edit --shell` 部分更新、`servers ls` 可见、
// 非法值拒绝。写法照抄 --expose-host 的测试(TestServersExposeHostFlags)。

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

func TestServersShellFlag(t *testing.T) {
	dbPath, mk := newHintEnv(t)

	// add:显式 PowerShell、显式 POSIX、默认(不传)三种。
	runCaptured(t, "servers", "add", "--name", "ps", "--host", "h", "--user", "u", "--shell", "powershell")
	runCaptured(t, "servers", "add", "--name", "nix", "--host", "h", "--user", "u", "--shell", "posix")
	runCaptured(t, "servers", "add", "--name", "bare", "--host", "h", "--user", "u")

	check := func(t *testing.T, name string, want models.ShellType) {
		t.Helper()
		st, err := store.Open(dbPath, mk)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		srv, _ := st.GetServerByName(name)
		if srv == nil {
			t.Fatalf("server %s missing", name)
		}
		if srv.ShellType != want {
			t.Fatalf("%s ShellType = %q, want %q", name, srv.ShellType, want)
		}
	}
	check(t, "ps", models.ShellPowerShell)
	check(t, "nix", models.ShellPOSIX)
	check(t, "bare", models.ShellPOSIX) // 默认 POSIX

	// edit:改成 cmd;不传旗标时保持现值。
	runCaptured(t, "servers", "edit", "ps", "--shell", "cmd")
	check(t, "ps", models.ShellWindowsCmd)
	runCaptured(t, "servers", "edit", "ps", "--role", "x")
	check(t, "ps", models.ShellWindowsCmd) // 未传 --shell 保持
	runCaptured(t, "servers", "edit", "bare", "--shell", "powershell")
	check(t, "bare", models.ShellPowerShell)
}

// TestServersShellFlagRejectsUnknown: 非法值在命令入口被拒,不落库。
func TestServersShellFlagRejectsUnknown(t *testing.T) {
	dbPath, mk := newHintEnv(t)
	run := func(args ...string) error {
		root := NewRootCmd()
		root.SetArgs(args)
		out := &bytes.Buffer{}
		root.SetOut(out)
		root.SetErr(out)
		return root.Execute()
	}
	for _, flag := range []string{"powershel", "bash", "POSIX"} { // 拼错/未知/大小写不符
		if err := run("servers", "add", "--name", "x", "--host", "h", "--user", "u", "--shell", flag); err == nil {
			t.Fatalf("add --shell %q must be rejected", flag)
		}
		if err := run("servers", "edit", "x", "--shell", flag); err == nil {
			t.Fatalf("edit --shell %q must be rejected", flag)
		}
	}
	st, err := store.Open(dbPath, mk)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	servers, err := st.ListServers()
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 0 {
		t.Fatalf("rejected adds must not persist, got %d servers", len(servers))
	}
}

// TestServersListShowsShellType: `servers ls` 每行可见 shell 类型(owner 的
// 查看面)。
func TestServersListShowsShellType(t *testing.T) {
	newHintEnv(t)
	runCaptured(t, "servers", "add", "--name", "ps", "--host", "h", "--user", "u", "--shell", "powershell")
	runCaptured(t, "servers", "add", "--name", "nix", "--host", "h", "--user", "u")

	root := NewRootCmd()
	root.SetArgs([]string{"servers", "ls"})
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("ls lines = %d, want 2:\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[0], "nix") || !strings.Contains(lines[0], "shell=posix") {
		t.Fatalf("nix line missing shell=posix: %q", lines[0])
	}
	if !strings.Contains(lines[1], "ps") || !strings.Contains(lines[1], "shell=powershell") {
		t.Fatalf("ps line missing shell=powershell: %q", lines[1])
	}
}

// TestServersEditShellFlagDefaultEmpty:`servers edit --shell` 的帮助文案
// 承诺「不传则保持现值」,旗标的缺省值因此必须是空串——否则 cobra 的
// 帮助面会把不传暗示成 posix。不传旗标保持现值的行为由上面的
// TestServersShellFlag 覆盖,这里只钉缺省值与文案一致。
func TestServersEditShellFlagDefaultEmpty(t *testing.T) {
	root := NewRootCmd()
	var edit *cobra.Command
	for _, c := range root.Commands() {
		if c.Name() != "servers" {
			continue
		}
		for _, sub := range c.Commands() {
			if sub.Name() == "edit" {
				edit = sub
			}
		}
	}
	if edit == nil {
		t.Fatal("servers edit subcommand not found")
	}
	flag := edit.Flags().Lookup("shell")
	if flag == nil {
		t.Fatal("servers edit has no --shell flag")
	}
	if flag.DefValue != "" {
		t.Fatalf("--shell default = %q, want \"\" (help says omit to keep current)", flag.DefValue)
	}
}
