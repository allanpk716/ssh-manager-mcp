package store

import "os"

// TemporaryClientCacheStore 是把客户端缓存快照导入临时数据库后的查询载
// 体。Path 仅供诊断与测试读取；调用方按自己的场景设置只读模式，并必须通
// 过 Close 关闭存储和清理临时数据库文件。
type TemporaryClientCacheStore struct {
	Store *Store
	Path  string
}

// OpenTemporaryClientCacheStore 用一次性主密钥新建临时数据库，并按客户端
// 缓存的导入规则写入快照。pattern 是 os.CreateTemp 的文件名模式。导入成功
// 后调用方仍需自行设置只读模式和设备码名字；失败时这里的临时文件已被清理。
func OpenTemporaryClientCacheStore(snapshot *Snapshot, pattern string) (TemporaryClientCacheStore, error) {
	throwawayKey, err := GenerateMasterKey()
	if err != nil {
		return TemporaryClientCacheStore{}, err
	}
	tempFile, err := os.CreateTemp("", pattern)
	if err != nil {
		return TemporaryClientCacheStore{}, err
	}
	tempPath := tempFile.Name()
	_ = tempFile.Close()

	st, err := Open(tempPath, throwawayKey)
	if err != nil {
		removeTemporaryStore(tempPath)
		return TemporaryClientCacheStore{}, err
	}
	temporary := TemporaryClientCacheStore{Store: st, Path: tempPath}
	if err := st.ImportSnapshotForClientCache(snapshot); err != nil {
		_ = temporary.Close()
		return TemporaryClientCacheStore{}, err
	}
	return temporary, nil
}

// Close 关闭临时数据库，并尽力删除主文件以及 SQLite 的预写日志、共享内
// 存附属文件。返回值是关闭数据库的错误；删除文件沿用临时文件的尽力清理
// 语义。
func (temporary TemporaryClientCacheStore) Close() error {
	if temporary.Store == nil {
		removeTemporaryStore(temporary.Path)
		return nil
	}
	err := temporary.Store.Close()
	removeTemporaryStore(temporary.Path)
	return err
}

func removeTemporaryStore(path string) {
	_ = os.Remove(path)
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
}
