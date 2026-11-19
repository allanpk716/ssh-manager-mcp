package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckIsolatedEnvironmentRequiresConfigRoot(t *testing.T) {
	t.Setenv("SSHMGR_CONFIG_ROOT", "")
	dir := t.TempDir()
	err := checkIsolatedEnvironment(dir)
	if err == nil || !strings.Contains(err.Error(), "SSHMGR_CONFIG_ROOT") {
		t.Fatalf("未设配置根应拒绝运行，实际: %v", err)
	}
}

func TestCheckIsolatedEnvironmentRejectsRootOutsideRunDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SSHMGR_CONFIG_ROOT", t.TempDir())
	err := checkIsolatedEnvironment(dir)
	if err == nil || !strings.Contains(err.Error(), "不在运行目录") {
		t.Fatalf("配置根在运行目录之外应拒绝运行，实际: %v", err)
	}
}

func TestCheckIsolatedEnvironmentAcceptsRootInsideRunDir(t *testing.T) {
	dir := t.TempDir()
	for _, root := range []string{dir, filepath.Join(dir, "sub")} {
		t.Setenv("SSHMGR_CONFIG_ROOT", root)
		if err := checkIsolatedEnvironment(dir); err != nil {
			t.Fatalf("配置根 %s 在运行目录内应通过，实际: %v", root, err)
		}
	}
}
