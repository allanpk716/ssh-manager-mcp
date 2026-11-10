package sharemount

import (
	"strings"
	"testing"
)

func TestSharedStorageDirectoryLockIsPortable(t *testing.T) {
	script := sharedStorageDirectoryLockScript("/etc/sshmgr/mount.lock", 3, 87, "SSHMGR-LINUX-MOUNT")

	for _, want := range []string{
		"sshmgr_lock_dir='/etc/sshmgr/mount.lock'",
		`mkdir "$sshmgr_lock_dir" 2>/dev/null`,
		`rmdir "$sshmgr_lock_dir"`,
		"sleep 1",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("文件锁脚本缺少 %q：%s", want, script)
		}
	}
	if strings.Contains(script, "flock") || strings.Contains(script, "rm -rf") {
		t.Fatalf("文件锁脚本不能依赖 flock 或递归删除：%s", script)
	}
}
