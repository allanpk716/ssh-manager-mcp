//go:build windows

package cli

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"ssh-manager-mcp/internal/store"
)

// mu serializes tests that touch the real Windows Credential Manager (the
// keyring backend is process-global; parallel access to the same service/user
// slot races). The tests use a unique service name per run, but the mutex
// keeps the confirm-prompt + migrateSources swap + keychain writes atomic
// against the other tests in this file.
var mu sync.Mutex

// testServiceBase is a fixed prefix for all keychain slots created by these
// tests, so they can be identified and swept post-test (defensive — each test
// also defers its own cleanup). Never the production "ssh-manager" /
// "ssh-manager-eval" names.
const testServiceBase = "ssh-manager-test-migrate"

// uniqueService returns a process-unique service name so concurrent test
// processes don't collide in the host keychain.
func uniqueService(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%s-%x", testServiceBase, b)
}

// sweepSlot deletes a keychain slot if it exists (best-effort test cleanup).
func sweepSlot(t *testing.T, kp store.KeyProvider) {
	t.Helper()
	type deleter interface{ Delete() error }
	if d, ok := kp.(deleter); ok {
		_ = d.Delete()
	}
}

// freshMigrateState swaps migrateSources + confirmMigratePrompt + keychain for
// the test, pointing at a temp dir + unique keychain service. Returns the
// master-slot / dek-slot legacy KeyringKeyProviders (to pre-seed + clean up)
// and the master-file / dek-file DPAPI destinations (to assert post-migrate).
func freshMigrateState(t *testing.T, service, dir string) (masterOld, dekOld store.KeyringKeyProvider, masterNew, dekNew store.DpapiKeyProvider) {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	masterOld = store.KeyringKeyProvider{Service: service, User: "master-key"}
	dekOld = store.KeyringKeyProvider{Service: service, User: "cache-dek"}
	masterNew = store.DpapiKeyProvider{Path: filepath.Join(dir, "master.key"), DirUser: os.Getenv("USERNAME")}
	dekNew = store.DpapiKeyProvider{Path: filepath.Join(dir, "cache-dek.key"), DirUser: os.Getenv("USERNAME")}

	// Default keychain seam → return ErrNotFound (so unlock hits the first-run
	// branch and invokes the migrator). Using a KeyringKeyProvider on a unique
	// service guarantees it's absent → ErrNotFound.
	prevKc := keychain
	keychain = store.KeyringKeyProvider{Service: service + "-absent"}
	t.Cleanup(func() { keychain = prevKc })

	prevSrc := migrateSources
	migrateSources = func() (m, d migrateSource) {
		return migrateSource{old: masterOld, new: masterNew}, migrateSource{old: dekOld, new: dekNew}
	}
	t.Cleanup(func() { migrateSources = prevSrc })

	// Always sweep both legacy slots on test exit (covers pre-seeded + any
	// partial-migrate leftovers). Idempotent; ignores ErrNotFound.
	t.Cleanup(func() {
		sweepSlot(t, masterOld)
		sweepSlot(t, dekOld)
	})
	return masterOld, dekOld, masterNew, dekNew
}

// setConfirm overrides confirmMigrate for the test. Each call to the
// prompt records the label so tests can assert which key was being confirmed.
func setConfirm(t *testing.T, accept bool) *[]string {
	t.Helper()
	seen := &[]string{}
	prev := confirmMigrate
	confirmMigrate = func(w interface{ Write([]byte) (int, error) }, label string) bool {
		*seen = append(*seen, label)
		return accept
	}
	t.Cleanup(func() { confirmMigrate = prev })
	return seen
}

// runUnlock executes `unlock` against a fresh root and returns (stdout, stderr,
// err). All migration prompts/messages go to stderr; stdout stays clean for
// the export line.
func runUnlock(t *testing.T) (stdout, stderr string, err error) {
	t.Helper()
	root := NewRootCmd()
	outBuf, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	root.SetOut(outBuf)
	root.SetErr(errBuf)
	root.SetArgs([]string{"unlock"})
	execErr := root.Execute()
	return outBuf.String(), errBuf.String(), execErr
}

