package cli

import (
	"encoding/hex"
	"path/filepath"
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
	err := mcpserver.RunStdio("not-a-real-token", keychain)
	if err == nil {
		t.Fatal("unknown token must error")
	}
}
