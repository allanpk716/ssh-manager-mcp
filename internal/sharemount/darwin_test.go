package sharemount

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

func TestDarwinMountCommandPassesPOSIXShellSyntaxCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 没有 POSIX shell 解释器")
	}
	command := buildDarwinMountCommand(
		&models.SharedStorage{Name: "datasets", MountUser: "mount-user"},
		[]*Candidate{{Address: "192.0.2.10"}, {Address: "100.64.10.10"}},
	)
	if strings.Contains(command, "LaunchDaemon") {
		t.Fatalf("macOS 固定命令不能使用 LaunchDaemon：%s", command)
	}
	script := t.TempDir() + "/darwin-mount-command.sh"
	if err := os.WriteFile(script, []byte(command), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("/bin/sh", "-n", script).CombinedOutput(); err != nil {
		t.Fatalf("macOS 固定命令不是合法 POSIX shell 脚本：%v\n%s\n%s", err, output, command)
	}
}
