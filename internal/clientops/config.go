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

// WriteCacheConfig atomically persists the instance's offline cap (v is the
// raw duration string, same grammar as the env). Called by `cache pull
// --max-offline` AFTER a successful pull — a failed pull never rewrites policy.
func WriteCacheConfig(dir, v string) error {
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
