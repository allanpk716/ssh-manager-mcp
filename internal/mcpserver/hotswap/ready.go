package hotswap

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// 就绪协议的默认节奏:轮询间隔落在规格建议的 50–100ms 区间上沿,
// 总超时 15 秒(规格实施决策第 3 条)。两者都可注入(测试用短超时)。
const (
	DefaultReadyTimeout = 15 * time.Second
	DefaultPollInterval = 100 * time.Millisecond
)

// ErrReadyTimeout 报告继任未在超时内写出合法就绪文件。
var ErrReadyTimeout = errors.New("successor not ready in time")

// Ready 是就绪文件的内容:继任完成自身初始化后写出,含其版本号。
type Ready struct {
	Version string `json:"version"`
}

// WriteReady 供继任侧使用:完成自身初始化后把含版本号的就绪文件写到 path。
// 内容为一段 JSON;父侧轮询期间读到的半写/空版本内容一律视为「尚未就绪」。
func WriteReady(path, version string) error {
	if version == "" {
		return errors.New("hotswap: ready version must not be empty")
	}
	b, err := json.Marshal(Ready{Version: version})
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// WaitReady 供父侧使用:轮询就绪文件直到出现合法内容(含非空版本)或到
// 超时。timeout/poll 非正值时用默认值。只看文件,不监听进程退出——
// 需要同时监听继任提前退出的场合用 Handover(它内置两者的竞速)。
func WaitReady(path string, timeout, poll time.Duration) (Ready, error) {
	if poll <= 0 {
		poll = DefaultPollInterval
	}
	if timeout <= 0 {
		timeout = DefaultReadyTimeout
	}
	deadline := time.Now().Add(timeout)
	for {
		if r, ok := readReady(path); ok {
			return r, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return Ready{}, fmt.Errorf("hotswap: ready file %q: %w after %v", path, ErrReadyTimeout, timeout)
		}
		if poll < remaining {
			time.Sleep(poll)
		} else {
			time.Sleep(remaining)
		}
	}
}

// readReady 读一次就绪文件:文件缺失、内容非法或版本为空都算「尚未就绪」。
func readReady(path string) (Ready, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Ready{}, false
	}
	var r Ready
	if err := json.Unmarshal(b, &r); err != nil || r.Version == "" {
		return Ready{}, false
	}
	return r, true
}
