package sharemount

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

func TestLinuxReselectTreatsStoppedAutomountAsUnmounted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 没有 POSIX shell 解释器")
	}
	shell := posixShellForTest(t)
	unmountScript := `set -eu
mount_point='/mnt/sshmgr/datasets'
umount() {
  echo umount-called
  return 32
}
` + linuxReselectUnmountScript(mountCommandOptions{Reselect: true})
	output, err := exec.Command(shell, "-c", unmountScript).CombinedOutput()
	if err != nil {
		t.Fatalf("automount 停止后挂载点已不在挂载状态时仍应继续：%v\n%s\n脚本：\n%s",
			err, output, unmountScript)
	}
	if strings.Contains(string(output), "umount-called") {
		t.Fatalf("挂载点未挂载时不应调用 umount：%q\n脚本：\n%s", output, unmountScript)
	}
}

func TestLinuxReselectStillFailsWhenMountedUnmountFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 没有 POSIX shell 解释器")
	}
	shell := posixShellForTest(t)
	helper := linuxReselectUnmountScript(mountCommandOptions{Reselect: true})
	mountedCheck := mountTableCheckScript(CheckSystemLinux)
	helper = strings.Replace(helper, mountedCheck, "sshmgr_mounted=yes\n", 1)
	script := `set -eu
mount_point='/mnt/sshmgr/datasets'
umount() { return 32; }
` + helper
	output, err := exec.Command(shell, "-c", script).CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 85 {
		t.Fatalf("仍处于挂载状态且 umount 失败时退出结果 = %v，期待 85；输出：\n%s\n脚本：\n%s",
			err, output, script)
	}
	if !strings.Contains(string(output), linuxReselectMarker+" unmount-failed") {
		t.Fatalf("失败输出缺少固定标记：%q\n脚本：\n%s", output, script)
	}
}

func TestLinuxReselectForcePathDoesNotAddStrictUnmount(t *testing.T) {
	helper := linuxReselectUnmountScript(mountCommandOptions{Reselect: true, Force: true})
	if strings.Contains(helper, "umount") || strings.Contains(helper, mountTableCheckScript(CheckSystemLinux)) {
		t.Fatalf("force 重选路径应保持不执行严格卸载：%s", helper)
	}
}

func TestLinuxReselectCommandCarriesLoginUserForOwnerMapping(t *testing.T) {
	command := buildLinuxReselectCommand(
		"deploy", &models.SharedStorage{ID: "share-001", Name: "datasets", Version: 7},
		[]*Candidate{{Address: "192.0.2.10"}}, false,
	)
	for _, needed := range []string{
		"sshmgr_login_user='deploy'",
		`id -u -- "$sshmgr_login_user"`,
		`id -g -- "$sshmgr_login_user"`,
		"uid=$sshmgr_uid,gid=$sshmgr_gid",
	} {
		if !strings.Contains(command, needed) {
			t.Fatalf("Linux 重选命令缺少 %q：%s", needed, command)
		}
	}
}
