// Package clientops: Plan 40 §3 — the per-instance offline-cap config.
// cache.config.json is MACHINE(instance)-level state (env is process-level —
// the root cause P0 closed); plaintext by design (policy, not a credential).
package clientops

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// resolveMaxOffline: env > <dir>/cache.config.json > off. A PRESENT env wins
// including its error (fail-closed is never masked by a file); the file uses
// the exact env grammar via parseMaxOffline with a file-sourced error label.
func resolveMaxOffline(dir string) (time.Duration, error) {
	if strings.TrimSpace(os.Getenv("SSHMGR_CACHE_MAX_OFFLINE")) != "" {
		return cacheMaxOffline()
	}
	blob, err := os.ReadFile(filepath.Join(dir, "cache.config.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("cache.config.json unreadable: %w", err)
	}
	var c struct {
		MaxOffline string `json:"max_offline"`
	}
	if err := json.Unmarshal(blob, &c); err != nil {
		return 0, fmt.Errorf("corrupt cache.config.json: %w", err)
	}
	return parseMaxOffline(strings.TrimSpace(c.MaxOffline), `max_offline in cache.config.json`)
}

// envSnapshotPollInterval 是轮询间隔的环境变量覆盖入口(规格 #93 / 票 #95),
// 写法与 SSHMGR_CACHE_MAX_OFFLINE 相同:进程在场时压过实例配置文件。
const envSnapshotPollInterval = "SSHMGR_CACHE_SNAPSHOT_POLL_INTERVAL"

// DefaultSnapshotPollInterval 是快照修订号轮询的默认间隔(5 分钟)。
const DefaultSnapshotPollInterval = 5 * time.Minute

// minSnapshotPollInterval 是轮询间隔的下限(1 分钟):更短的值会把权威端
// 打爆,一律拒绝。
const minSnapshotPollInterval = time.Minute

// cacheConfigFile 是 cache.config.json 的完整结构。新增键时两个字段都要
// 保留:任何一处写入都先读旧文件,只改自己的键,另一个键原样带回。
type cacheConfigFile struct {
	MaxOffline           string `json:"max_offline"`
	SnapshotPollInterval string `json:"snapshot_poll_interval,omitempty"`
}

// parseSnapshotPollInterval 是轮询间隔的统一写法(环境变量与配置文件相同):
// 空字符串表示未设置(调用方用默认值);合法时长且不小于 1 分钟才接受。
func parseSnapshotPollInterval(v, source string) (time.Duration, error) {
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < minSnapshotPollInterval {
		return 0, fmt.Errorf("invalid %s %q: must be a Go duration >= 1m (e.g. 5m)", source, v)
	}
	return d, nil
}

// readCacheConfigFile 读取实例的 cache.config.json;文件不存在时返回零值结构。
// 损坏或不可读一律报错(fail-closed,与离线上限的读法一致)。
func readCacheConfigFile(dir string) (cacheConfigFile, error) {
	blob, err := os.ReadFile(filepath.Join(dir, "cache.config.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return cacheConfigFile{}, nil
	}
	if err != nil {
		return cacheConfigFile{}, fmt.Errorf("cache.config.json unreadable: %w", err)
	}
	var c cacheConfigFile
	if err := json.Unmarshal(blob, &c); err != nil {
		return cacheConfigFile{}, fmt.Errorf("corrupt cache.config.json: %w", err)
	}
	return c, nil
}

// EffectiveSnapshotPollInterval 解析实例当前生效的快照修订号轮询间隔,并给出
// 来源标签("default" / "file" / "env"),供 `cache config` 显示与桥启动读取。
// 环境变量在场时压过文件;两者都未设置时用默认值 5 分钟。
func EffectiveSnapshotPollInterval(dir string) (time.Duration, string, error) {
	if strings.TrimSpace(os.Getenv(envSnapshotPollInterval)) != "" {
		d, err := parseSnapshotPollInterval(strings.TrimSpace(os.Getenv(envSnapshotPollInterval)), envSnapshotPollInterval)
		if err != nil {
			return 0, "env", err
		}
		return d, "env", nil
	}
	c, err := readCacheConfigFile(dir)
	if err != nil {
		return 0, "", err
	}
	d, perr := parseSnapshotPollInterval(strings.TrimSpace(c.SnapshotPollInterval), `snapshot_poll_interval in cache.config.json`)
	if perr != nil {
		return 0, "file", perr
	}
	if d == 0 {
		return DefaultSnapshotPollInterval, "default", nil
	}
	return d, "file", nil
}

// ValidateSnapshotPollInterval 是命令行校验轮询间隔的入口(与
// ValidateMaxOffline 同一模式):写文件前先按统一写法校验原始时长字符串。
func ValidateSnapshotPollInterval(v string) (time.Duration, error) {
	return parseSnapshotPollInterval(strings.TrimSpace(v), "snapshot_poll_interval")
}

// failNextConfigWriteForTest, when armed, makes the NEXT WriteCacheConfig fail
// (Plan 46 T1 失败注入矩阵:「config 写失败(既有 warning 语义)后重跑→文案仍
// 双路径」需要确定性注入,而让文件保持可读的纯文件系统注入无法跨平台成立——
// 镜像 expiry.go 的 FailNextMetaWriteForTest 同一模式)。生产永不置位。
var failNextConfigWriteForTest bool

// FailNextConfigWriteForTest arms the one-shot config write failure.
func FailNextConfigWriteForTest() { failNextConfigWriteForTest = true }

// WriteCacheConfig atomically persists the instance's offline cap (v is the
// raw duration string, same grammar as the env). Called by `cache pull
// --max-offline` AFTER a successful pull — a failed pull never rewrites policy.
// 已存在的 snapshot_poll_interval 键原样保留。
func WriteCacheConfig(dir, v string) error {
	if failNextConfigWriteForTest {
		failNextConfigWriteForTest = false
		return errors.New("test-injected cache.config.json write failure")
	}
	c, err := readCacheConfigFile(dir)
	if err != nil {
		return err
	}
	c.MaxOffline = v
	blob, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return atomicWriteUnique(filepath.Join(dir, "cache.config.json"), blob)
}

// WriteSnapshotPollIntervalConfig 原子地写入实例的快照修订号轮询间隔,已存在
// 的 max_offline 键原样保留(两个键互不覆盖)。
func WriteSnapshotPollIntervalConfig(dir, v string) error {
	c, err := readCacheConfigFile(dir)
	if err != nil {
		return err
	}
	c.SnapshotPollInterval = v
	blob, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return atomicWriteUnique(filepath.Join(dir, "cache.config.json"), blob)
}

// ValidateMaxOffline is the exported parseMaxOffline thin shell the CLI layer
// uses to validate `cache pull --max-offline` (internal/cli cannot see the
// private rule body). source label = "max_offline", matching the persisted key.
func ValidateMaxOffline(v string) (time.Duration, error) {
	return parseMaxOffline(strings.TrimSpace(v), "max_offline")
}

// validateCapFileIndependent checks the slot's cache.config.json VALIDITY
// regardless of SSHMGR_CACHE_MAX_OFFLINE (rev5 §1.2-5): on the PULL side an
// invalid file must refuse the WRITE even under a valid env — otherwise this
// pull writes what an env-less loader will later refuse ("pulls but won't
// load"). The LOAD side keeps env-wins (batch-1 semantics, §13.14).
func validateCapFileIndependent(dir string) error {
	blob, err := os.ReadFile(filepath.Join(dir, "cache.config.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cache.config.json unreadable: %w", err)
	}
	var c struct {
		MaxOffline string `json:"max_offline"`
	}
	if err := json.Unmarshal(blob, &c); err != nil {
		return fmt.Errorf("corrupt cache.config.json: %w", err)
	}
	_, perr := parseMaxOffline(strings.TrimSpace(c.MaxOffline), `max_offline in cache.config.json`)
	return perr
}

// EffectiveMaxOffline resolves a slot's effective cap with its SOURCE label
// ("env" / "file" / "off") for `cache config` display. Mirrors resolveMaxOffline
// exactly (env wins including its error — display is not a write gate).
func EffectiveMaxOffline(dir string) (time.Duration, string, error) {
	if strings.TrimSpace(os.Getenv("SSHMGR_CACHE_MAX_OFFLINE")) != "" {
		d, err := cacheMaxOffline()
		return d, "env", err
	}
	blob, err := os.ReadFile(filepath.Join(dir, "cache.config.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, "off", nil
	}
	if err != nil {
		return 0, "", fmt.Errorf("cache.config.json unreadable: %w", err)
	}
	var c struct {
		MaxOffline string `json:"max_offline"`
	}
	if err := json.Unmarshal(blob, &c); err != nil {
		return 0, "", fmt.Errorf("corrupt cache.config.json: %w", err)
	}
	d, perr := parseMaxOffline(strings.TrimSpace(c.MaxOffline), `max_offline in cache.config.json`)
	return d, "file", perr
}
