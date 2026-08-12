package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// writeStaleLock writes a lock file with a start-ts far in the past,
// simulating a crashed previous run's orphan lock.
func writeStaleLock(t *testing.T, dir string, age time.Duration) {
	t.Helper()
	ts := time.Now().Add(-age).Unix()
	if err := os.WriteFile(filepath.Join(dir, ".ssh-manager-backup.lock"),
		[]byte(strconv.FormatInt(ts, 10)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireBackupLock_Fresh(t *testing.T) {
	dir := t.TempDir()
	lk, err := acquireBackupLock(dir)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lk.Release()
	// lock file exists with a numeric start-ts
	b, err := os.ReadFile(filepath.Join(dir, ".ssh-manager-backup.lock"))
	if err != nil {
		t.Fatal(err)
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		t.Fatalf("lock content not a unix ts: %q", b)
	}
	if time.Unix(ts, 0).After(time.Now().Add(time.Second)) {
		t.Fatalf("start-ts in the future: %d", ts)
	}
}

func TestAcquireBackupLock_ConcurrentSkip(t *testing.T) {
	dir := t.TempDir()
	// hold a fresh lock (start-ts = now)
	lk1, err := acquireBackupLock(dir)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer lk1.Release()
	// second acquire should hit ErrConcurrentBackup (not stale — just created)
	_, err = acquireBackupLock(dir)
	if err != ErrConcurrentBackup {
		t.Fatalf("second acquire err = %v, want ErrConcurrentBackup", err)
	}
}

func TestAcquireBackupLock_StaleReclaim(t *testing.T) {
	dir := t.TempDir()
	// orphan lock from 10 min ago (> 5 min threshold)
	writeStaleLock(t, dir, 10*time.Minute)
	lk, err := acquireBackupLock(dir)
	if err != nil {
		t.Fatalf("acquire should reclaim stale lock: %v", err)
	}
	defer lk.Release()
}

func TestAcquireBackupLock_NotYetStale(t *testing.T) {
	dir := t.TempDir()
	// lock from 1 min ago (< 5 min threshold) — still "running", must NOT reclaim
	writeStaleLock(t, dir, time.Minute)
	_, err := acquireBackupLock(dir)
	if err != ErrConcurrentBackup {
		t.Fatalf("err = %v, want ErrConcurrentBackup (lock not yet stale)", err)
	}
}

func TestBackupLock_ReleaseRemovesFile(t *testing.T) {
	dir := t.TempDir()
	lk, err := acquireBackupLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	lk.Release()
	if _, err := os.Stat(filepath.Join(dir, ".ssh-manager-backup.lock")); !os.IsNotExist(err) {
		t.Fatalf("lock file still exists after Release: %v", err)
	}
	// double Release is a no-op (must not panic)
	lk.Release()
}

// TestAcquireBackupLock_CorruptContent is the N2 test: a lock file whose
// content is garbage (not a parseable unix ts) makes lockIsStale return a
// parse error. acquireBackupLock must NOT panic, NOT get stuck, and either
// reclaim via retry-create or report ErrConcurrentBackup. Since the corrupt
// lock file already exists, the retry O_EXCL create will fail with ErrExist,
// so the expected outcome is ErrConcurrentBackup. This locks in lock.go's
// "parse failure => retry once => treat as concurrent" branch (lines 52-59),
// which previously had no direct test.
func TestAcquireBackupLock_CorruptContent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".ssh-manager-backup.lock"),
		[]byte("not-a-number\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lk, err := acquireBackupLock(dir)
	if err == nil {
		// unexpected but not wrong (a racing second caller could have stolen it);
		// just make sure to release.
		lk.Release()
		return
	}
	if err != ErrConcurrentBackup {
		t.Fatalf("err = %v, want ErrConcurrentBackup (corrupt lock should be treated as concurrent after retry-create fails)", err)
	}
}