// TestMigrate_MasterKey_LegacySlotMigrated is the happy path: a v0.2.0 vault
// has its master key in the legacy keychain slot; `unlock` (interactive) reads
// it, the user accepts the prompt, and the key lands in the DPAPI file with
// the legacy slot deleted.
func TestMigrate_MasterKey_LegacySlotMigrated(t *testing.T) {
	service := uniqueService(t)
	dir := t.TempDir()
	masterOld, dekOld, masterNew, _ := freshMigrateState(t, service, dir)

	want := make([]byte, 32)
	if _, err := rand.Read(want); err != nil {
		t.Fatal(err)
	}
	if err := masterOld.Set(want); err != nil {
		t.Fatalf("seed legacy master slot: %v", err)
	}
	setConfirm(t, true)

	stdout, stderr, err := runUnlock(t)
	if err != nil {
		t.Fatalf("unlock: %v (stderr=%q)", err, stderr)
	}
	// DPAPI file now holds the migrated key.
	got, err := masterNew.Get()
	if err != nil {
		t.Fatalf("masterNew.Get: %v", err)
	}
	if !bytesEqual(got, want) {
		t.Fatalf("migrated key mismatch: got %x want %x", got, want)
	}
	// Legacy master slot deleted.
	if _, err := masterOld.Get(); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("legacy master slot still present after migrate: err=%v", err)
	}
	// stdout carries the export line with the SAME key.
	if !strings.Contains(stdout, hex.EncodeToString(want)) {
		t.Fatalf("stdout missing migrated key hex: %q", stdout)
	}
	// cache DEK slot was absent → no second prompt fired; no cache-dek.key file.
	if _, statErr := os.Stat(filepath.Join(dir, "cache-dek.key")); !os.IsNotExist(statErr) {
		t.Fatalf("cache-dek.key should not exist (no legacy DEK slot): %v", statErr)
	}
	_ = dekOld
}

// TestMigrate_MasterKey_Decline_NoGenerate asserts the safety property: if
// the user declines the migration prompt, unlock MUST NOT first-run generate a
// fresh key (that would orphan the legacy vault behind a new DPAPI file).
// Instead it stops cleanly with remediation guidance on stderr.
func TestMigrate_MasterKey_Decline_NoGenerate(t *testing.T) {
	service := uniqueService(t)
	dir := t.TempDir()
	masterOld, _, masterNew, _ := freshMigrateState(t, service, dir)

	want := make([]byte, 32)
	if _, err := rand.Read(want); err != nil {
		t.Fatal(err)
	}
	if err := masterOld.Set(want); err != nil {
		t.Fatalf("seed legacy master slot: %v", err)
	}
	setConfirm(t, false)

	stdout, stderr, err := runUnlock(t)
	if err != nil {
		t.Fatalf("unlock should not hard-error on declined migration: %v", err)
	}
	// No DPAPI file created (would mask the legacy slot).
	if _, gErr := masterNew.Get(); !errors.Is(gErr, store.ErrNotFound) {
		t.Fatalf("master.key created despite declined migration: err=%v", gErr)
	}
	// Legacy slot untouched (still readable).
	got, err := masterOld.Get()
	if err != nil {
		t.Fatalf("legacy slot should still be readable after decline: %v", err)
	}
	if !bytesEqual(got, want) {
		t.Fatalf("legacy key changed after decline: got %x want %x", got, want)
	}
	// No export line printed.
	if strings.Contains(stdout, "export SSHMGR_MASTERKEY_HEX") {
		t.Fatalf("stdout should not contain export line after decline: %q", stdout)
	}
	// Remediation guidance printed.
	if !strings.Contains(stderr, "declined") {
		t.Fatalf("stderr missing decline guidance: %q", stderr)
	}
}

