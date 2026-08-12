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
	migrateSources = func() (m, d migrateSource) {
		return migrateSource{
				old: store.KeyringKeyProvider{Service: service, User: "master-key"},
				new: dpapiSeam,
			}, migrateSource{
				old: store.KeyringKeyProvider{Service: service, User: "cache-dek"},
				new: store.DpapiKeyProvider{Path: filepath.Join(dir, "cache-dek.key"), DirUser: os.Getenv("USERNAME")},
			}
	}
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
