package sharemount

import (
	"errors"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// exitLiteral 匹配脚本源码里写死的非零退出码；退出码必须来自 Go 常量。
var exitLiteral = regexp.MustCompile(`exit\s+[1-9]`)

func TestLinuxMountCommandPassesPOSIXShellSyntaxCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 没有 POSIX shell 解释器")
	}
	command := buildLinuxMountCommand(&models.SharedStorage{ID: "share-001", Name: "datasets"}, []*Candidate{
		{Address: "192.0.2.10"}, {Address: "100.64.10.10"},
	})
	script := t.TempDir() + "/mount-command.sh"
	if err := os.WriteFile(script, []byte(command), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("/bin/sh", "-n", script).CombinedOutput(); err != nil {
		t.Fatalf("Linux 固定命令不是合法 POSIX shell 脚本：%v\n%s\n%s", err, output, command)
	}
}

func TestLinuxMountExitCodesHaveExplanationsAndClasses(t *testing.T) {
	command := buildLinuxMountCommand(&models.SharedStorage{ID: "share-001", Name: "datasets"}, []*Candidate{
		{Address: "192.0.2.10"},
	})
	validClasses := map[string]bool{
		MountClassMissingSoftware: true, MountClassProtocolFailed: true,
		MountClassUnmountFailed: true, MountClassVerifyFailed: true,
		MountClassLockTimeout: true, MountClassRemoteVersionNewer: true,
		MountClassError: true,
	}
	for _, code := range []int{
		linuxExitMissingCIFS, linuxExitProtocolFailed, linuxExitTrialUnmountFailed,
		linuxExitVerifyFailed, linuxExitLockTimeout, linuxExitRemoteNewer, linuxExitInvalidFstab,
	} {
		if !strings.Contains(command, "exit "+strconv.Itoa(code)) &&
			!strings.Contains(command, "="+strconv.Itoa(code)+"\n") {
			t.Fatalf("Linux 固定命令应使用常量插值退出码 %d：%s", code, command)
		}
		err := linuxMountExitCodeError(code)
		if err == nil {
			t.Fatalf("Linux 退出码 %d 没有中文说明", code)
		}
		var mountFailure *MountFailure
		if !errors.As(err, &mountFailure) || !validClasses[mountFailure.Class] {
			t.Fatalf("Linux 退出码 %d 的错误 = %v，分类不在允许集合内", code, err)
		}
	}
	source, err := os.ReadFile("linux.go")
	if err != nil {
		t.Fatal(err)
	}
	if exitLiteral.Find(source) != nil {
		t.Fatalf("Linux 固定命令源码不能写非零数字退出码字面量：%s", exitLiteral.Find(source))
	}
}

func TestSafeMountShareNameRejectsConfigurationFieldBreakers(t *testing.T) {
	for _, name := range []string{"", ".", "..", "data set", "data\tset", "data/set", `data\set`, "data\x00set"} {
		if safeMountShareName(name) {
			t.Fatalf("共享名 %q 不能直接作为 fstab 字段使用", name)
		}
	}
	if !safeMountShareName("datasets") {
		t.Fatal("普通共享名应可用")
	}
}
