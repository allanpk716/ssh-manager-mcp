package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/store"
)

// vaultProbe and cacheProbe are injectable for tests (production: real paths).
func TestDetectMode_ForceWins(t *testing.T) {
	for _, c := range []struct{ force string; want Mode }{
		{"broker", ModeBroker}, {"client", ModeClient},
	} {
		got, err := DetectModeWith(c.force, func() bool { return false }, func() bool { return false })
		if err != nil || got != c.want {
			t.Fatalf("force=%q: got (%v,%v)", c.force, got, err)
		}
	}
}

func TestDetectMode_Auto(t *testing.T) {
	// vault present → broker
	if m, err := DetectModeWith("", func() bool { return true }, func() bool { return false }); err != nil || m != ModeBroker {
		t.Fatalf("vault: (%v,%v)", m, err)
	}
	// no vault + cache → client
	if m, err := DetectModeWith("", func() bool { return false }, func() bool { return true }); err != nil || m != ModeClient {
		t.Fatalf("cache: (%v,%v)", m, err)
	}
	// neither → guided error
	if _, err := DetectModeWith("", func() bool { return false }, func() bool { return false }); err == nil {
		t.Fatal("neither vault nor cache must error with guidance")
	}
}

// Tri-state probe tests (real filesystem): absent / locked / unlocked vault.
// SSHMGR_FILEKEY_PATH is also pinned per-test so a real master.key.plain on
// the dev machine (C:\ProgramData\ssh-manager) can never leak into the probe.

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

func TestVaultProbe_LockedStore_NeverClientMode(t *testing.T) {
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
	// A stale cache.auth.json must NOT degrade a locked broker into client mode.
	cacheDir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", cacheDir)
	if err := os.WriteFile(filepath.Join(cacheDir, "cache.auth.json"), []byte(`{"url":"https://x","token":"t"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	mode, err := DetectMode("")
	if err == nil || !strings.Contains(err.Error(), "unlock") {
		t.Fatalf("DetectMode on locked vault: err = %v, want error mentioning unlock", err)
	}
	if mode == ModeClient {
		t.Fatal("locked vault must never degrade to ModeClient")
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
	if m, err := DetectMode(""); err != nil || m != ModeBroker {
		t.Fatalf("DetectMode on unlocked vault: (%v,%v), want ModeBroker", m, err)
	}
}
