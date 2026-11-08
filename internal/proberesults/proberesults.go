// Package proberesults 维护「探测结果」的本地文件：每个 sshmgr 实例把
// 自己发起的连通观察（探测工具与真实 SSH 连接顺带记录）保存在实例本地
// 的独立 JSON 文件里，不回传权威端。键是「起点设备、目标设备、网络、
// 地址、端口」，同一个键只保留最新一条观察，超过 7 天的观察在读写时被
// 清理。文件位置的按角色解析与生产观察者在 internal/probeobserve。
package proberesults

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// ProbeResultRetention 是探测结果的保留期，超过后读写时清理。
const ProbeResultRetention = 7 * 24 * time.Hour

// 观察来源的固定值：探测工具写 probe，真实 SSH 连接顺带记录写 dial。
const (
	SourceProbe = "probe"
	SourceDial  = "dial"
)

// keySeparator 用一个不会出现在设备标识、网络名、地址与端口号里的控制
// 字符拼接键成员，避免不同键拼出同一个字符串。
const keySeparator = "\x1f"

// Observation 是一条探测结果观察。
type Observation struct {
	OriginDeviceID string
	TargetDeviceID string
	Network        string
	Address        string
	Port           int
	OK             bool
	Category       models.ProbeErrorCategory
	Detail         string
	// Duration 是 TCP 建连耗时；负值表示服务器取不到毫秒时钟、耗时未知。
	Duration time.Duration
	// ObservedAt 是观察时间。
	ObservedAt time.Time
	// Source 是观察来源（probe 或 dial）。
	Source string
}

func (o Observation) key() string {
	parts := []string{o.OriginDeviceID, o.TargetDeviceID, o.Network, store.NormalizeSSHHost(o.Address), strconv.Itoa(o.Port)}
	return strings.Join(parts, keySeparator)
}

type fileObservation struct {
	OriginDeviceID       string                    `json:"origin_device_id"`
	TargetDeviceID       string                    `json:"target_device_id"`
	Network              string                    `json:"network"`
	Address              string                    `json:"address"`
	Port                 int                       `json:"port"`
	OK                   bool                      `json:"ok"`
	Category             models.ProbeErrorCategory `json:"category"`
	Detail               string                    `json:"detail,omitempty"`
	DurationMilliseconds int64                     `json:"duration_ms"`
	ObservedAt           time.Time                 `json:"observed_at"`
	Source               string                    `json:"source"`
}

type fileState struct {
	Version      int                        `json:"version"`
	Observations map[string]fileObservation `json:"observations"`
}

// FileProbeResults 是保存在指定 JSON 文件里的探测结果集合。写法沿用成功
// 地址缓存：文件锁、原子替换、只给当前用户读写、带版本号。
type FileProbeResults struct {
	path string
}

// NewFileProbeResults 返回使用指定 JSON 文件的探测结果集合。
func NewFileProbeResults(path string) *FileProbeResults {
	return &FileProbeResults{path: path}
}

// Record 保存一条观察并清理过期记录；同一个键只保留最新一条。
func (f *FileProbeResults) Record(observation Observation) error {
	if observation.OriginDeviceID == "" || observation.TargetDeviceID == "" || observation.Network == "" ||
		observation.Address == "" || observation.Port <= 0 || observation.ObservedAt.IsZero() {
		return fmt.Errorf("probe observation is incomplete: %+v", observation)
	}
	lock, err := lockProbeResults(f.path)
	if err != nil {
		return err
	}
	defer lock.Close()
	state, err := f.read()
	if err != nil {
		return err
	}
	state = removeExpiredObservations(state, time.Now())
	state.Observations[observation.key()] = observationToFile(observation)
	return f.write(state)
}

// List 按起点设备（可选目标设备）列出仍保留的最新观察，按观察时间倒序
// 排列；读取时同样清理过期记录。
func (f *FileProbeResults) List(originDeviceID, targetDeviceID string) ([]Observation, error) {
	lock, err := lockProbeResults(f.path)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	state, err := f.read()
	if err != nil {
		return nil, err
	}
	expired := removeExpiredObservations(state, time.Now())
	if len(expired.Observations) != len(state.Observations) {
		if err := f.write(expired); err != nil {
			return nil, err
		}
		state = expired
	}
	out := make([]Observation, 0, len(state.Observations))
	for _, item := range state.Observations {
		if item.OriginDeviceID != originDeviceID {
			continue
		}
		if targetDeviceID != "" && item.TargetDeviceID != targetDeviceID {
			continue
		}
		out = append(out, observationFromFile(item))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].ObservedAt.Equal(out[j].ObservedAt) {
			return out[i].ObservedAt.After(out[j].ObservedAt)
		}
		if out[i].TargetDeviceID != out[j].TargetDeviceID {
			return out[i].TargetDeviceID < out[j].TargetDeviceID
		}
		return out[i].Address < out[j].Address
	})
	return out, nil
}

func (f *FileProbeResults) read() (fileState, error) {
	state := fileState{Version: 1, Observations: map[string]fileObservation{}}
	blob, err := os.ReadFile(f.path)
	if errors.Is(err, fs.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return fileState{}, err
	}
	var envelope struct {
		Version int `json:"version"`
	}
	if json.Unmarshal(blob, &envelope) != nil {
		return state, nil
	}
	if envelope.Version != 1 {
		return state, nil
	}
	if json.Unmarshal(blob, &state) != nil || state.Observations == nil {
		return fileState{Version: 1, Observations: map[string]fileObservation{}}, nil
	}
	return state, nil
}

func (f *FileProbeResults) write(state fileState) error {
	state.Version = 1
	blob, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		return err
	}
	return atomicWriteProbeResults(f.path, append(blob, '\n'))
}

func atomicWriteProbeResults(path string, blob []byte) error {
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
			return store.HardenACL(path)
		}
		lastErr = err
		if !errors.Is(err, fs.ErrPermission) {
			return err
		}
		time.Sleep(time.Duration(10+attempt*2) * time.Millisecond)
	}
	return lastErr
}

func removeExpiredObservations(state fileState, now time.Time) fileState {
	for key, item := range state.Observations {
		if item.ObservedAt.IsZero() || now.Sub(item.ObservedAt) > ProbeResultRetention {
			delete(state.Observations, key)
		}
	}
	return state
}

func observationToFile(observation Observation) fileObservation {
	return fileObservation{
		OriginDeviceID: observation.OriginDeviceID, TargetDeviceID: observation.TargetDeviceID,
		Network: observation.Network, Address: observation.Address, Port: observation.Port,
		OK: observation.OK, Category: observation.Category, Detail: observation.Detail,
		DurationMilliseconds: observation.Duration.Milliseconds(), ObservedAt: observation.ObservedAt,
		Source: observation.Source,
	}
}

func observationFromFile(item fileObservation) Observation {
	return Observation{
		OriginDeviceID: item.OriginDeviceID, TargetDeviceID: item.TargetDeviceID,
		Network: item.Network, Address: item.Address, Port: item.Port,
		OK: item.OK, Category: item.Category, Detail: item.Detail,
		Duration:   time.Duration(item.DurationMilliseconds) * time.Millisecond,
		ObservedAt: item.ObservedAt, Source: item.Source,
	}
}
