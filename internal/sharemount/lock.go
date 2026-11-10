package sharemount

import (
	"fmt"

	"ssh-manager-mcp/internal/sshbroker"
)

// sharedStorageDirectoryLockScript 返回 Linux 与 macOS 都可用的服务器级
// 文件锁前缀。锁是一把全服务器共享的空目录：mkdir 在 POSIX 文件系统上是
// 原子操作，不需要 Linux 才有的 flock 命令；超时后调用方报「另一个挂载正
// 在进行」。清理只用 rmdir 删除这个空目录，不递归删除任何目录。
//
// 调用方必须在成功拿锁后的清理函数里调用 sshmgr_release_shared_storage_lock。
// 锁目录若因进程被强制终止而残留，后来者会超时并提示人工处理；这是刻意
// 的保守行为，避免误删其他进程仍持有的锁。
func sharedStorageDirectoryLockScript(
	lockPath string, attempts int, timeoutExitCode int, outputMarker string,
) string {
	quotedPath := sshbroker.POSIXShellQuote(lockPath)
	return fmt.Sprintf(`sshmgr_lock_dir=%s
sshmgr_lock_attempts=0
until mkdir "$sshmgr_lock_dir" 2>/dev/null; do
  sshmgr_lock_attempts=$((sshmgr_lock_attempts+1))
  if [ "$sshmgr_lock_attempts" -ge %d ]; then
    echo %s lock-timeout >&2
    exit %d
  fi
  sleep 1
done
sshmgr_release_shared_storage_lock() {
  rmdir "$sshmgr_lock_dir"
}
`, quotedPath, attempts, sshbroker.POSIXShellQuote(outputMarker), timeoutExitCode)
}
