package sharemount

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSharedStorageDirectoryLockIsPortable(t *testing.T) {
	script := sharedStorageDirectoryLockScript("/var/run/sshmgr-mount.lock", 3, 87, "SSHMGR-LINUX-MOUNT")

	for _, want := range []string{
		"sshmgr_lock_dir='/var/run/sshmgr-mount.lock'",
		`mkdir "$sshmgr_lock_dir" 2>/dev/null`,
		`date +%s > "$sshmgr_lock_owner"`,
		`rm -f "$sshmgr_lock_owner"`,
		`rmdir "$sshmgr_lock_dir"`,
		"sleep 1",
		`trap sshmgr_release_shared_storage_lock EXIT HUP INT TERM`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("文件锁脚本缺少 %q：%s", want, script)
		}
	}
	if strings.Contains(script, "flock") || strings.Contains(script, "rm -rf") {
		t.Fatalf("文件锁脚本不能依赖 flock 或递归删除：%s", script)
	}
}

// runLockScriptWithHarness 在真实 POSIX shell 里执行生成的文件锁脚本，并在
// 拿锁成功后正常退出，让 trap 释放锁。返回退出码与合并输出。
func runLockScriptWithHarness(t *testing.T, script string) (int, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows 没有 POSIX shell 解释器")
	}
	harness := script + "\necho sshmgr-lock-acquired\nexit 0\n"
	command := exec.Command("/bin/sh", "-c", harness)
	output, err := command.CombinedOutput()
	exitCode := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		exitCode = exitErr.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return exitCode, string(output)
}

func writeLockOwnerFile(t *testing.T, path string, timestamp int64) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strconv.FormatInt(timestamp, 10)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSharedStorageDirectoryLockReclaimsExpiredOwnerFile(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "mount.lock")
	if err := os.Mkdir(lockPath, 0o700); err != nil {
		t.Fatal(err)
	}
	writeLockOwnerFile(t, filepath.Join(lockPath, "owner"), time.Now().Add(-700*time.Second).Unix())
	script := sharedStorageDirectoryLockScript(lockPath, 30, 87, "SSHMGR-LINUX-MOUNT")

	exitCode, output := runLockScriptWithHarness(t, script)
	if exitCode != 0 || !strings.Contains(output, "sshmgr-lock-acquired") {
		t.Fatalf("过期锁没有被回收：exit=%d output=%s", exitCode, output)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("正常退出后锁目录应被释放：%v", err)
	}
}

func TestSharedStorageDirectoryLockTimesOutWhileFreshlyHeld(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "mount.lock")
	if err := os.Mkdir(lockPath, 0o700); err != nil {
		t.Fatal(err)
	}
	writeLockOwnerFile(t, filepath.Join(lockPath, "owner"), time.Now().Unix())
	script := sharedStorageDirectoryLockScript(lockPath, 2, 87, "SSHMGR-LINUX-MOUNT")

	exitCode, output := runLockScriptWithHarness(t, script)
	if exitCode != 87 || !strings.Contains(output, "lock-timeout") {
		t.Fatalf("未过期锁不应被回收：exit=%d output=%s", exitCode, output)
	}
	if _, err := os.Stat(filepath.Join(lockPath, "owner")); err != nil {
		t.Fatalf("未过期锁的 owner 文件不应被删除：%v", err)
	}
}

func TestSharedStorageDirectoryLockReclaimsOwnerlessDirectoryByMtime(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "mount.lock")
	if err := os.Mkdir(lockPath, 0o700); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-700 * time.Second)
	if err := os.Chtimes(lockPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	script := sharedStorageDirectoryLockScript(lockPath, 30, 87, "SSHMGR-LINUX-MOUNT")

	exitCode, output := runLockScriptWithHarness(t, script)
	if exitCode != 0 || !strings.Contains(output, "sshmgr-lock-acquired") {
		t.Fatalf("没有 owner 文件的过期锁没有被按目录修改时间回收：exit=%d output=%s", exitCode, output)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("正常退出后锁目录应被释放：%v", err)
	}
}
