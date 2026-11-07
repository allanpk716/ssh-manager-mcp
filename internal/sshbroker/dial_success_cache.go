package sshbroker

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// DialSuccessCacheLifetime 是成功地址缓存的有效期。
const DialSuccessCacheLifetime = 10 * time.Minute

// DialSuccessRecord 记录一个服务器条目最近一次成功连接所用的地址、成功
// 时间与那次拨号耗时。
type DialSuccessRecord struct {
	Address     string
	SucceededAt time.Time
	Duration    time.Duration
}

type dialSuccessFileRecord struct {
	Address              string    `json:"address"`
	SucceededAt          time.Time `json:"succeeded_at"`
	DurationMilliseconds int64     `json:"duration_ms"`
}

type dialSuccessFileState struct {
	Version int                              `json:"version"`
	Servers map[string]dialSuccessFileRecord `json:"servers"`
}

// FileDialSuccessCache 是按服务器条目保存成功地址的共享文件缓存。
type FileDialSuccessCache struct {
	path string
}

// NewFileDialSuccessCache 返回使用指定 JSON 文件的成功地址缓存。
func NewFileDialSuccessCache(path string) *FileDialSuccessCache {
	return &FileDialSuccessCache{path: path}
}

// Success 返回服务器条目当前记录。文件不存在或无法解析时表示没有缓存。
func (c *FileDialSuccessCache) Success(server *models.Server) (DialSuccessRecord, bool, error) {
	state, err := c.read()
	if err != nil {
		return DialSuccessRecord{}, false, err
	}
	record, ok := state.Servers[server.ID]
	if !ok {
		return DialSuccessRecord{}, false, nil
	}
	return DialSuccessRecord{
		Address:     record.Address,
		SucceededAt: record.SucceededAt,
		Duration:    time.Duration(record.DurationMilliseconds) * time.Millisecond,
	}, true, nil
}

// Record 保存服务器条目最近一次成功地址。
func (c *FileDialSuccessCache) Record(server *models.Server, record DialSuccessRecord) error {
	lock, err := lockDialSuccessCache(c.path)
	if err != nil {
		return err
	}
	defer lock.Close()
	state, err := c.read()
	if err != nil {
		return err
	}
	state.Servers[server.ID] = dialSuccessFileRecord{
		Address:              record.Address,
		SucceededAt:          record.SucceededAt,
		DurationMilliseconds: record.Duration.Milliseconds(),
	}
	return c.write(state)
}

// Delete 删除服务器条目中与 address 等价的记录；没有记录时不需要写盘。
func (c *FileDialSuccessCache) Delete(server *models.Server, address string) error {
	lock, err := lockDialSuccessCache(c.path)
	if err != nil {
		return err
	}
	defer lock.Close()
	state, err := c.read()
	if err != nil {
		return err
	}
	record, ok := state.Servers[server.ID]
	if !ok || normalizeDialAddress(record.Address) != normalizeDialAddress(address) {
		return nil
	}
	delete(state.Servers, server.ID)
	return c.write(state)
}

func (c *FileDialSuccessCache) read() (dialSuccessFileState, error) {
	blob, err := os.ReadFile(c.path)
	if errors.Is(err, fs.ErrNotExist) {
		return dialSuccessFileState{Version: 1, Servers: map[string]dialSuccessFileRecord{}}, nil
	}
	if err != nil {
		return dialSuccessFileState{}, err
	}
	var state dialSuccessFileState
	if err := json.Unmarshal(blob, &state); err != nil || state.Version != 1 {
		return dialSuccessFileState{Version: 1, Servers: map[string]dialSuccessFileRecord{}}, nil
	}
	if state.Servers == nil {
		state.Servers = map[string]dialSuccessFileRecord{}
	}
	return state, nil
}

func (c *FileDialSuccessCache) write(state dialSuccessFileState) error {
	state.Version = 1
	blob, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	return atomicWriteDialSuccessCache(c.path, append(blob, '\n'))
}

func atomicWriteDialSuccessCache(path string, blob []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(blob); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt < 50; attempt++ {
		err := os.Rename(temporaryPath, path)
		if err == nil {
			if err := store.HardenACL(path); err != nil {
				return err
			}
			return nil
		}
		lastErr = err
		if !errors.Is(err, fs.ErrPermission) {
			return err
		}
		time.Sleep(time.Duration(10+attempt*2) * time.Millisecond)
	}
	return lastErr
}
