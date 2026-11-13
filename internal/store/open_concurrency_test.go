package store

import (
	"path/filepath"
	"sync"
	"testing"
)

// TestOpenConcurrentlyPreventsSchemaLockFailure 复现单机 Web UI 的并发窗口：
// 保存、后台连接测试与结果页读取会同时打开同一个本机 vault。
func TestOpenConcurrentlyPreventsSchemaLockFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.db")
	key, err := GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	initial, err := Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := initial.Close(); err != nil {
		t.Fatal(err)
	}

	const openers = 12
	var wait sync.WaitGroup
	errors := make(chan error, openers)
	wait.Add(openers)
	for i := 0; i < openers; i++ {
		go func() {
			defer wait.Done()
			store, openErr := Open(path, key)
			if openErr != nil {
				errors <- openErr
				return
			}
			defer store.Close()
			if _, listErr := store.ListServers(); listErr != nil {
				errors <- listErr
			}
		}()
	}
	wait.Wait()
	close(errors)
	for openErr := range errors {
		t.Fatalf("并发打开不得让结构检查变成数据库锁定错误：%v", openErr)
	}
}
