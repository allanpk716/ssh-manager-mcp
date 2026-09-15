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
func WriteCacheConfig(dir, v string) error {
	if failNextConfigWriteForTest {
		failNextConfigWriteForTest = false
		return errors.New("test-injected cache.config.json write failure")
	}
	blob, err := json.Marshal(struct {
		MaxOffline string `json:"max_offline"`
	}{v})
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
