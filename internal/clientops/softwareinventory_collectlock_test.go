package clientops

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestSoftwareInventoryCollectionLockFile 用同一个锁文件验证计划任务与
// MCP 桥的顺手补采不能并发执行。
func TestSoftwareInventoryCollectionLockFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "software-inventory-collect.lock")
	lock, err := AcquireSoftwareInventoryCollectionLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, busy := AcquireSoftwareInventoryCollectionLock(path); !errors.Is(busy, ErrSoftwareInventoryCollectionBusy) {
		t.Fatalf("第二把锁应返回忙：%v", busy)
	}
	lock.Unlock()
	second, err := AcquireSoftwareInventoryCollectionLock(path)
	if err != nil {
		t.Fatal(err)
	}
	second.Unlock()
}

// TestSoftwareInventoryCollectionLockPathUsesCacheInstance 钉住计划任务与
// MCP 桥共用的实例锁文件路径。
func TestSoftwareInventoryCollectionLockPathUsesCacheInstance(t *testing.T) {
	t.Setenv("SSHMGR_CACHE_DIR", t.TempDir())
	path, err := SoftwareInventoryCollectionLockPath("")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "software-inventory-collect.lock" {
		t.Fatalf("锁文件路径 = %q", path)
	}
}
