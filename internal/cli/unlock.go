package cli

import (
	"errors"
	"fmt"
	"os"
	"runtime"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"ssh-manager-mcp/internal/store"
)

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

// masterKeyProvider returns the KeyProvider `unlock` reads / writes the master
// key through (Plan 16: FileKeyProvider at the program-fixed path, spec §3.1 /
// §4.2). Was the build-tag `keychain` seam (DPAPI / keyring) before Plan 16;
// that seam is gone. The path is env-overridable (SSHMGR_FILEKEY_PATH) so tests
// can point at a temp file without touching the production fixed path.
func masterKeyProvider() store.FileKeyProvider {
	return store.FileKeyProvider{Path: os.Getenv("SSHMGR_FILEKEY_PATH")}
}

func newUnlockCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unlock",
		Short: "Resolve the master key (file, else passphrase) and print SSHMGR_MASTERKEY_HEX",
		RunE: func(cmd *cobra.Command, args []string) error {
			fp := masterKeyProvider()
			mk, err := fp.Get()
			if err == nil {
				printMasterKey(cmd, mk)
				return nil
			}
			if !errors.Is(err, store.ErrNotFound) {
				// master.key present but unreadable (corrupt blob / FS permission
				// / admin password reset). unlock is the interactive CLI entry
				// and ALWAYS falls through to passphrase here (this branch is
				// intentionally not a hard error — the user gets a usable key via
				// passphrase derivation).
				//
				// This is DISTINCT from resolveMasterKey (the programmatic
				// master-key resolver, called only by vault.Open / serve — NOT by
				// unlock): resolveMasterKey HARD-FAILs on a non-ErrNotFound error
				// (no TTY, no safe degradation; spec §5.6). The two paths
				// intentionally diverge: unlock may degrade to passphrase;
				// programmatic open may not.
				return runPassphraseUnlock(cmd)
			}
			// ErrNotFound — no master.key yet: clean first run. Generate +
			// persist via FileKeyProvider.Set (writes the fixed-path file, ACL
			// hardened by T6 on Windows). Set failure → fall through to
			// passphrase so the user still gets a usable key.
			mk, err = store.GenerateMasterKey()
			if err != nil {
				return err
			}
			if err := fp.Set(mk); err != nil {
				return runPassphraseUnlock(cmd)
			}
			printMasterKey(cmd, mk)
			return nil
		},
	}
}

// masterKeyEnvLine renders the line the user sources into their shell to
// expose the master key as SSHMGR_MASTERKEY_HEX. The syntax is chosen per
// BUILD platform (release binaries are per-OS; probing the parent shell is
// unreliable): a POSIX `export` is inert text in PowerShell — a 2026-10 NUC10
// session ran `unlock`, saw it "succeed", and the printed line changed
// nothing in the window it was pasted into.
func masterKeyEnvLine(mk []byte) string {
	hexStr := hexEncode(mk)
	if runtime.GOOS == "windows" {
		return "$env:SSHMGR_MASTERKEY_HEX = '" + hexStr + "'"
	}
	return "export SSHMGR_MASTERKEY_HEX=" + hexStr
}

// masterKeyUnsetLine is `lock`'s counterpart of masterKeyEnvLine.
func masterKeyUnsetLine() string {
	if runtime.GOOS == "windows" {
		return "Remove-Item Env:SSHMGR_MASTERKEY_HEX"
	}
	return "unset SSHMGR_MASTERKEY_HEX"
}

// printMasterKey emits the shell line the user sources into their environment.
func printMasterKey(cmd *cobra.Command, mk []byte) {
	fmt.Fprintln(cmd.OutOrStdout(), masterKeyEnvLine(mk))
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
	fmt.Fprintln(cmd.OutOrStdout(), masterKeyEnvLine(mk))
	return nil
}

func newLockCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "lock",
		Short: "Clear the master key from this shell",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), masterKeyUnsetLine())
			os.Unsetenv("SSHMGR_MASTERKEY_HEX")
			return nil
		},
	}
}
