package clientops

import (
	"os"
	"path/filepath"
	"testing"
)

// redirectUserConfigDir pins os.UserConfigDir to a temp dir (roles_test /
// clear_test precedent). Multi-instance tests must NOT set SSHMGR_CACHE_DIR
// (spec §9.5: env and --instance are mutually exclusive).
func redirectUserConfigDir(t *testing.T) string {
	t.Helper()
	userDir := t.TempDir()
	t.Setenv("APPDATA", userDir)         // os.UserConfigDir on Windows
	t.Setenv("XDG_CONFIG_HOME", userDir) // and on Unix
	t.Setenv("SSHMGR_CACHE_DIR", "")
	return userDir
}

func TestCachePathsFor_InstanceRouting(t *testing.T) {
	userDir := redirectUserConfigDir(t)
	base := filepath.Join(userDir, "ssh-manager")

	dir, bin, meta, audit, err := CachePathsFor("")
	if err != nil || dir != base || bin != filepath.Join(base, "cache.bin") ||
		meta != filepath.Join(base, "cache.meta.json") || audit != filepath.Join(base, "cache-audit.log") {
		t.Fatalf("default = %q,%q,%q,%q,%v", dir, bin, meta, audit, err)
	}
	// CachePaths() is the zero-change wrapper
	d2, _, _, _, _ := CachePaths()
	if d2 != base {
		t.Fatalf("CachePaths() = %q, want %q", d2, base)
	}

	idir := filepath.Join(base, "instances", "agentA")
	dir, bin, _, _, err = CachePathsFor("agentA")
	if err != nil || dir != idir || bin != filepath.Join(idir, "cache.bin") {
		t.Fatalf("agentA = %q,%q,%v", dir, bin, err)
	}

	// env wins entirely (escape hatch; CLI layer enforces the mutex)
	t.Setenv("SSHMGR_CACHE_DIR", filepath.Join(userDir, "override"))
	dir, _, _, _, _ = CachePathsFor("agentA")
	if dir != filepath.Join(userDir, "override") {
		t.Fatalf("env override must win: %q", dir)
	}

	// illegal name: fail-closed
	t.Setenv("SSHMGR_CACHE_DIR", "")
	if _, _, _, _, err := CachePathsFor("../evil"); err == nil {
		t.Fatal("illegal instance must be refused before Join")
	}
}

func TestListInstances(t *testing.T) {
	userDir := redirectUserConfigDir(t)
	root := filepath.Join(userDir, "ssh-manager", "instances")
	if got, err := ListInstances(); err != nil || len(got) != 0 {
		t.Fatalf("missing root = %v, %v", got, err)
	}
	for _, n := range []string{"agentB", "agentA"} {
		if err := os.MkdirAll(filepath.Join(root, n), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "not-a-dir"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ListInstances()
	if err != nil || len(got) != 2 || got[0] != "agentA" || got[1] != "agentB" {
		t.Fatalf("ListInstances = %v, %v", got, err)
	}
	if r, err := InstancesRoot(); err != nil || r != root {
		t.Fatalf("InstancesRoot = %q, %v", r, err)
	}
}
