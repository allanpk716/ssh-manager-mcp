package cli

import (
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/store"
)

func TestRunStdioRejectsUnknownToken(t *testing.T) {
	dir := t.TempDir()
	mk, _ := store.GenerateMasterKey()
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         filepath.Join(dir, "t.db"),
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(mk),
	})
	err := mcpserver.RunStdio("not-a-real-token", store.FileKeyProvider{})
	if err == nil {
		t.Fatal("unknown token must error")
	}
}

// TestResolveToken: --token wins, SSHMGR_TOKEN env is the fallback, both
// empty yields "" (RunE turns that into the required error).
func TestResolveToken(t *testing.T) {
	t.Setenv("SSHMGR_TOKEN", "from-env")
	if got := resolveToken("from-flag"); got != "from-flag" {
		t.Fatal("flag 必须优先")
	}
	if got := resolveToken(""); got != "from-env" {
		t.Fatal("env 兜底")
	}
	t.Setenv("SSHMGR_TOKEN", "")
	if got := resolveToken(""); got != "" {
		t.Fatal("双空返回空（RunE 报错）")
	}
}

// TestMcpRequiresTokenOrEnv: with MarkFlagRequired gone, the required-ness
// moved into RunE — running plain `mcp` with neither flag nor env must fail
// with the combined error (synthetic token values only; this is a PUBLIC repo).
func TestMcpRequiresTokenOrEnv(t *testing.T) {
	t.Setenv("SSHMGR_TOKEN", "")
	root := NewRootCmd()
	root.SetArgs([]string{"mcp"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "--token or SSHMGR_TOKEN is required") {
		t.Fatalf("want '--token or SSHMGR_TOKEN is required', got %v", err)
	}
}
