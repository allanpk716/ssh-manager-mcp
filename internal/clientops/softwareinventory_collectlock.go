package clientops

// 软件清单客户端采集的跨进程互斥锁。计划任务运行的命令行与 MCP 桥的新
// 鲜度轮询都要先拿到同一把本地锁文件，才允许执行同一套采集逻辑。

import (
	"errors"
	"os"
	"path/filepath"
)

// ErrSoftwareInventoryCollectionBusy 表示另一个计划任务或 MCP 桥正在采集。
var ErrSoftwareInventoryCollectionBusy = errors.New("另一份软件清单采集正在执行")

// SoftwareInventoryCollectionLock 是已持有的本地锁文件。
type SoftwareInventoryCollectionLock struct {
	file *os.File
}

// SoftwareInventoryCollectionLockPath 返回一个缓存实例的采集锁文件路径。
func SoftwareInventoryCollectionLockPath(instance string) (string, error) {
	directory, _, _, _, err := CachePathsFor(instance)
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, "software-inventory-collect.lock"), nil
}

// AcquireSoftwareInventoryCollectionLock 非阻塞获取锁文件；已有采集在执行
// 时返回 ErrSoftwareInventoryCollectionBusy，不排队。
func AcquireSoftwareInventoryCollectionLock(path string) (*SoftwareInventoryCollectionLock, error) {
	if parent := filepath.Dir(path); parent != "" && parent != "." {
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return nil, err
		}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockSoftwareInventoryFile(file); err != nil {
		_ = file.Close()
		if errors.Is(err, ErrSoftwareInventoryCollectionBusy) {
			return nil, err
		}
		return nil, err
	}
	return &SoftwareInventoryCollectionLock{file: file}, nil
}

// Unlock 释放并关闭锁文件。重复调用是安全的。
func (l *SoftwareInventoryCollectionLock) Unlock() {
	if l == nil || l.file == nil {
		return
	}
	_ = unlockSoftwareInventoryFile(l.file)
	_ = l.file.Close()
	l.file = nil
}
