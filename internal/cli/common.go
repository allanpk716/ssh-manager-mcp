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
// Delegates to the shared vault package (env → FileProvider), used by both CLI
// and MCP server. The master-key KeyProvider (store.FileKeyProvider{}) is
// injected here so vault stays OS-agnostic and doesn't import cli. Plan 16:
// the build-tag `keychain` seam is gone; FileKeyProvider is the sole master-key
// backend (spec §4.2).
func openUnlockedStore() (*store.Store, error) {
	return vault.OpenStore(store.FileKeyProvider{})
}
