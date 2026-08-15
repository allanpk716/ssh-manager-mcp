package tui

import (
	"os"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/store"
)

// Tri-state probe tests (real filesystem): absent / locked / unlocked vault.
// SSHMGR_FILEKEY_PATH is also pinned per-test so a real master.key.plain on
// the dev machine (C:\ProgramData\ssh-manager) can never leak into the probe.
// (DetectMode itself was deleted in Plan 20 T1 — roles.ResolveMode owns mode
// resolution; these tests pin the probe helpers the tests still use.)

func TestVaultProbe_NoStoreFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SSHMGR_STORE", filepath.Join(dir, "store.db"))
	t.Setenv("SSHMGR_FILEKEY_PATH", filepath.Join(dir, "master.key.plain"))
	if vaultExists() {
		t.Fatal("vaultExists must be false when no store.db file exists")
	}
	if vaultUnlocked() {
		t.Fatal("vaultUnlocked must be false when no store.db file exists")
	}
	// And probing must NOT have created one (stat-first, no OpenStore side effect).
	if _, err := os.Stat(filepath.Join(dir, "store.db")); !os.IsNotExist(err) {
		t.Fatalf("probe must not create store.db, stat err = %v", err)
	}
}

func TestVaultProbe_LockedStore(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SSHMGR_STORE", filepath.Join(dir, "store.db"))
	// No master.key.plain at the pinned path → FileKeyProvider.Get fails → locked.
	t.Setenv("SSHMGR_FILEKEY_PATH", filepath.Join(dir, "master.key.plain"))
	if err := os.WriteFile(filepath.Join(dir, "store.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !vaultExists() {
		t.Fatal("vaultExists must be true when store.db exists")
	}
	if vaultUnlocked() {
		t.Fatal("vaultUnlocked must be false for a store with no readable master key")
	}
}

func TestVaultProbe_UnlockedStore(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "store.db")
	keyPath := filepath.Join(dir, "master.key.plain")
	t.Setenv("SSHMGR_STORE", dbPath)
	t.Setenv("SSHMGR_FILEKEY_PATH", keyPath)
	t.Setenv("SSHMGR_CACHE_DIR", t.TempDir())

	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, mk, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dbPath, mk)
	if err != nil {
		t.Fatalf("seed unlocked vault: %v", err)
	}
	st.Close()

	if !vaultExists() || !vaultUnlocked() {
		t.Fatalf("unlocked vault: exists=%v unlocked=%v, want true/true", vaultExists(), vaultUnlocked())
	}
}
