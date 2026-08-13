//go:build windows

package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// DpapiKeyProvider stores the master key in a file encrypted with machine-scope
// DPAPI (Plan 15; user-scope failed cross-logon-session, spec §3.2). Windows-only
// replacement for the keychain path: Credential Manager (wincred) fails in
// sshd/Service sessions (ERROR_NO_SUCH_LOGON_SESSION 1312), but DPAPI works
// across RDP/sshd/TaskScheduler sessions (spec 12 spike).
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
	// machine-scope 主路径(Plan 15:跨 logon session 可解)
	if mk, err := dpapiUnprotect(blob, true); err == nil {
		return mk, nil
	}
	// user-scope fallback(迁移窗口期:旧 master.key 是 user-scope)。
	// spike 2:flag 不强制隔离,但双 scope 尝试保证"无论旧 blob 哪个 scope 都能读出"。
	// 两个 scope 都失败则返回 machine-scope 的错误(下面再调一次取 err)。
	mk, err := dpapiUnprotect(blob, false)
	if err == nil {
		return mk, nil
	}
	// 都失败:重试 machine-scope 拿它的错误信息(machine-scope 是主路径,错误更相关)
	if mk2, err2 := dpapiUnprotect(blob, true); err2 == nil {
		return mk2, nil
	} else {
		return nil, err // 返回 user-scope 的 err(最后一个)
	}
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
	blob, err := dpapiProtect(mk, true) // machine-scope(Plan 15)
	if err != nil {
		return err
	}
	// ACL 契约(pi #3):temp 必须在 protectedDir(dir)内,继承 allan716-only ACL。
	// 严禁 os.TempDir()(那里继承宽 ACL,rename 后保留 → machine-scope 下全库失守)。
	tmp, err := os.CreateTemp(dir, ".master.key.tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
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
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	// Plan 15 T4: stamp the machine-scope sentinel sidecar. spike 2 proved DPAPI
	// blobs are cross-scope decryptable, so the only sound signal that this blob
	// was protected with the machine flag is a sidecar file. serve install's
	// precheck (verifyMachineScopeForBoot) reads this sentinel to refuse boot-
	// task installation against a legacy user-scope blob (which would crash-loop
	// at boot = FINDING B). Written AFTER the atomic rename so a crash mid-Set
	// leaves no sentinel pointing at a missing/old blob; a Set that fails before
	// rename leaves neither blob nor sentinel.
	return writeMachineScopeSentinel(path)
}

// writeMachineScopeSentinel writes the machine-scope sentinel sidecar next to
// the master.key file (Path + ".machinescope"). The sentinel is the ONLY sound
// signal of machine-scope DPAPI protection under spike 2 (Plan 15 T4). The
// content is a timestamp for debuggability; the presence of the file is what
// callers check.
func writeMachineScopeSentinel(masterKeyPath string) error {
	sentinelPath := masterKeyPath + ".machinescope"
	content := []byte("machine-scope DPAPI sentinel\nwritten-by: ssh-manager DpapiKeyProvider.Set\ntimestamp: " + time.Now().UTC().Format(time.RFC3339) + "\n")
	// Write with 0o600; the parent dir ACL (ensureDirACL, allan716-only) is the
	// real protection. Atomicity is best-effort (sentinel is a debug hint, not
	// a correctness-critical blob); a torn write would at worst make the precheck
	// reject, which is the safe direction.
	return os.WriteFile(sentinelPath, content, 0o600)
}

// removeMachineScopeSentinel removes the sentinel sidecar (best-effort; absent
// is a no-op). Called by Delete so a deleted master.key does not leave a stale
// sentinel that would mislead a future precheck into accepting a re-created
// user-scope blob at the same path.
func removeMachineScopeSentinel(masterKeyPath string) {
	_ = os.Remove(masterKeyPath + ".machinescope")
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
	// Plan 15 T4: also drop the machine-scope sentinel sidecar so a future
	// re-created user-scope blob at the same path can't ride on a stale
	// sentinel. Best-effort (absent is the desired end state).
	removeMachineScopeSentinel(path)
	return err
}

// MachineUnprotectForMigrate / UserUnprotectForMigrate / UserProtectForMigrate
// expose scope-specific protect/unprotect for the Plan 15 T3 user-scope →
// machine-scope migration logic (cli.migrateDpapiScope) + its tests. They are
// thin wrappers over the unexported dpapiProtect / dpapiUnprotect syscall
// helpers. NOT part of the KeyProvider interface (Get/Set only) — inspection /
// migration helpers only.
//
// CAUTION (spike 2, TestDpapi_CrossScopeInteroperable): DPAPI's flag is a
// hint, NOT a hard scope gate; a blob self-describes its scope and BOTH flags
// can unprotect it. So MachineUnprotectForMigrate succeeding does NOT prove the
// blob was machine-protected — it succeeds on user-protected blobs too.
// migrateDpapiScope must not treat "machine unprotect OK" as proof of
// already-machine-scope; see its doc comment for the actual decision rule.
func (p DpapiKeyProvider) MachineUnprotectForMigrate(blob []byte) ([]byte, error) {
	return dpapiUnprotect(blob, true)
}

// UserUnprotectForMigrate decrypts blob with the user-scope flag.
func (p DpapiKeyProvider) UserUnprotectForMigrate(blob []byte) ([]byte, error) {
	return dpapiUnprotect(blob, false)
}

// UserProtectForMigrate encrypts plain with the user-scope flag. Used by tests
// to synthesize a legacy user-scope master.key blob.
func (p DpapiKeyProvider) UserProtectForMigrate(plain []byte) ([]byte, error) {
	return dpapiProtect(plain, false)
}

// PathOrEmpty returns the master.key path this provider resolves to (the value
// of p.path()), or "" on error. Exported for migrateDpapiScope + serve-install
// precheck to locate master.key without re-deriving the path. Empty path means
// %AppData% is unset (production default unavailable).
func (p DpapiKeyProvider) PathOrEmpty() (string, error) {
	return p.path()
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
