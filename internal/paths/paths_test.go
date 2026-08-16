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

func TestServeLogPath(t *testing.T) {
	// Default: serve.log under VaultDir.
	t.Setenv("SSHMGR_SERVE_LOG", "")
	got, err := ServeLogPath()
	if err != nil {
		t.Fatalf("ServeLogPath: %v", err)
	}
	if dir, _ := VaultDir(); filepath.Dir(got) != dir || filepath.Base(got) != "serve.log" {
		t.Fatalf("ServeLogPath = %s, want serve.log under VaultDir %s", got, dir)
	}

	// Env override (test/relocate seam — same pattern as SSHMGR_CACHE_DEK).
	t.Setenv("SSHMGR_SERVE_LOG", "/tmp/custom-serve.log")
	if p, _ := ServeLogPath(); p != "/tmp/custom-serve.log" {
		t.Fatalf("SSHMGR_SERVE_LOG override ignored: %s", p)
	}
}

func winOrUnix(win, unix string) string {
	if runtime.GOOS == "windows" {
		return win
	}
	return unix
}

func TestServeCertPaths(t *testing.T) {
	// Default: under VaultDir.
	t.Setenv("SSHMGR_STORE", "")
	t.Setenv("SSHMGR_SERVE_CERT", "")
	t.Setenv("SSHMGR_SERVE_KEY", "")
	cert, err := ServeCertPath()
	if err != nil {
		t.Fatalf("ServeCertPath: %v", err)
	}
	key, err := ServeKeyPath()
	if err != nil {
		t.Fatalf("ServeKeyPath: %v", err)
	}
	if filepath.Base(cert) != "serve-cert.pem" || filepath.Base(key) != "serve-key.pem" {
		t.Fatalf("unexpected paths: %s / %s", cert, key)
	}
	if dir, _ := VaultDir(); filepath.Dir(cert) != dir || filepath.Dir(key) != dir {
		t.Fatalf("cert/key not under VaultDir: %s / %s (want dir %s)", cert, key, dir)
	}

	// Env override.
	t.Setenv("SSHMGR_SERVE_CERT", "/tmp/custom-cert.pem")
	t.Setenv("SSHMGR_SERVE_KEY", "/tmp/custom-key.pem")
	if c, _ := ServeCertPath(); c != "/tmp/custom-cert.pem" {
		t.Fatalf("SSHMGR_SERVE_CERT override ignored: %s", c)
	}
	if k, _ := ServeKeyPath(); k != "/tmp/custom-key.pem" {
		t.Fatalf("SSHMGR_SERVE_KEY override ignored: %s", k)
	}
}

var _ = os.Getenv // keep import if unused after edits
