package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestVaultDir_FixedPath(t *testing.T) {
	t.Setenv("SSHMGR_STORE", "")
	t.Setenv("SSHMGR_FILEKEY_PATH", "")
	got, err := VaultDir()
	if err != nil {
		t.Fatalf("VaultDir: %v", err)
	}
	want := winOrUnix("C:\\ProgramData\\ssh-manager", "/var/lib/ssh-manager")
	if got != want {
		t.Errorf("VaultDir = %q, want %q", got, want)
	}
}

func TestStorePath_EnvOverride(t *testing.T) {
	t.Setenv("SSHMGR_STORE", "/tmp/custom/store.db")
	got, err := StorePath()
	if err != nil || got != "/tmp/custom/store.db" {
		t.Errorf("StorePath = %q,%v; want env override", got, err)
	}
}

func TestMasterKeyPath_NoEnvLandsInVaultDir(t *testing.T) {
	t.Setenv("SSHMGR_FILEKEY_PATH", "")
	got, _ := MasterKeyPath()
	dir, _ := VaultDir()
	want := filepath.Join(dir, "master.key.plain")
	if got != want {
		t.Errorf("MasterKeyPath = %q, want %q", got, want)
	}
}

func TestCacheDekPath(t *testing.T) {
	got, _ := CacheDekPath()
	dir, _ := VaultDir()
	want := filepath.Join(dir, "cache-dek.key")
	if got != want {
		t.Errorf("CacheDekPath = %q, want %q", got, want)
	}
}

func winOrUnix(win, unix string) string {
	if runtime.GOOS == "windows" {
		return win
	}
	return unix
}

var _ = os.Getenv // keep import if unused after edits
