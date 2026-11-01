package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVaultDir_FixedPath(t *testing.T) {
	root := useTestVaultRoot(t)
	t.Setenv("SSHMGR_STORE", "")
	t.Setenv("SSHMGR_FILEKEY_PATH", "")
	got, err := VaultDir()
	if err != nil {
		t.Fatalf("VaultDir: %v", err)
	}
	want := filepath.Join(root, "ssh-manager")
	if got != want {
		t.Errorf("VaultDir = %q, want %q", got, want)
	}
}

func TestVaultDir_TestProcessRequiresEnv(t *testing.T) {
	t.Setenv(VaultRootEnv, "")
	got, err := VaultDir()
	if err == nil {
		t.Fatalf("VaultDir = %q, want an error in a test process", got)
	}
	if !strings.Contains(err.Error(), "SSHMGR_VAULT_ROOT must be set in tests") {
		t.Fatalf("VaultDir error = %v, want the SSHMGR_VAULT_ROOT test guard", err)
	}
	if got != "" {
		t.Fatalf("VaultDir returned %q with an error; want an empty path", got)
	}
}

func TestStorePath_EnvOverride(t *testing.T) {
	t.Setenv("SSHMGR_STORE", "/tmp/custom/store.db")
	got, err := StorePath()
	if err != nil || got != "/tmp/custom/store.db" {
		t.Errorf("StorePath = %q,%v; want env override", got, err)
	}
}

func TestUserConfigDir_EnvOverride(t *testing.T) {
	root := t.TempDir()
	t.Setenv(UserConfigRootEnv, root)
	got, err := UserConfigDir()
	if err != nil {
		t.Fatalf("UserConfigDir: %v", err)
	}
	if got != root {
		t.Fatalf("UserConfigDir = %q, want %q", got, root)
	}
}

func TestUserConfigDir_TestProcessRequiresEnv(t *testing.T) {
	t.Setenv(UserConfigRootEnv, "")
	got, err := UserConfigDir()
	if err == nil {
		t.Fatalf("UserConfigDir = %q, want an error in a test process", got)
	}
	if !strings.Contains(err.Error(), "SSHMGR_CONFIG_ROOT must be set in tests") {
		t.Fatalf("UserConfigDir error = %v, want the SSHMGR_CONFIG_ROOT test guard", err)
	}
	if got != "" {
		t.Fatalf("UserConfigDir returned %q with an error; want an empty path", got)
	}
}

func TestMasterKeyPath_NoEnvLandsInVaultDir(t *testing.T) {
	useTestVaultRoot(t)
	t.Setenv("SSHMGR_FILEKEY_PATH", "")
	got, _ := MasterKeyPath()
	dir, _ := VaultDir()
	want := filepath.Join(dir, "master.key.plain")
	if got != want {
		t.Errorf("MasterKeyPath = %q, want %q", got, want)
	}
}

func TestCacheDekPath(t *testing.T) {
	useTestVaultRoot(t)
	got, _ := CacheDekPath()
	dir, _ := VaultDir()
	want := filepath.Join(dir, "cache-dek.key")
	if got != want {
		t.Errorf("CacheDekPath = %q, want %q", got, want)
	}
}

func TestServeLogPath(t *testing.T) {
	useTestVaultRoot(t)
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

func TestServeCertPaths(t *testing.T) {
	useTestVaultRoot(t)
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

func TestCacheDekPathFor(t *testing.T) {
	vd := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DEK", "")
	t.Setenv("SSHMGR_CACHE_DEK_DIR", vd)
	// default instance: legacy filename, unchanged
	p, err := CacheDekPathFor("")
	if err != nil || filepath.Base(p) != "cache-dek.key" || filepath.Dir(p) != vd {
		t.Fatalf("default = %q, %v", p, err)
	}
	// named instance: per-instance variant
	p, err = CacheDekPathFor("agentA")
	if err != nil || filepath.Base(p) != "cache-dek-agentA.key" || filepath.Dir(p) != vd {
		t.Fatalf("agentA = %q, %v", p, err)
	}
	// single-file env wins over everything (existing escape hatch)
	t.Setenv("SSHMGR_CACHE_DEK", filepath.Join(vd, "x.key"))
	p, err = CacheDekPathFor("agentA")
	if err != nil || p != filepath.Join(vd, "x.key") {
		t.Fatalf("env override = %q, %v", p, err)
	}
	// illegal instance name: fail-closed (path-traversal backstop)
	t.Setenv("SSHMGR_CACHE_DEK", "")
	if _, err := CacheDekPathFor("../evil"); err == nil {
		t.Fatal("illegal instance must be refused")
	}
}

func useTestVaultRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv(VaultRootEnv, root)
	return root
}

var _ = os.Getenv // keep import if unused after edits
