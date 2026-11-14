package sharemount

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

func TestDarwinMountCommandPassesPOSIXShellSyntaxCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 没有 POSIX shell 解释器")
	}
	command := buildDarwinMountCommand(
		&models.Server{User: "deploy"},
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

func TestDarwinMountExitCodesHaveExplanationsAndClasses(t *testing.T) {
	command := buildDarwinMountCommand(
		&models.Server{User: "deploy"},
		&models.SharedStorage{Name: "datasets", MountUser: "mount-user"},
		[]*Candidate{{Address: "192.0.2.10"}},
	)
	validClasses := map[string]bool{
		MountClassMissingSoftware: true, MountClassUnmountFailed: true,
		MountClassVerifyFailed: true, MountClassLockTimeout: true,
		MountClassRemoteVersionNewer: true, MountClassError: true,
	}
	for _, code := range []int{
		darwinExitMissingCommand, darwinExitUnmountFailed, darwinExitVerifyFailed,
		darwinExitLockTimeout, darwinExitRemoteNewer, darwinExitInvalidConfig,
	} {
		if !strings.Contains(command, "exit "+strconv.Itoa(code)) &&
			!strings.Contains(command, "="+strconv.Itoa(code)+"\n") {
			t.Fatalf("macOS 固定命令应使用常量插值退出码 %d：%s", code, command)
		}
		err := darwinMountExitCodeError(code)
		if err == nil {
			t.Fatalf("macOS 退出码 %d 没有中文说明", code)
		}
		var mountFailure *MountFailure
		if !errors.As(err, &mountFailure) || !validClasses[mountFailure.Class] {
			t.Fatalf("macOS 退出码 %d 的错误 = %v，分类不在允许集合内", code, err)
		}
	}
	source, err := os.ReadFile("darwin.go")
	if err != nil {
		t.Fatal(err)
	}
	if exitLiteral.Find(source) != nil {
		t.Fatalf("macOS 固定命令源码不能写非零数字退出码字面量：%s", exitLiteral.Find(source))
	}
}
