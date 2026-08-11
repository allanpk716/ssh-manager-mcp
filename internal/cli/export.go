package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/vaultio"
)

// passphraseConfirmPrompt is the second-prompt seam for `export` (defaults to
// the terminal; tests override). The first prompt reuses passphrasePrompt from
// unlock.go so the terminal-reading logic stays in exactly one place.
var passphraseConfirmPrompt = func() ([]byte, error) {
	return readPassphrase("Confirm passphrase: ")
}

func newExportCmd() *cobra.Command {
	var outPath string
	c := &cobra.Command{
		Use:   "export",
		Short: "Export the entire vault to a passphrase-encrypted portable file",
		Long: `Export every server, credential, profile, project, host key, and audit row to a
portable, passphrase-encrypted file. Credentials are decrypted then re-encrypted
with a key derived from YOUR passphrase (Argon2id + AES-256-GCM) — the file is
independent of this machine's vault master key, so it restores on any machine.

The file is only as strong as its passphrase (offline brute-force is possible if
the file leaks — like a KeePass database). Use a strong passphrase.

To restore: ssh-manager import <file> (into a fresh/empty vault).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer st.Close()

			snap, err := st.ExportSnapshot()
			if err != nil {
				return err
			}
			plaintext, err := json.MarshalIndent(snap, "", "  ")
			if err != nil {
				return err
			}

			// Prompt BEFORE creating the file — a mismatch or empty passphrase
			// must never produce (or clobber) the output file.
			pw, err := passphrasePrompt()
			if err != nil {
				return err
			}
			pw2, err := passphraseConfirmPrompt()
			if err != nil {
				return err
			}
			if string(pw) != string(pw2) {
				return fmt.Errorf("passphrases do not match")
			}
			if len(pw) == 0 {
				return fmt.Errorf("passphrase must not be empty")
			}

			blob, err := vaultio.Encrypt(pw, plaintext)
			if err != nil {
				return err
			}

			var w io.Writer
			if outPath == "-" || outPath == "" {
				w = cmd.OutOrStdout()
			} else {
				f, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
				if err != nil {
					return err
				}
				defer f.Close()
				w = f
			}
			if _, err := w.Write(blob); err != nil {
				return err
			}
			if outPath != "-" && outPath != "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "exported %d servers / %d credentials to %s\n", len(snap.Servers), len(snap.Credentials), outPath)
			}
			return nil
		},
	}
	c.Flags().StringVar(&outPath, "out", "", "output file (use '-' for stdout; required for a real file)")
	return c
}