// TestMigrate_MasterKey_Unreadable_NoGenerate simulates the sshd / Service
// session: the legacy slot EXISTS but can't be read (Credential Manager
// returns ERROR_NO_SUCH_LOGON_SESSION 1312). The migrator must surface a clear
// "rerun in an interactive session" message and MUST NOT generate a fresh key
// (the old vault is still behind the unreadable slot). unlock returns nil (the
// guidance is on stderr); it must NOT print an export line.
func TestMigrate_MasterKey_Unreadable_NoGenerate(t *testing.T) {
	dir := t.TempDir()
	prevSrc := migrateSources
	masterNew := store.DpapiKeyProvider{Path: filepath.Join(dir, "master.key"), DirUser: os.Getenv("USERNAME")}
	dekNew := store.DpapiKeyProvider{Path: filepath.Join(dir, "cache-dek.key"), DirUser: os.Getenv("USERNAME")}
	migrateSources = func() (m, d migrateSource) {
		return migrateSource{old: &failingGetProvider{err: errors.New("wincred error 1312: ERROR_NO_SUCH_LOGON_SESSION")}, new: masterNew},
			migrateSource{old: &failingGetProvider{err: store.ErrNotFound}, new: dekNew} // cache DEK absent
	}
	t.Cleanup(func() { migrateSources = prevSrc })

	prevKc := keychain
	keychain = &failingGetProvider{err: store.ErrNotFound} // ErrNotFound → first-run branch
	t.Cleanup(func() { keychain = prevKc })

	setConfirm(t, true) // would accept; prompt must NOT fire (Get failed before it)

	stdout, stderr, err := runUnlock(t)
	if err != nil {
		t.Fatalf("unlock should not hard-error on unreadable legacy slot: %v", err)
	}
	// Crucially: NO master.key generated (would orphan the old vault).
	if _, gErr := masterNew.Get(); !errors.Is(gErr, store.ErrNotFound) {
		t.Fatalf("master.key generated despite unreadable legacy slot: err=%v", gErr)
	}
	// Guidance printed.
	if !strings.Contains(stderr, "interactive session") && !strings.Contains(stderr, "RDP") {
		t.Fatalf("stderr missing interactive-session guidance: %q", stderr)
	}
	// A generated key would have printed an export line; assert silence.
	if strings.Contains(stdout, "export SSHMGR_MASTERKEY_HEX") {
		t.Fatalf("unlock generated + printed a key despite unreadable legacy slot: %q", stdout)
	}
}

// TestMigrate_CleanEnv_FirstRunGenerate is the no-legacy baseline: no legacy
// slots, no DPAPI file → unlock first-run generates a fresh master key and
// persists it via the keychain seam (DpapiKeyProvider in the test).
func TestMigrate_CleanEnv_FirstRunGenerate(t *testing.T) {
	service := uniqueService(t)
	dir := t.TempDir()
	// Use a DpapiKeyProvider as the keychain seam so first-run generate writes
	// a real DPAPI file we can read back.
	dpapiSeam := store.DpapiKeyProvider{Path: filepath.Join(dir, "master.key"), DirUser: os.Getenv("USERNAME")}
	prevKc := keychain
	keychain = dpapiSeam
	t.Cleanup(func() { keychain = prevKc })

	// Point migrateSources at a UNIQUE service whose slots are absent → migrator
	// finds nothing to migrate → unlock proceeds to first-run generate.
	prevSrc := migrateSources
	migrateSources = func() (m, d migrateSource) {
		return migrateSource{
				old: store.KeyringKeyProvider{Service: service, User: "master-key"},
				new: dpapiSeam,
			}, migrateSource{
				old: store.KeyringKeyProvider{Service: service, User: "cache-dek"},
				new: store.DpapiKeyProvider{Path: filepath.Join(dir, "cache-dek.key"), DirUser: os.Getenv("USERNAME")},
			}
	}
	t.Cleanup(func() { migrateSources = prevSrc })
	// Prompt must not fire in the clean case.
	prompted := setConfirm(t, true)
	t.Cleanup(func() {
		if len(*prompted) != 0 {
			t.Fatalf("confirm prompt fired in clean env: %v", *prompted)
		}
	})

	stdout, _, err := runUnlock(t)
	if err != nil {
		t.Fatalf("unlock: %v", err)
	}
	// DPAPI file persisted + readable.
	mk, err := dpapiSeam.Get()
	if err != nil {
		t.Fatalf("seam.Get after first-run: %v", err)
	}
	if len(mk) != 32 {
		t.Fatalf("generated key length = %d, want 32", len(mk))
	}
	if !strings.Contains(stdout, hex.EncodeToString(mk)) {
		t.Fatalf("stdout missing generated key hex: %q", stdout)
	}
}

