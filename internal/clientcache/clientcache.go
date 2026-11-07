// Package clientcache resolves the local files shared by all client-cache
// features: the cache directory, pull credential, and named instance slots.
package clientcache

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"ssh-manager-mcp/internal/instname"
	"ssh-manager-mcp/internal/paths"
)

// Credential is the persisted device-code credential in cache.auth.json.
type Credential struct {
	URL   string `json:"url"`
	Token string `json:"token"`         // 设备码本体
	Pin   string `json:"pin,omitempty"` // 最近一次成功拉取时生效的服务器证书固定值
}

// DirectoryFor resolves one client-cache directory. An empty instance means
// the default slot. SSHMGR_CACHE_DIR is a complete single-cache override and
// therefore takes precedence over the instance name.
func DirectoryFor(instance string) (string, error) {
	if instance != "" {
		if err := instname.Valid(instance); err != nil {
			return "", err
		}
	}
	if directory := os.Getenv("SSHMGR_CACHE_DIR"); directory != "" {
		return directory, nil
	}
	base, err := paths.UserConfigDir()
	if err != nil {
		return "", err
	}
	directory := filepath.Join(base, "ssh-manager")
	if instance != "" {
		directory = filepath.Join(directory, "instances", instance)
	}
	return directory, nil
}

// PathsFor returns the cache directory and its snapshot, metadata, and audit
// files for one instance.
func PathsFor(instance string) (directory, snapshot, metadata, audit string, err error) {
	directory, err = DirectoryFor(instance)
	if err != nil {
		return "", "", "", "", err
	}
	return directory,
		filepath.Join(directory, "cache.bin"),
		filepath.Join(directory, "cache.meta.json"),
		filepath.Join(directory, "cache-audit.log"),
		nil
}

// CredentialPathFor returns the cache.auth.json path for one instance.
func CredentialPathFor(instance string) (string, error) {
	directory, err := DirectoryFor(instance)
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, "cache.auth.json"), nil
}

// ReadCredentialFor returns one instance's pull credential. A missing file
// means no enrollment; a present but incomplete or corrupt file is an error.
func ReadCredentialFor(instance string) (*Credential, error) {
	path, err := CredentialPathFor(instance)
	if err != nil {
		return nil, err
	}
	blob, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var credential Credential
	if err := json.Unmarshal(blob, &credential); err != nil {
		return nil, fmt.Errorf("cache.auth.json corrupt: %w", err)
	}
	if credential.URL == "" || credential.Token == "" {
		return nil, fmt.Errorf("cache.auth.json incomplete (missing url/token)")
	}
	return &credential, nil
}

// InstancesRoot is where named instances live. It deliberately ignores
// SSHMGR_CACHE_DIR because that variable is a complete single-cache override.
func InstancesRoot() (string, error) {
	base, err := paths.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "ssh-manager", "instances"), nil
}

// ListInstanceSlots returns sorted named-instance directory names. A missing
// root means no named instances.
func ListInstanceSlots() ([]string, error) {
	root, err := InstancesRoot()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, entry := range entries {
		if entry.IsDir() {
			out = append(out, entry.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}
