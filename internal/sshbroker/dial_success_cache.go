package sshbroker

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
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

type dialSuccessFileServer struct {
	Addresses map[string]dialSuccessFileRecord `json:"addresses"`
}

type dialSuccessFileState struct {
	Version int                              `json:"version"`
	Servers map[string]dialSuccessFileServer `json:"servers"`
}

type dialSuccessFileStateVersionOne struct {
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

// Successes 返回服务器条目当前仍有效的全部地址记录。文件不存在或无法解析
// 时表示没有缓存；过期记录在这里被忽略。
func (c *FileDialSuccessCache) Successes(server *models.Server) ([]DialSuccessRecord, bool, error) {
	state, err := c.read()
	if err != nil {
		return nil, false, err
	}
	fileServer, ok := state.Servers[server.ID]
	if !ok {
		return nil, false, nil
	}
	now := time.Now()
	out := make([]DialSuccessRecord, 0, len(fileServer.Addresses))
	for _, record := range fileServer.Addresses {
		converted := dialSuccessRecordFromFile(record)
		if !dialSuccessRecordValid(converted, now) {
			continue
		}
		out = append(out, converted)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].SucceededAt.Equal(out[j].SucceededAt) {
			return out[i].SucceededAt.Before(out[j].SucceededAt)
		}
		return out[i].Address < out[j].Address
	})
	return out, len(out) > 0, nil
}

// Record 保存服务器条目其中一个地址的成功记录，并顺手清除所有过期记录。
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
	state = removeExpiredDialSuccessRecords(state, time.Now())
	fileServer := state.Servers[server.ID]
	if fileServer.Addresses == nil {
		fileServer.Addresses = make(map[string]dialSuccessFileRecord)
	}
	fileServer.Addresses[normalizeDialAddress(record.Address)] = dialSuccessFileRecord{
		Address: record.Address, SucceededAt: record.SucceededAt,
		DurationMilliseconds: record.Duration.Milliseconds(),
	}
	state.Servers[server.ID] = fileServer
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
	fileServer, ok := state.Servers[server.ID]
	key := normalizeDialAddress(address)
	if !ok {
		return nil
	}
	_, exists := fileServer.Addresses[key]
	if !exists {
		return nil
	}
	delete(fileServer.Addresses, key)
	if len(fileServer.Addresses) == 0 {
		delete(state.Servers, server.ID)
	} else {
		state.Servers[server.ID] = fileServer
	}
	return c.write(state)
}

// ServerDialSuccess 让文件缓存可以直接作为统一拨号请求的成功地址缓存。
func (c *FileDialSuccessCache) ServerDialSuccess(server *models.Server) ([]DialSuccessRecord, bool, error) {
	return c.Successes(server)
}

// RecordServerDialSuccess 保存服务器条目其中一个地址的成功记录。
func (c *FileDialSuccessCache) RecordServerDialSuccess(server *models.Server, record DialSuccessRecord) error {
	return c.Record(server, record)
}

// DeleteServerDialSuccess 删除服务器条目其中一个地址的成功记录。
func (c *FileDialSuccessCache) DeleteServerDialSuccess(server *models.Server, address string) error {
	return c.Delete(server, address)
}

func (c *FileDialSuccessCache) read() (dialSuccessFileState, error) {
	state := dialSuccessFileState{Version: 2, Servers: map[string]dialSuccessFileServer{}}
	blob, err := os.ReadFile(c.path)
	if errors.Is(err, fs.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return dialSuccessFileState{}, err
	}
	var envelope struct {
		Version int `json:"version"`
	}
	if json.Unmarshal(blob, &envelope) != nil {
		return state, nil
	}
	switch envelope.Version {
	case 1:
		var oldState dialSuccessFileStateVersionOne
		if json.Unmarshal(blob, &oldState) != nil {
			return state, nil
		}
		for serverID, record := range oldState.Servers {
			fileServer := state.Servers[serverID]
			if fileServer.Addresses == nil {
				fileServer.Addresses = make(map[string]dialSuccessFileRecord)
			}
			fileServer.Addresses[normalizeDialAddress(record.Address)] = record
			state.Servers[serverID] = fileServer
		}
	case 2:
		if json.Unmarshal(blob, &state) != nil || state.Servers == nil {
			return dialSuccessFileState{Version: 2, Servers: map[string]dialSuccessFileServer{}}, nil
		}
	}
	return state, nil
}

func dialSuccessRecordFromFile(record dialSuccessFileRecord) DialSuccessRecord {
	return DialSuccessRecord{
		Address: record.Address, SucceededAt: record.SucceededAt,
		Duration: time.Duration(record.DurationMilliseconds) * time.Millisecond,
	}
}

func removeExpiredDialSuccessRecords(state dialSuccessFileState, now time.Time) dialSuccessFileState {
	for serverID, fileServer := range state.Servers {
		for key, fileRecord := range fileServer.Addresses {
			if !dialSuccessRecordValid(dialSuccessRecordFromFile(fileRecord), now) {
				delete(fileServer.Addresses, key)
			}
		}
		if len(fileServer.Addresses) == 0 {
			delete(state.Servers, serverID)
			continue
		}
		state.Servers[serverID] = fileServer
	}
	return state
}

func (c *FileDialSuccessCache) write(state dialSuccessFileState) error {
	state.Version = 2
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
