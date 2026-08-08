package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/store"
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
// (In Plan 1 we pass the master key via SSHMGR_MASTERKEY_HEX for tests; real unlock lands when wired to keyring.)
func openUnlockedStore(cmd *cobra.Command) (*store.Store, error) {
	path, err := storePath()
	if err != nil {
		return nil, err
	}
	mkHex := os.Getenv("SSHMGR_MASTERKEY_HEX")
	if mkHex == "" {
		return nil, fmt.Errorf("vault locked: run `ssh-manager unlock` (or set SSHMGR_MASTERKEY_HEX for scripting)")
	}
	mk, err := hexDecode(mkHex)
	if err != nil {
		return nil, err
	}
	return store.Open(path, mk)
}
