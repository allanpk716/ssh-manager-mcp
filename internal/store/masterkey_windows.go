//go:build windows

package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
)

// DpapiKeyProvider stores the master key in a file encrypted with user-scope
// DPAPI. Windows-only replacement for the keychain path: Credential Manager
// (wincred) fails in sshd/Service sessions (ERROR_NO_SUCH_LOGON_SESSION 1312),
// but DPAPI works across RDP/sshd/TaskScheduler sessions (spec 12 spike).
//
// Path is the master.key file (empty → default %AppData%\ssh-manager\master.key).
// DirUser is the username for the folder ACL (empty → current user). cache DEK
// reuses this provider with a different Path (spec 4 scope note).
type DpapiKeyProvider struct {
	Path    string
	DirUser string
}

func (p DpapiKeyProvider) path() (string, error) {
	if p.Path != "" {
		return p.Path, nil
	}
	appData := os.Getenv("AppData")
	if appData == "" {
		return "", errors.New("dpapi: %AppData% not set")
	}
	return filepath.Join(appData, "ssh-manager", "master.key"), nil
}

func (p DpapiKeyProvider) dirUser() string {
	if p.DirUser != "" {
		return p.DirUser
	}
	return os.Getenv("USERNAME")
}

func (p DpapiKeyProvider) Get() ([]byte, error) {
	path, err := p.path()
	if err != nil {
		return nil, err
	}
	blob, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	mk, err := dpapiUnprotect(blob, false)
	if err != nil {
		// Decryption failure (corrupt file / admin-reset password / session
		// anomaly): return the error AS-IS (not ErrNotFound) so resolveMasterKey
		// hard-fails instead of falling through to plaintext FileProvider.
		return nil, err
	}
	return mk, nil
}

func (p DpapiKeyProvider) Set(mk []byte) error {
	path, err := p.path()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := ensureDirACL(dir, p.dirUser()); err != nil {
		return fmt.Errorf("dpapi: ensureDirACL: %w", err)
	}
	blob, err := dpapiProtect(mk, false)
	if err != nil {
		return err
	}
	// Atomic write: temp + os.Rename. Half-write crash leaves no corrupt
	// master.key (the trust root — losing it = full vault loss). spec 5.2.
	tmp, err := os.CreateTemp(dir, ".master.key.tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	// Best-effort cleanup if any step below fails; no-op after successful rename.
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(blob); err != nil {
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
	return os.Rename(tmpPath, path)
}

func (p DpapiKeyProvider) Delete() error {
	path, err := p.path()
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// ensureDirACL creates dir (if absent) and locks its ACL to DirUser only:
// inheritance off, (OI)(CI) FullControl for the user. icacls runs
// UNCONDITIONALLY on every Set (not just on dir creation) — it's idempotent,
// and re-running it defends against the folder ACL being loosened by an
// external process between Sets.
//
// Windows ignores os.WriteFile mode bits (review consensus D); ACL must be
// explicit via icacls or SetFileSecurity. We use icacls (simpler than SDDL Go).
func ensureDirACL(dir, user string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// NOTE: run icacls unconditionally is simplest + safe; icacls is idempotent.
	// /inheritance:r disables inheritance; (OI)(CI) = object+container inherit;
	// F = full control. If user is empty this fails clearly.
	if user == "" {
		return errors.New("ensureDirACL: empty user")
	}
	cmd := exec.Command("icacls", dir, "/inheritance:r", "/grant:r", user+":(OI)(CI)F")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("icacls: %v: %s", err, out)
	}
	return nil
}
