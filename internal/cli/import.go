package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vaultio"
)

func newImportCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "import <file>",
		Short: "Restore a vault from a passphrase-encrypted export file",
		Long: `Restore an export file into THIS vault. The target vault must be EMPTY (import
never clobbers — move/delete store.db to get a fresh empty vault first) and
UNLOCKED (credentials are re-sealed under this vault's master key).

Project tokens carry their hash, so the original plaintext token (from when the
export was made) still validates after import.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			blob, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			pw, err := passphrasePrompt()
			if err != nil {
				return err
			}
			plaintext, err := vaultio.Decrypt(pw, blob)
			if err != nil {
				return err
			}
			var snap store.Snapshot
			if err := json.Unmarshal(plaintext, &snap); err != nil {
				return err
			}
			st, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer st.Close()
			if err := st.ImportSnapshot(&snap); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "imported %d servers / %d credentials\n", len(snap.Servers), len(snap.Credentials))
			return nil
		},
	}
	return c
}
