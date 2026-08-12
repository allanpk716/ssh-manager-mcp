package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// staleLockSeconds is how old a backup lock file must be before a later run
// reclaims it. KB-scale vault + LAN NAS backup completes in seconds; 5 min is
// generous (a backup still running after 5 min means the NAS is hung — not worth
// waiting longer). Spec §3.8.
//
// NOTE: typed time.Duration, not an untyped int. The brief's original
// `const staleLockSeconds = 300` compared a Duration to an untyped 300, which
// Go coerces to 300*time.Nanosecond (Duration is int64 nanoseconds), not
// 300*time.Second — making every lock older than 300ns "stale" and defeating
// the ConcurrentSkip guard entirely.
const staleLockSeconds = 300 * time.Second

const backupLockName = ".ssh-manager-backup.lock"

// ErrConcurrentBackup is returned by acquireBackupLock when the lock is held by
// a still-running (non-stale) backup. Callers exit 0 with a "skipping" message
// rather than waiting or erroring.
var ErrConcurrentBackup = errors.New("another backup is in progress")

// backupLock is an O_EXCL advisory lock guarding `backup create` against
// concurrent runs. O_EXCL does NOT auto-release on process exit, so any crash
// (SIGKILL/OOM/panic) leaves an orphan; reclaimLogic is pure-timestamp (not
// pidAlive) — single-host single-mount deployment has no cross-machine contention.
type backupLock struct {
	path string
}

// acquireBackupLock creates an O_EXCL lock file containing the current unix ts.
// If the lock exists, it inspects the stored start-ts: older than staleLockSeconds
// => reclaim (steal); otherwise => ErrConcurrentBackup.
func acquireBackupLock(dir string) (*backupLock, error) {
	path := filepath.Join(dir, backupLockName)
	if lk, err := tryCreateLock(path); err == nil {
		return lk, nil
	} else if !errors.Is(err, os.ErrExist) {
		return nil, err
	}

	// Lock exists — read its start-ts.
	stale, err := lockIsStale(path)
	if err != nil {
		// unreadable / unparseable / raced away — one retry, then treat as concurrent.
		if lk, e2 := tryCreateLock(path); e2 == nil {
			return lk, nil
		}
		return nil, ErrConcurrentBackup
	}
	if !stale {
		return nil, ErrConcurrentBackup
	}
	// Stale: steal by removing then recreating. If the remove/recreate races
	// (another run beat us), fall back to ErrConcurrentBackup.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("reclaim stale lock %s: %w", path, err)
	}
	if lk, err := tryCreateLock(path); err == nil {
		return lk, nil
	}
	return nil, ErrConcurrentBackup
}

// tryCreateLock does the O_EXCL create + writes "<unix-ts>\n".
func tryCreateLock(path string) (*backupLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := f.WriteString(strconv.FormatInt(time.Now().Unix(), 10) + "\n"); err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return nil, err
	}
	return &backupLock{path: path}, nil
}

// lockIsStale reads the lock's start-ts and reports whether it exceeds
// staleLockSeconds. An unparseable/missing file returns an error so the caller
// can retry-create.
func lockIsStale(path string) (bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return false, err
	}
	return time.Since(time.Unix(ts, 0)) > staleLockSeconds, nil
}

// Release removes the lock file. Idempotent (ignores not-found).
func (l *backupLock) Release() {
	if l == nil || l.path == "" {
		return
	}
	os.Remove(l.path) // best-effort; ignore error
}