// TestMigrate_BothKeys_MasterAndDEKMigrated verifies the master + cache DEK
// migrate together when both legacy slots are present (spec §5.7 "unlock
// handles both"). Both land in separate DPAPI files; both legacy slots deleted.
func TestMigrate_BothKeys_MasterAndDEKMigrated(t *testing.T) {
	service := uniqueService(t)
	dir := t.TempDir()
	masterOld, dekOld, masterNew, dekNew := freshMigrateState(t, service, dir)

	wantMK := make([]byte, 32)
	wantDEK := make([]byte, 32)
	if _, err := rand.Read(wantMK); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(wantDEK); err != nil {
		t.Fatal(err)
	}
	if err := masterOld.Set(wantMK); err != nil {
		t.Fatalf("seed legacy master: %v", err)
	}
	if err := dekOld.Set(wantDEK); err != nil {
		t.Fatalf("seed legacy DEK: %v", err)
	}
	seen := setConfirm(t, true)

	_, stderr, err := runUnlock(t)
	if err != nil {
		t.Fatalf("unlock: %v (stderr=%q)", err, stderr)
	}
	// Both DPAPI files hold the migrated keys.
	gotMK, err := masterNew.Get()
	if err != nil {
		t.Fatalf("masterNew.Get: %v", err)
	}
	if !bytesEqual(gotMK, wantMK) {
		t.Fatalf("master mismatch: got %x want %x", gotMK, wantMK)
	}
	gotDEK, err := dekNew.Get()
	if err != nil {
		t.Fatalf("dekNew.Get: %v", err)
	}
	if !bytesEqual(gotDEK, wantDEK) {
		t.Fatalf("DEK mismatch: got %x want %x", gotDEK, wantDEK)
	}
	// Both legacy slots deleted.
	if _, err := masterOld.Get(); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("legacy master slot still present: %v", err)
	}
	if _, err := dekOld.Get(); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("legacy DEK slot still present: %v", err)
	}
	// Confirm fired twice (master + DEK).
	if len(*seen) != 2 {
		t.Fatalf("confirm prompt fired %d times, want 2 (master + DEK): %v", len(*seen), *seen)
	}
}

// TestMigrate_MasterDeclined_SkipsDEKPrompt verifies the "one declination
// governs both" property: when the user declines the MASTER migration, the
// migrator must NOT prompt for the cache DEK even if that slot is present
// (a partial migrate — DEK moved, master left — would be confusing + risks
// masking the legacy DEK behind a fresh DPAPI file the user didn't sanction).
// The master Declined outcome routes to firstRunStop; the DEK migrate is gated
// behind (Done || Absent) only.
func TestMigrate_MasterDeclined_SkipsDEKPrompt(t *testing.T) {
	service := uniqueService(t)
	dir := t.TempDir()
	masterOld, dekOld, masterNew, dekNew := freshMigrateState(t, service, dir)

	wantMK := make([]byte, 32)
	wantDEK := make([]byte, 32)
	if _, err := rand.Read(wantMK); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(wantDEK); err != nil {
		t.Fatal(err)
	}
	if err := masterOld.Set(wantMK); err != nil {
		t.Fatal(err)
	}
	if err := dekOld.Set(wantDEK); err != nil {
		t.Fatal(err)
	}
	seen := setConfirm(t, false) // decline master

	_, _, err := runUnlock(t)
	if err != nil {
		t.Fatalf("unlock should not hard-error on decline: %v", err)
	}
	// Only ONE prompt fired (master); the DEK migrate was gated off by the
	// Declined master outcome.
	if len(*seen) != 1 {
		t.Fatalf("confirm fired %d times, want 1 (master declination should suppress DEK prompt): %v", len(*seen), *seen)
	}
	// Neither DPAPI file created.
	if _, gErr := masterNew.Get(); !errors.Is(gErr, store.ErrNotFound) {
		t.Fatalf("master.key created despite decline: %v", gErr)
	}
	if _, gErr := dekNew.Get(); !errors.Is(gErr, store.ErrNotFound) {
		t.Fatalf("cache-dek.key created despite master decline: %v", gErr)
	}
	// Both legacy slots untouched.
	if got, gErr := masterOld.Get(); gErr != nil || !bytesEqual(got, wantMK) {
		t.Fatalf("legacy master changed after decline: got=%x err=%v", got, gErr)
	}
	if got, gErr := dekOld.Get(); gErr != nil || !bytesEqual(got, wantDEK) {
		t.Fatalf("legacy DEK changed after master decline: got=%x err=%v", got, gErr)
	}
}

// failingGetProvider is a KeyProvider whose Get always returns err (non-nil,
// non-ErrNotFound unless err IS store.ErrNotFound). Used to simulate the sshd
// 1312 read failure on the legacy keychain slot.
type failingGetProvider struct {
	err error
}

