package cli

import (
	"fmt"
	"os"

	"ssh-manager-mcp/internal/instname"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
)

// checkInstanceFlag enforces the §2.2 env×flag mutex and the name whitelist
// at the CLI layer: the single-file/dir CACHE envs fully override path
// resolution, so combining one with --instance would silently route the
// command to the wrong instance (or make all instances share one DEK).
// SSHMGR_CACHE_DEK_DIR deliberately composes — it is a directory-level seam
// that derives per-instance paths.
func checkInstanceFlag(instance string) error {
	if instance == "" {
		return nil
	}
	for _, env := range []string{"SSHMGR_CACHE_DIR", "SSHMGR_CACHE_DEK"} {
		if os.Getenv(env) != "" {
			return fmt.Errorf("--instance and %s are mutually exclusive — %s fully overrides the cache path/DEK resolution and would silently route this command to the wrong instance; unset the env or drop --instance", env, env)
		}
	}
	return instname.Valid(instance)
}

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
