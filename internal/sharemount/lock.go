package sharemount

import (
	"fmt"

	"ssh-manager-mcp/internal/sshbroker"
)

// sharedStorageLockPath 是 Linux 与 macOS 共用的服务器级文件锁路径。
// /var/run 在 Linux 上指向 /run 临时文件系统、macOS 开机时清理，服务器
// 重启即清空，避免强杀进程留下的锁目录永久卡住后续挂载。
const sharedStorageLockPath = "/var/run/sshmgr-mount.lock"

// sharedStorageLockExpireSeconds 是残留锁的回收阈值。整次挂载调用（包含
// 固定挂载命令）的默认超时是 DefaultTimeout = 5 分钟（300 秒），600 秒
// 大于它，保证持有者仍在最长执行窗口内时不会被误回收。
const sharedStorageLockExpireSeconds = 600

// sharedStorageDirectoryLockScript 返回 Linux 与 macOS 都可用的服务器级
// 文件锁前缀。锁是一把全服务器共享的目录：mkdir 在 POSIX 文件系统上是
// 原子操作，不需要 Linux 才有的 flock 命令；超时后调用方报「另一个挂载
// 正在进行」。
//
// 拿锁成功后立即写 owner 文件（内容为拿锁时刻的 Unix 秒数）并设置 trap
// 释放，然后才把控制权交回调用方；调用方随后的清理函数必须继续调用
// sshmgr_release_shared_storage_lock。等待拿锁时按 sharedStorageLockExpire
// Seconds 阈值回收残留锁：owner 文件存在就以它的秒数为準，不存在（持有
// 者写 owner 前被杀）就以锁目录自身修改时间为准；先删 owner 文件再
// rmdir 锁目录，绝不递归删除。调用方超时通常意味着另一个挂载仍在进行。
func sharedStorageDirectoryLockScript(
	lockPath string, attempts int, timeoutExitCode int, outputMarker string,
) string {
	quotedPath := sshbroker.POSIXShellQuote(lockPath)
	return fmt.Sprintf(`sshmgr_lock_dir=%s
sshmgr_lock_owner="$sshmgr_lock_dir/owner"
sshmgr_lock_attempts=0
sshmgr_reclaim_shared_storage_lock() {
  sshmgr_lock_timestamp=''
  if [ -f "$sshmgr_lock_owner" ]; then
    sshmgr_lock_timestamp=$(cat "$sshmgr_lock_owner" 2>/dev/null) || sshmgr_lock_timestamp=''
  fi
  if [ -z "$sshmgr_lock_timestamp" ]; then
    sshmgr_lock_timestamp=$(stat -f %%m "$sshmgr_lock_dir" 2>/dev/null) || sshmgr_lock_timestamp=''
  fi
  if [ -z "$sshmgr_lock_timestamp" ]; then
    sshmgr_lock_timestamp=$(stat -c %%Y "$sshmgr_lock_dir" 2>/dev/null) || sshmgr_lock_timestamp=''
  fi
  case "$sshmgr_lock_timestamp" in *[!0-9]*|'') return 0 ;; esac
  sshmgr_lock_now=$(date +%%s)
  sshmgr_lock_hold_time=$((sshmgr_lock_now - sshmgr_lock_timestamp))
  [ "$sshmgr_lock_hold_time" -ge %d ] || return 0
  rm -f "$sshmgr_lock_owner"
  rmdir "$sshmgr_lock_dir" 2>/dev/null || true
}
until mkdir "$sshmgr_lock_dir" 2>/dev/null; do
  sshmgr_lock_attempts=$((sshmgr_lock_attempts+1))
  if [ "$sshmgr_lock_attempts" -ge %d ]; then
    echo %s lock-timeout >&2
  exit %d
  fi
  sshmgr_reclaim_shared_storage_lock
  sleep 1
done
date +%%s > "$sshmgr_lock_owner"
sshmgr_release_shared_storage_lock() {
  rm -f "$sshmgr_lock_owner" 2>/dev/null || true
  rmdir "$sshmgr_lock_dir" 2>/dev/null || true
}
trap sshmgr_release_shared_storage_lock EXIT HUP INT TERM
`, quotedPath, sharedStorageLockExpireSeconds, attempts,
		sshbroker.POSIXShellQuote(outputMarker), timeoutExitCode)
}
