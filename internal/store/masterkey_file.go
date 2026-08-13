package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"ssh-manager-mcp/internal/paths"
)

// FileKeyProvider stores the master key as a plaintext file (0600 on Unix;
// Windows ACL inherited from the folder — see docs). Weaker than DPAPI/
// keychain; intended ONLY for environments with neither (CI / containers /
// headless Linux without secret-service). Windows production uses
// DpapiKeyProvider; this is the last-resort fallback in resolveMasterKey.
type FileKeyProvider struct {
	Path string // empty → program-fixed paths.MasterKeyPath() (spec §3.1)
}

func (p FileKeyProvider) path() string {
	if p.Path != "" {
		return p.Path
	}
	// default: program-fixed path (spec §3.1). SSHMGR_FILEKEY_PATH read inside paths.
	pth, err := paths.MasterKeyPath()
	if err != nil || pth == "" {
		return "master.key.plain" // last-resort (test env with no fixed path)
	}
	return pth
}

func (p FileKeyProvider) Get() ([]byte, error) {
	b, err := os.ReadFile(p.path())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return b, nil
}

func (p FileKeyProvider) Set(mk []byte) error {
	dir := filepath.Dir(p.path())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// atomic write (same rationale as DpapiKeyProvider — trust root)
	tmp, err := os.CreateTemp(dir, ".master.key.plain.tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(mk); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, p.path())
}

func (p FileKeyProvider) Delete() error {
	err := os.Remove(p.path())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