func (f *failingGetProvider) Get() ([]byte, error) { return nil, f.err }
func (f *failingGetProvider) Set([]byte) error     { return nil }
func (f *failingGetProvider) Delete() error        { return nil }

// TestMigrate_DEKFile_ReusedByLoadOrCreateDEK is the F1 regression guard (Plan
// 14 T5 review F2). Before the fix, the v0.2.0 cache-DEK migration was
// WRITE-ONLY: migrate_windows.go wrote cache-dek.key (DpapiKeyProvider), but
// dekProvider (cache.go) still returned a KeyringKeyProvider — so the next
// loadOrCreateDEK() consulted the (deleted) keychain slot, hit ErrNotFound, and
// generated a FRESH DEK, orphaning the migrated file. This test forces the bug
// to stay fixed: after migration, the production dekProvider seam (DpapiKeyProvider
// at the migration's cache-dek.key path) MUST return the migrated DEK, not a
// newly-generated one.
//
// The test mirrors the production shape: dekProvider is swapped to a
// DpapiKeyProvider pointed at the SAME cache-dek.key path the migration writes.
// A path mismatch (write A, read B) is exactly the F1 bug — and is now
// impossible by construction because both sides go through dpapiCacheDekPath().
// The test pins that contract: change either side to a different path and this
// test fails.
func TestMigrate_DEKFile_ReusedByLoadOrCreateDEK(t *testing.T) {
	service := uniqueService(t)
	dir := t.TempDir()
	masterOld, dekOld, _, dekNew := freshMigrateState(t, service, dir)

	// Seed a KNOWN cache DEK in the legacy keychain slot. Distinctive pattern
	// (0xAB repeated) — a freshly-generated 32-byte DEK won't match it.
	wantDEK := make([]byte, 32)
	for i := range wantDEK {
		wantDEK[i] = byte(0xAB)
	}
	if err := dekOld.Set(wantDEK); err != nil {
		t.Fatalf("seed legacy DEK slot: %v", err)
	}

	// Also seed a master-key legacy slot so the master migrate outcome is Done
	// (which gates the DEK migrate on green light — Done || Absent).
	masterMK := make([]byte, 32)
	if _, err := rand.Read(masterMK); err != nil {
		t.Fatal(err)
	}
	if err := masterOld.Set(masterMK); err != nil {
		t.Fatalf("seed legacy master slot: %v", err)
	}

	setConfirm(t, true) // accept both migrate prompts (master + DEK)

	if _, _, err := runUnlock(t); err != nil {
		t.Fatalf("unlock: %v", err)
	}

	// Migration wrote cache-dek.key at dekNew.Path. Verify.
	gotMigrated, err := dekNew.Get()
	if err != nil {
		t.Fatalf("dekNew.Get after migrate: %v", err)
	}
	if !bytesEqual(gotMigrated, wantDEK) {
		t.Fatalf("migrated DEK file does not hold the legacy DEK: got %x want %x", gotMigrated, wantDEK)
	}

	// THE F1 REGRESSION CHECK: swap dekProvider to the PRODUCTION shape — a
	// DpapiKeyProvider at the SAME path the migration just wrote (this is exactly
	// what cache_dek_windows.go's default seam does, just in a temp dir). Then
	// loadOrCreateDEK() must return the MIGRATED DEK, not ErrNotFound→generate.
	prevDekProvider := dekProvider
	dekProvider = func() store.KeyProvider {
		return &store.DpapiKeyProvider{Path: dekNew.Path, DirUser: os.Getenv("USERNAME")}
	}
	t.Cleanup(func() { dekProvider = prevDekProvider })

	gotDEK, err := loadOrCreateDEK()
	if err != nil {
		t.Fatalf("loadOrCreateDEK after migrate: %v", err)
	}
	if !bytesEqual(gotDEK, wantDEK) {
		t.Fatalf("F1 REGRESSION: loadOrCreateDEK did not reuse the migrated DEK file — got %x want %x (a fresh DEK was generated, meaning dekProvider reads a different path than the migration writes)", gotDEK, wantDEK)
	}

	// Legacy keychain slot MUST be deleted post-migrate (otherwise a future
	// reader that still consulted the slot would silently resurrect an old DEK).
	if _, err := dekOld.Get(); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("legacy DEK slot not deleted after migrate: err=%v", err)
	}
}

