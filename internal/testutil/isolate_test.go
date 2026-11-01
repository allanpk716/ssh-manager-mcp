package testutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/paths"
)

// TestIsolateDirs_CoversAllPlatformDefaults 验证共享测试辅助函数在三个
// 操作系统上都离开真实用户目录与系统级 vault 目录。
func TestIsolateDirs_CoversAllPlatformDefaults(t *testing.T) {
	vaultDir, userDir := IsolateDirs(t)

	gotUserDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if gotUserDir != userDir {
		t.Fatalf("os.UserConfigDir() = %q, want isolated %q", gotUserDir, userDir)
	}
	if os.Getenv("HOME") == "" || !strings.HasPrefix(os.Getenv("HOME"), os.TempDir()) {
		t.Fatalf("HOME must point into a test temporary directory, got %q", os.Getenv("HOME"))
	}

	for name, got := range map[string]string{
		"store":        mustPath(t, paths.StorePath),
		"master key":   mustPath(t, paths.MasterKeyPath),
		"default DEK":  mustPath(t, paths.CacheDekPath),
		"instance DEK": mustPath(t, func() (string, error) { return paths.CacheDekPathFor("agentA") }),
		"serve log":    mustPath(t, paths.ServeLogPath),
		"serve cert":   mustPath(t, paths.ServeCertPath),
		"serve key":    mustPath(t, paths.ServeKeyPath),
		"serve marker": mustPath(t, paths.ServeCertMarkerPath),
	} {
		if filepath.Dir(got) != vaultDir {
			t.Fatalf("%s path = %q; want parent directory %q", name, got, vaultDir)
		}
	}
}

func mustPath(t *testing.T, fn func() (string, error)) string {
	t.Helper()
	path, err := fn()
	if err != nil {
		t.Fatal(err)
	}
	return path
}
