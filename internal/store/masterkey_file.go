package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"ssh-manager-mcp/internal/paths"
)

// FileKeyProvider stores the master key as a plaintext file (0600 on Unix;
// on Windows the FILE itself is ACL-locked by HardenACL after every Set —
// SYSTEM+Admins+current user, inheritance disabled, broad groups removed;
// see HardenACL / spec §5.2). Weaker than DPAPI/keychain; intended ONLY for
// environments with neither (CI / containers / headless Linux without
// secret-service). Windows production uses DpapiKeyProvider; this is the
// last-resort fallback in resolveMasterKey.
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
	if err := os.Rename(tmpPath, p.path()); err != nil {
		return err
	}
	// HardenACL after the atomic rename. Spec §5.2: under L1+ the master.key
	// is PLAINTEXT, so the Windows ACL is the ONLY protection layer — a fresh
	// tmp file inherits the parent dir's DACL (which may include Authenticated
	// Users:modify), so we re-ACL unconditionally after rename. Hard-fail on
	// error: if we can't lock the ACL we MUST NOT leave plaintext world-
	// readable (the caller surfaces "needs admin"). Run on every Set (idempotent
	// — also defends against an external process loosening the ACL between Sets).
	// On non-Windows HardenACL is a no-op (file mode 0600 is the protection).
	if err := HardenACL(p.path()); err != nil {
		// Defensive cleanup: the just-renamed plaintext file currently has a
		// potentially-broad inherited ACL. Try to remove it so a failed ACL
		// set doesn't leave a world-readable key on disk. Removal failure is
		// appended to the error chain but does not mask the ACL error (which
		// is what the caller must act on).
		if rmErr := os.Remove(p.path()); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			return fmt.Errorf("harden ACL on master key %q (cleanup also failed: %v): %w", p.path(), rmErr, err)
		}
		return fmt.Errorf("harden ACL on master key %q (plaintext file removed): %w", p.path(), err)
	}
	return nil
}

func (p FileKeyProvider) Delete() error {
	err := os.Remove(p.path())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
