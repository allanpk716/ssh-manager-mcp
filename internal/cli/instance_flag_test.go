package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstanceFlagMutex(t *testing.T) {
	for _, env := range []string{"SSHMGR_CACHE_DIR", "SSHMGR_CACHE_DEK"} {
		t.Run(env, func(t *testing.T) {
			withEnv(t, map[string]string{env: t.TempDir()})
			if err := checkInstanceFlag("agentA"); err == nil ||
				!strings.Contains(err.Error(), "mutually exclusive") {
				t.Fatalf("%s + --instance must error: %v", env, err)
			}
			if err := checkInstanceFlag(""); err != nil {
				t.Fatalf("no flag = no mutex: %v", err)
			}
		})
	}
	t.Run("SSHMGR_CACHE_DEK_DIR composes", func(t *testing.T) {
		withEnv(t, map[string]string{"SSHMGR_CACHE_DEK_DIR": t.TempDir(), "SSHMGR_CACHE_DIR": "", "SSHMGR_CACHE_DEK": ""})
		if err := checkInstanceFlag("agentA"); err != nil {
			t.Fatalf("dir-level seam must compose with --instance: %v", err)
		}
	})
	t.Run("illegal flag name", func(t *testing.T) {
		withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": "", "SSHMGR_CACHE_DEK": ""})
		if err := checkInstanceFlag("bad name"); err == nil {
			t.Fatal("illegal name must be refused at the flag layer")
		}
	})
}

// mcp 无默认 cache 且有实例 → 报错列实例（exit 前的 RunE 错误）
func TestMCP_NoDefaultCache_WithInstances_Errors(t *testing.T) {
	userDir := t.TempDir()
	t.Setenv("APPDATA", userDir)
	t.Setenv("XDG_CONFIG_HOME", userDir)
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": "", "SSHMGR_CACHE_DEK": ""})
	t.Setenv("SSHMGR_CACHE_DEK_DIR", t.TempDir())
	for _, n := range []string{"agentA", "agentB"} {
		os.MkdirAll(filepath.Join(userDir, "ssh-manager", "instances", n), 0o700)
	}
	cmd := newMCPCmd()
	cmd.SetArgs([]string{"--token", "x", "--cache"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--instance") ||
		!strings.Contains(err.Error(), "agentA") || !strings.Contains(err.Error(), "agentB") {
		t.Fatalf("must refuse listing instances: %v", err)
	}
}
