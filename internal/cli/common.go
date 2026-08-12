package cli

import (
	"os"

	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
)

// storePath resolves the vault path (env override > default).
func storePath() (string, error) {
	if p := os.Getenv("SSHMGR_STORE"); p != "" {
		return p, nil
	}
	return store.DefaultStorePath()
}

func metaFilePath() (string, error) {
	p, err := storePath()
	if err != nil {
		return "", err
	}
	// meta.json lives next to the store file
	return p + ".meta.json", nil
}

// openUnlockedStore fails the command with guidance if the vault is locked.
// Delegates to the shared vault package (env → injected keychain → FileProvider)
// used by both CLI and MCP server. The platform KeyProvider (cli/keychain seam)
// is injected here so vault stays OS-agnostic and doesn't import cli.
func openUnlockedStore() (*store.Store, error) { return vault.OpenStore(keychain) }
