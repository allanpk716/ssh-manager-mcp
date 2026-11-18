package store

import (
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
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

// TestIndependentStoreSchemaCheckWaitsForWriteTransaction 模拟权威端常驻
// 服务持有写事务时，另一个独立连接执行打开阶段的结构检查。结构检查必须
// 等待写锁释放，而不是在读锁升级为写锁时立即报 SQLITE_BUSY。
func TestIndependentStoreSchemaCheckWaitsForWriteTransaction(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.db")
	key := independentTestMasterKey(t)
	seed, err := Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.AddDevice("并发打开设备", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	worker := openIndependentStore(t, path, key)
	holder := openIndependentStore(t, path, key)
	holderTx := beginIndependentSettingsWrite(t, holder, "test.independent-open")

	errors := make(chan error, 1)
	go func() {
		errors <- initSchema(worker.db)
	}()
	time.Sleep(100 * time.Millisecond)
	if err := holderTx.Commit(); err != nil {
		t.Fatal(err)
	}

	select {
	case schemaErr := <-errors:
		if schemaErr != nil {
			t.Fatalf("独立连接的结构检查不能立即失败：%v", schemaErr)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("独立连接的结构检查等待超过 8 秒")
	}
}

// TestIndependentStoreWriteWaitsForWriteTransaction 覆盖软件清单采集落库
// 这类先读后写的事务：另一个独立连接已经持有写事务时，本事务必须在开始
// 时取得写意图并等待，而不是拿着旧读快照升级写锁时立即失败。
func TestIndependentStoreWriteWaitsForWriteTransaction(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.db")
	key := independentTestMasterKey(t)
	seed, err := Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.AddDevice("并发写入设备", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	worker := openIndependentStore(t, path, key)
	holder := openIndependentStore(t, path, key)
	holderTx := beginIndependentSettingsWrite(t, holder, "test.independent-write")

	errors := make(chan error, 1)
	go func() {
		write := SoftwareObservedWrite{
			Items: []models.SoftwareObservedItem{{
				Category: models.SoftwareCategorySystemPackage,
				Name:     "concurrency-test", Version: "1",
			}},
			Collector:         models.AuthorityCollector(),
			CommandSetVersion: "posix-v2", SystemType: "darwin",
		}
		errors <- worker.ReplaceSoftwareObservedItems(
			"并发写入设备", write, CLISource(),
		)
	}()
	time.Sleep(100 * time.Millisecond)
	if err := holderTx.Commit(); err != nil {
		t.Fatal(err)
	}

	select {
	case writeErr := <-errors:
		if writeErr != nil {
			t.Fatalf("独立连接写入不能立即失败：%v", writeErr)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("独立连接写入等待超过 8 秒")
	}
}

func independentTestMasterKey(t *testing.T) []byte {
	t.Helper()
	key, err := GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func openIndependentStore(t *testing.T, path string, key []byte) *Store {
	t.Helper()
	store, err := Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func beginIndependentSettingsWrite(t *testing.T, store *Store, key string) *sql.Tx {
	t.Helper()
	tx, err := store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(
		`INSERT INTO settings(key,value,updated_at) VALUES(?,?,?)`,
		key, "holder", now(),
	); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	return tx
}
