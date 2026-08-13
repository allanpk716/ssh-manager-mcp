package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"ssh-manager-mcp/internal/store"
)

// keychain is the package seam master-key source (defined in keychain_unix.go
// or keychain_windows.go, build-tag selected). Tests swap it for a fake.

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

// firstRunOutcome enumerates the outcome of the v0.2.0 → DPAPI migration
// probe on `unlock`'s ErrNotFound branch (Windows; nil hook on Unix).
type firstRunOutcome int

const (
	// firstRunSkip: nothing to migrate (clean env — no legacy keychain slot).
	// Caller proceeds to first-run generate + persist.
	firstRunSkip firstRunOutcome = iota
	// firstRunMigrated: legacy key migrated to DPAPI file (already persisted).
	// Caller prints mk (from the return value) — do NOT generate.
	firstRunMigrated
	// firstRunStop: abort without generating (declined prompt, or legacy slot
	// present but unreadable — sshd/1312). err carries the user-facing reason
	// (already printed to stderr by the migrator when it's the 1312 path).
	firstRunStop
)

// firstRunMigrator, if non-nil, is invoked when the platform KeyProvider
// (keychain seam) reports ErrNotFound on `unlock` — i.e. no master key is
// persisted yet. It is the single hook for the v0.2.0 keychain → DPAPI
// migration (Windows only; nil on Unix, see migrate_windows.go). It probes the
// legacy v0.2.0 keychain slots (master-key + cache-dek) and, in an interactive
// session, migrates them to DPAPI files. The return outcome tells the caller
// whether to print the migrated key, first-run generate, or stop. All
// migration messages route through w (the command's stderr writer) so tests
// capture them via cmd.ErrOrStderr().
//
// Overridable by tests (Unix builds compile a nil default; Windows builds set
// the real impl via migrate_windows.go init).
var firstRunMigrator func(w interface{ Write([]byte) (int, error) }) (mk []byte, outcome firstRunOutcome, err error)

// postGetMigrator, if non-nil, is invoked AFTER keychain.Get() succeeds but
// BEFORE printMasterKey. It is the hook for the user-scope → machine-scope
// DPAPI migration (Windows only; nil on Unix — migrate_windows.go's init sets
// it, Unix has no migrate_windows.go so it stays nil and the hook is a no-op).
// Unlike firstRunMigrator (which fires on ErrNotFound / no key yet),
// postGetMigrator fires when a key WAS read — needed because Plan 15 T2's
// dual-scope Get returns success for a legacy user-scope blob, so
// firstRunMigrator (ErrNotFound-gated) never sees it (Plan 15 codex #1).
//
// mk is the key just read by Get; the migrator may re-protect the on-disk file
// in place but the key VALUE is unchanged (re-protect preserves the plaintext
// master key C). The caller always prints the mk it already has.
//
// Return values: (migrated bool, err error).
//   - migrated=true: master.key was re-protected to machine-scope. Caller
//     proceeds to print mk (unchanged).
//   - migrated=false, err=nil: nothing to migrate (already machine-scope, no
//     master.key at the path, user declined, or scope unreadable) — caller
//     proceeds to print mk.
//   - err non-nil: hard failure mid-migrate. Caller surfaces it.
//
// Overridable by tests; nil on Unix by construction (no migrate_windows.go).
var postGetMigrator func(w interface{ Write([]byte) (int, error) }, mk []byte) (bool, error)

func newUnlockCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unlock",
		Short: "Resolve the master key (keychain, else passphrase) and print SSHMGR_MASTERKEY_HEX",
		RunE: func(cmd *cobra.Command, args []string) error {
			mk, err := keychain.Get()
			if err == nil {
				// Plan 15 codex #1: post-Get migration hook. On Windows this
				// re-protects a legacy user-scope master.key to machine-scope
				// (needed for boot auto-start via Task Scheduler, which has no
				// interactive logon → can't read a user-scope blob). Nil on Unix
				// (no migrate_windows.go → no-op). MUST run BEFORE printMasterKey
				// so the printed key matches the now-machine-scope file. The mk
				// VALUE is unchanged by re-protect (only the DPAPI scope changes),
				// so we print the mk we already hold either way.
				if postGetMigrator != nil {
					if _, mErr := postGetMigrator(cmd.ErrOrStderr(), mk); mErr != nil {
						return mErr
					}
				}
				printMasterKey(cmd, mk)
				return nil
			}
			if !errors.Is(err, store.ErrNotFound) {
				// keychain unavailable (e.g. headless Linux w/o Secret Service), or
				// Windows master.key present but undecryptable (DPAPI failure on a
				// corrupt file / admin password reset / session anomaly). unlock is
				// the interactive CLI entry and ALWAYS falls through to passphrase
				// here on every platform (this branch is intentionally not a hard
				// error — the user gets a usable key via passphrase derivation).
				//
				// This is DISTINCT from resolveMasterKey (the programmatic master-key
				// resolver, called only by vault.Open / serve — NOT by unlock):
				// resolveMasterKey hard-fails on Windows DPAPI decrypt (no TTY, no
				// safe degradation; spec §5.6). The two paths intentionally diverge:
				// unlock may degrade to passphrase; programmatic open may not. The
				// Windows silent-wrong-key concern (passphrase-derived key ≠ DPAPI
				// master key → printed key can't open the vault) is pre-existing
				// (T3/T4-approved) and out of scope for T5's migration branch.
				return runPassphraseUnlock(cmd)
			}
			// ErrNotFound — no master key persisted. On Windows this may be a
			// v0.2.0 → DPAPI migration opportunity (old keychain slot readable in
			// an interactive session). On Unix firstRunMigrator is nil → skip.
			if firstRunMigrator != nil {
				migrated, outcome, mErr := firstRunMigrator(cmd.ErrOrStderr())
				if mErr != nil {
					return mErr
				}
				switch outcome {
				case firstRunMigrated:
					printMasterKey(cmd, migrated)
					return nil
				case firstRunStop:
					// Declined prompt, or legacy slot present but unreadable
					// (sshd/1312). The migrator already printed guidance to
					// stderr. Abort WITHOUT generating a fresh key (generating
					// would orphan the old vault behind the unreadable slot).
					return nil
				}
				// firstRunSkip: clean env → fall through to first-run generate.
			}
			// clean first run (no migration): generate + store
			mk, err = store.GenerateMasterKey()
			if err != nil {
				return err
			}
			if err := keychain.Set(mk); err != nil {
				// can't persist to keychain → fall back to passphrase path
				return runPassphraseUnlock(cmd)
			}
			printMasterKey(cmd, mk)
			return nil
		},
	}
}

// printMasterKey emits the export line the user sources into their shell.
func printMasterKey(cmd *cobra.Command, mk []byte) {
	fmt.Fprintf(cmd.OutOrStdout(), "export SSHMGR_MASTERKEY_HEX=%s\n", hexEncode(mk))
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