// TestMigrate_DEKFile_PathContractBetweenMigratorAndReader is the FIRST line of
// defense against the F1 regression (Plan 14 T5 review F1). It asserts — with
// NO migration, NO keychain, NO DPAPI file, just the production defaults — that
// the cache-DEK provider seam (cache_dek_windows.go's dekProvider) and the
// migration's DEK destination (migrate_windows.go's migrateSources) resolve to
// the SAME cache-dek.key path. Before the fix, the migrator wrote
// DpapiKeyProvider{Path: <dir>/cache-dek.key} but the reader returned
// KeyringKeyProvider{...} (no path at all → keychain) → the migrated file was
// orphaned. This test pins the contract: if either side drifts to a different
// path (or a different medium), it fails immediately.
//
// It uses the production dekProvider + migrateSources AS-IS (no overrides), so
// a future change that reverts the seam to a keychain slot WILL fail here.
func TestMigrate_DEKFile_PathContractBetweenMigratorAndReader(t *testing.T) {
	// dpapiPath extracts the Path from a DpapiKeyProvider whether the seam
	// returned it by value or by pointer (both are valid — methods are
	// value-receiver). Returns ok=false for any other type (the F1 bug shape:
	// a KeyringKeyProvider has no Path at all).
	dpapiPath := func(kp store.KeyProvider) (string, bool) {
		switch dp := kp.(type) {
		case *store.DpapiKeyProvider:
			return dp.Path, true
		case store.DpapiKeyProvider:
			return dp.Path, true
		}
		return "", false
	}

	// Reader side: production dekProvider seam. The seam MUST be a DpapiKeyProvider
	// (not a KeyringKeyProvider) whose Path matches the migration destination.
	reader := dekProvider()
	readerPath, ok := dpapiPath(reader)
	if !ok {
		t.Fatalf("F1 REGRESSION: production dekProvider is %T, want a DpapiKeyProvider (cache_dek_windows.go must bind a DpapiKeyProvider so the migrated cache-dek.key is actually read back — a KeyringKeyProvider reads the keychain, not the file, orphaning the migrated DEK)", reader)
	}
	if readerPath == "" {
		t.Fatalf("F1 REGRESSION: production dekProvider DpapiKeyProvider.Path is empty (would fall back to master.key default — wrong file)")
	}

	// Writer side: production migrateSources DEK destination.
	_, dek := migrateSources()
	writerPath, ok := dpapiPath(dek.new)
	if !ok {
		t.Fatalf("migrateSources DEK destination is %T, want a DpapiKeyProvider", dek.new)
	}

	// THE F1 CONTRACT: reader path == writer path.
	if readerPath != writerPath {
		t.Fatalf("F1 REGRESSION: dekProvider reads %q but migrateSources writes %q — the migrated DEK would be orphaned (write-only migration, the original F1 bug)", readerPath, writerPath)
	}

	// And both must resolve via the shared helper (dpapiCacheDekPath), so a
	// future drift in either side is impossible by construction.
	if readerPath != dpapiCacheDekPath() {
		t.Fatalf("F1 REGRESSION: dekProvider Path %q != dpapiCacheDekPath() %q — reader is not using the shared path helper", readerPath, dpapiCacheDekPath())
	}
	if writerPath != dpapiCacheDekPath() {
		t.Fatalf("F1 REGRESSION: migrateSources DEK Path %q != dpapiCacheDekPath() %q — writer is not using the shared path helper", writerPath, dpapiCacheDekPath())
	}
}

