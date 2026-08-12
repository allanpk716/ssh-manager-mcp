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
	var passphraseSrc string
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
			// Sniff BEFORE prompting: a plaintext JSON backup (Plan 13) must not
			// trigger a passphrase prompt. Only the SSHMGRV1 envelope path prompts.
			// NOTE: vaultio.EncryptWithKey (Plan 12 cache) ALSO uses SSHMGRV1 magic.
			// Feeding a cache file here would classify as encrypted, prompt, then
			// fail GCM auth (safe — just not the user's intent). Import is for
			// export/backup files, not cache files.
			//
			// --passphrase-file only applies to the ENCRYPTED branch; on plaintext
			// it is irrelevant and intentionally ignored (no error).
			var plaintext []byte
			if vaultio.IsEncrypted(blob) {
				var pw []byte
				if passphraseSrc != "" {
					pw, err = readPassphraseFile(passphraseSrc)
					if err != nil {
						return err
					}
				} else {
					pw, err = passphrasePrompt()
					if err != nil {
						return err
					}
				}
				plaintext, err = vaultio.Decrypt(pw, blob)
				if err != nil {
					return err
				}
			} else {
				plaintext = blob
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
	c.Flags().StringVar(&passphraseSrc, "passphrase-file", "", "read passphrase from this file (non-interactive; encrypted files only). Plaintext imports ignore it.")
	return c
}
