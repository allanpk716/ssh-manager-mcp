package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"ssh-manager-mcp/internal/store"
)

// keychain is the master-key source (default real OS keychain; tests override).
var keychain store.KeyProvider = store.KeyringKeyProvider{}

// readPassphrase prints prompt to stderr and reads a line from the terminal
// without echo. Shared by unlock / export / import — the single place that
// knows how to talk to the terminal for a passphrase.
func readPassphrase(prompt string) ([]byte, error) {
	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	return b, err
}

// passphrasePrompt reads a passphrase (default terminal; tests override).
// Shared seam — export and import reuse it for their first prompt.
var passphrasePrompt = func() ([]byte, error) {
	return readPassphrase("Enter passphrase to unlock vault: ")
}

func newUnlockCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unlock",
		Short: "Resolve the master key (keychain, else passphrase) and print SSHMGR_MASTERKEY_HEX",
		RunE: func(cmd *cobra.Command, args []string) error {
			mk, err := keychain.Get()
			if err == nil {
				fmt.Fprintf(cmd.OutOrStdout(), "export SSHMGR_MASTERKEY_HEX=%s\n", hexEncode(mk))
				return nil
			}
			if err != store.ErrNotFound {
				// keychain unavailable (e.g. headless Linux w/o Secret Service) → passphrase fallback
				return runPassphraseUnlock(cmd)
			}
			// first run with a working keychain: generate + store
			mk, err = store.GenerateMasterKey()
			if err != nil {
				return err
			}
			if err := keychain.Set(mk); err != nil {
				// can't persist to keychain → fall back to passphrase path
				return runPassphraseUnlock(cmd)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "export SSHMGR_MASTERKEY_HEX=%s\n", hexEncode(mk))
			return nil
		},
	}
}

func runPassphraseUnlock(cmd *cobra.Command) error {
	metaPath, err := metaFilePath()
	if err != nil {
		return err
	}
	meta, _ := store.LoadMeta(metaPath)
	if meta == nil {
		// first passphrase use: generate salt
		if err := store.SaveMeta(metaPath, &store.Meta{PassphraseSalt: store.NewSalt16()}); err != nil {
			return err
		}
		m, err := store.LoadMeta(metaPath)
		if err != nil {
			return fmt.Errorf("reload vault metadata: %w", err)
		}
		meta = m
	}
	pass, err := passphrasePrompt()
	if err != nil {
		return err
	}
	mk := store.DeriveFromPassphrase(pass, meta.PassphraseSalt)
	fmt.Fprintf(cmd.OutOrStdout(), "export SSHMGR_MASTERKEY_HEX=%s\n", hexEncode(mk))
	return nil
}

func newLockCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "lock",
		Short: "Clear the master key from this shell",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), "unset SSHMGR_MASTERKEY_HEX")
			os.Unsetenv("SSHMGR_MASTERKEY_HEX")
			return nil
		},
	}
}