// bytesEqual is a local equal (no generics needed).
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestUnlock_MigratesUserScopeToMachineScope 验 unlock 在 Get 成功后触发
// postGetMigrator,把旧 user-scope master.key 重 protect 为 machine-scope。
// 钉死 codex #1:没有 postGetMigrator 钩子则迁移不可达(firstRunMigrator 只在
// ErrNotFound 触发,而双 scope Get 对旧 user-scope blob 返回 success)。
//
// 严正性:Plan 15 spike 2 证明 DPAPI flag 不隔离 scope(blob 自描述),
// dpapiUnprotect(userBlob, machineFlag) 同样成功 —— 所以仅靠 MachineUnprotectForMigrate
// 能否解出无法判定 "已 machine-scope vs 待迁移 user-scope"。测试因此断言两件事:
//   - (A) 重 protect 后 blob 仍解得出原 mk(简报基本断言,spike-2 下可能假通过)。
//   - (B) blob 字节确实被改写(Set 会用 DPAPI 非确定性 IV 重 protect,字节必变)——
//     这是判定 "Set 真的跑了" 的硬证据,不被 spike-2 互通欺骗。
//   - (C) stderr 含迁移成功消息 + confirm 提示真的触发了 —— 证明走了迁移分支。
//
// 若 postGetMigrator 未实现 / 被 spike-2 短路:旧 blob 不被改写,字节相同 → B 失败。
func TestUnlock_MigratesUserScopeToMachineScope(t *testing.T) {
	mu.Lock()
	defer mu.Unlock()

	dir := t.TempDir()
	masterPath := filepath.Join(dir, "master.key")
	user := os.Getenv("USERNAME")
	if user == "" {
		t.Skip("USERNAME empty (ACL setup needs a real user)")
	}
	originalMK := []byte("user-scope-migrate-test-key32-pad00")[:32]

	// 写旧 user-scope master.key(用 Step 4 加的 UserProtectForMigrate 导出 helper)。
	legacyBlob, err := store.DpapiKeyProvider{}.UserProtectForMigrate(originalMK)
	if err != nil {
		t.Fatalf("UserProtectForMigrate: %v", err)
	}
	if err := os.WriteFile(masterPath, legacyBlob, 0o600); err != nil {
		t.Fatal(err)
	}

	// keychain seam 指向这个 master.key;Get 成功 → postGetMigrator 触发。
	masterProv := store.DpapiKeyProvider{Path: masterPath, DirUser: user}
	prevKc := keychain
	keychain = masterProv
	t.Cleanup(func() { keychain = prevKc })

	// migrateSources 也指向同一 master.key(否则 migrateDpapiScope 找不到路径)。
	// cache-dek 指向另一个空路径(该文件不存在 → migrateKeyProvider 走 Absent,
	// 不触发 prompt,不干扰 master 迁移断言)。
	prevSrc := migrateSources
	migrateSources = func() (m, d migrateSource) {
		return migrateSource{
				old: store.KeyringKeyProvider{Service: uniqueService(t), User: "master-key"}, // absent
				new: masterProv,
			}, migrateSource{
				old: store.KeyringKeyProvider{Service: uniqueService(t), User: "cache-dek"}, // absent
				new: store.DpapiKeyProvider{Path: filepath.Join(dir, "cache-dek.key"), DirUser: user},
			}
	}
	t.Cleanup(func() { migrateSources = prevSrc })

	// 接受迁移 prompt。
	seen := setConfirm(t, true)

	// 跑 unlock(postGetMigrator 读 master.key 发现是 user-scope,
	// confirmMigrate 返回 true → 重 protect 为 machine-scope)。
	stdout, stderr, err := runUnlock(t)
	if err != nil {
		t.Fatalf("unlock: %v (stderr=%q)", err, stderr)
	}

	// (A) 新 blob 解出原 mk(spike-2 下可能假通过)。
	newBlob, err := os.ReadFile(masterPath)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.DpapiKeyProvider{}.MachineUnprotectForMigrate(newBlob)
	if err != nil {
		t.Fatalf("重 protect 后 machine-scope unprotect 失败: %v", err)
	}
	if !bytesEqual(got, originalMK) {
		t.Fatalf("迁移后 key 变了: got %x want %x", got, originalMK)
	}

	// (B) blob 字节被改写 —— 证明 Set 真的跑了(DPAPI 非确定性 IV → 必变)。
	// 若 postGetMigrator 被 spike-2 短路,Set 没跑,旧 blob 原样 → 这条会失败。
	if bytesEqual(newBlob, legacyBlob) {
		t.Fatalf("master.key blob 未被改写:迁移未触发(spike-2 短路或 postGetMigrator 未调 Set)。legacy=%x new=%x", legacyBlob, newBlob)
	}

	// (C) stderr 含迁移成功消息 + confirm 真的触发了(证明走了迁移分支,不是短路)。
	if !strings.Contains(stderr, "user-scope to machine-scope") {
		t.Fatalf("stderr 缺迁移成功消息: %q", stderr)
	}
	if len(*seen) == 0 {
		t.Fatalf("confirm prompt 未触发(迁移分支未到达): seen=%v", *seen)
	}

	// stdout 含原 mk 的 export 行。
	if !strings.Contains(stdout, hex.EncodeToString(originalMK)) {
		t.Fatalf("stdout 缺 mk export 行: %q", stdout)
	}
}
