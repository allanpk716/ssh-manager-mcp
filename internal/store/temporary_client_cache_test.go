package store

import (
	"os"
	"path/filepath"
	"testing"
)

// TestTemporaryClientCacheStoreCloseRemovesDatabaseAndSidecars 覆盖临时客
// 户端缓存的清理边界：关闭临时库时，主数据库文件和 SQLite 的预写日志、共
// 享内存附属文件都不能留在磁盘上。
func TestTemporaryClientCacheStoreCloseRemovesDatabaseAndSidecars(t *testing.T) {
	sourceDir := t.TempDir()
	masterKey, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("generate master key: %v", err)
	}
	source, err := Open(filepath.Join(sourceDir, "source.db"), masterKey)
	if err != nil {
		t.Fatalf("open source store: %v", err)
	}
	defer source.Close()
	if _, err := source.AddSite("家A", "清理测试"); err != nil {
		t.Fatalf("add site: %v", err)
	}
	snapshot, err := source.ExportSnapshot()
	if err != nil {
		t.Fatalf("export snapshot: %v", err)
	}

	temporary, err := OpenTemporaryClientCacheStore(snapshot, "sshmgr-temporary-*.db")
	if err != nil {
		t.Fatalf("open temporary client cache store: %v", err)
	}
	sites, err := temporary.Store.ListSites()
	if err != nil {
		t.Fatalf("list sites: %v", err)
	}
	if len(sites) != 1 || sites[0].Name != "家A" {
		t.Fatalf("temporary store must contain the exported site, got %+v", sites)
	}
	databasePath := temporary.Path
	if err := temporary.Close(); err != nil {
		t.Fatalf("close temporary store: %v", err)
	}
	for _, path := range []string{databasePath, databasePath + "-wal", databasePath + "-shm"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("temporary database cleanup must remove %s, stat error=%v", path, err)
		}
	}
}
