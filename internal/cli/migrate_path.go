package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/paths"
	"ssh-manager-mcp/internal/store"
)

// migratePathOpts are the knobs for runMigratePath. Tests construct them
// directly; the cobra layer fills them from flags.
type migratePathOpts struct {
	// From is the source vault directory (contains store.db + master.key.plain).
	// Empty → auto-detect the pre-Plan-16 default (UserConfigDir/ssh-manager/).
	From string
	// KeepOld preserves the source files after a successful migration.
	// Default (false) deletes the old store.db + master.key.plain once the
	// N/N self-check passes.
	KeepOld bool
}

// guidanceUnreadableBackend is the error returned when migrate-path detects an
// old vault at the source location but its master.key is NOT a readable file
// (e.g. it is a legacy DPAPI blob or keychain entry that can't be resolved in
// the current session — the NUC10 case where machine-scope DPAPI fails under
// sshd). migrate-path MUST NOT try to read DPAPI/keyring (spec §5.3, xcheck
// consensus B+C; Plan 16 Q6/Q10 deleted that code). Instead it tells the user
// to run export + import in a session where the old backend IS readable.
const guidanceUnreadableBackend = `old vault master key at %q is not a readable file in this session.
It may be a legacy DPAPI blob or keychain entry that this process cannot resolve
(sshd / service sessions often can't). migrate-path only relocates FILE-type vaults.

To migrate, run these steps in an interactive / RDP session where the old
backend IS readable (export also goes through the old backend — it can't run
headlessly either). ` + "`import <file>`" + ` writes into the vault at SSHMGR_STORE
(or the program-fixed path if SSHMGR_STORE is unset), and requires an EMPTY,
UNLOCKED target — so set the path and create the new master key first:

    # 1. export the old vault to a portable passphrase-encrypted file
    sshmgr export --out vault.sme --passphrase-file pass.txt

    # 2. point at the NEW fixed path and create its master key + empty store
    #    (on Windows the default is C:\ProgramData\ssh-manager\ — leave SSHMGR_STORE unset)
    sshmgr unlock

    # 3. import the portable file into the new vault (re-seals under the new key)
    sshmgr import --passphrase-file pass.txt vault.sme

Then delete the old store.db + master.key.* by hand. See docs/backup-restore.md.`

func newMigratePathCmd() *cobra.Command {
	var (
		from    string
		keepOld bool
	)
	c := &cobra.Command{
		Use:   "migrate-path",
		Short: "Relocate a file-vault from an old location to the program-fixed path",
		Long: `Migrate a FILE-type vault (master.key.plain + store.db) from an old location to
the program-fixed path (Windows C:\ProgramData\ssh-manager\; Unix /var/lib/ssh-manager/).

Scope (spec §5.3): file-type vaults ONLY. If the old master key is not a readable
file (e.g. legacy DPAPI blob or keychain entry unreadable in this session), this
command errors out and tells you to run ` + "`export`" + ` then ` + "`import --passphrase-file`" + `
in an interactive/RDP session where the old backend resolves. migrate-path never
reads DPAPI/keyring.

After copying, a N/N self-check runs: every server's credential must decrypt under
the copied key. Only then is the old location deleted (unless --keep-old).

The OLD path defaults to UserConfigDir/ssh-manager/ (the pre-Plan-16 default);
override with --from <dir>. The NEW path comes from the paths package (env
override SSHMGR_STORE / SSHMGR_FILEKEY_PATH for testing or custom installs).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMigratePath(cmd.OutOrStdout(), migratePathOpts{From: from, KeepOld: keepOld})
		},
	}
	c.Flags().StringVar(&from, "from", "", "source vault dir (default: UserConfigDir/ssh-manager/)")
	c.Flags().BoolVar(&keepOld, "keep-old", false, "preserve the old vault files after a successful migration")
	return c
}

// oldVaultDir resolves the source directory (spec §5.3 detection). --from wins;
// otherwise fall back to the pre-Plan-16 default UserConfigDir/ssh-manager/.
// SSHMGR_STORE is intentionally NOT consulted here: paths.StorePath honors it as
// the NEW destination, so overloading it as the OLD indicator would collide.
// Use --from to point at an old vault on a non-default path.
func oldVaultDir(from string) (string, error) {
	if from != "" {
		return from, nil
	}
	cfg, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config dir for old vault: %w", err)
	}
	return filepath.Join(cfg, "ssh-manager"), nil
}

// runMigratePath relocates a file-type vault from the old location to the
// program-fixed path with an N/N self-check. See spec §5.3.
func runMigratePath(w io.Writer, opts migratePathOpts) error {
	oldDir, err := oldVaultDir(opts.From)
	if err != nil {
		return err
	}
	oldStore := filepath.Join(oldDir, paths.StoreFilename)
	oldMK := filepath.Join(oldDir, paths.MasterKeyFilename)

	// NEW destination comes from the paths package (honors SSHMGR_STORE /
	// SSHMGR_FILEKEY_PATH for test/migrate; defaults to the fixed path).
	newStore, err := paths.StorePath()
	if err != nil {
		return fmt.Errorf("resolve new store path: %w", err)
	}
	newMK, err := paths.MasterKeyPath()
	if err != nil {
		return fmt.Errorf("resolve new master key path: %w", err)
	}

	// Refuse same-location migration — nothing to do, and the delete step would
	// destroy the very vault we just "migrated".
	if samePath(oldStore, newStore) {
		fmt.Fprintf(w, "old and new store paths are the same (%q); nothing to migrate\n", newStore)
		return nil
	}

	// --- detect: is there an old vault at all? ---
	oldStoreExists, err := fileExists(oldStore)
	if err != nil {
		return fmt.Errorf("stat old store %q: %w", oldStore, err)
	}
	oldMKExists, err := fileExists(oldMK)
	if err != nil {
		return fmt.Errorf("stat old master key %q: %w", oldMK, err)
	}
	if !oldStoreExists && !oldMKExists {
		// No source vault — nothing to migrate. Idempotent no-op (re-running
		// after a successful migrate lands here cleanly).
		fmt.Fprintf(w, "no old vault found at %q; nothing to migrate\n", oldDir)
		return nil
	}
	if !oldStoreExists && oldMKExists {
		// Orphan master key with no store — nothing meaningful to migrate.
		// Tell the user rather than silently copying a useless key file.
		return fmt.Errorf("found master.key.plain at %q but no store.db at %q; nothing to migrate (remove the orphan key by hand if stale)", oldMK, oldStore)
	}

	// --- read old master key as a FILE (no DPAPI / no keyring). ---
	// This is the file-type discriminator: if the key is absent or unreadable
	// as a file, the old backend is not a file-type vault and migrate-path must
	// NOT try to read it through any other backend (spec §5.3, Q6/Q10 clean delete).
	mk, err := os.ReadFile(oldMK)
	if err != nil {
		// fs.ErrNotExist here means store.db is present but master.key.plain is
		// gone — the signature of a DPAPI/keychain backend with no plaintext key
		// file (NUC10 machine-scope DPAPI case). Any other read error (EISDIR
		// for a directory standing in for a blob, EACCES, …) is the same "not a
		// readable file backend" shape. Either way, guide the user to run
		// export + import in a session where the old backend IS readable.
		return errors.New(GuidanceUnreadableBackendMsg(oldMK))
	}
	if !store.ValidMasterKeyLen(mk) {
		// Key file exists and is readable but is the wrong length (not a 32-byte
		// FileKeyProvider key) — likely a DPAPI/keyring blob written to that path
		// by a legacy build. Treat as unreadable backend: we can't use it as a
		// FileKeyProvider key, and we MUST NOT feed a blob to store.Open.
		return errors.New(GuidanceUnreadableBackendMsg(oldMK))
	}

	// --- guard the destination: never clobber a populated new vault. ---
	if populated, err := storeHasServers(newStore, mk); err != nil {
		return fmt.Errorf("probe new vault at %q: %w", newStore, err)
	} else if populated {
		return fmt.Errorf("new vault at %q already has servers; refusing to clobber (delete it first if you want to re-migrate)", newStore)
	}

	// --- N before: count servers in the old store. ---
	// storeServerCount also Checkpoints the WAL into the main file (so the
	// byte-copy below is self-contained); a Checkpoint failure is surfaced
	// rather than swallowed because an un-checkpointed WAL could hold UPDATE-only
	// changes the byte-copy would miss while still passing the decrypt self-check.
	oldCount, err := storeServerCount(oldStore, mk)
	if err != nil {
		// store.db exists but won't open under the file key — same shape as the
		// unreadable backend; the key is wrong / the blob isn't a file key.
		return fmt.Errorf("%s (open old store: %v)", GuidanceUnreadableBackendMsg(oldMK), err)
	}

	// --- relocate: copy master.key + store.db to the new location. ---
	// The new master.key is written through FileKeyProvider.Set so HardenACL
	// (T6) is applied on Windows — a migrate is exactly the moment you want the
	// new file's ACL hardened, not a stale inherited DACL.
	if err := os.MkdirAll(filepath.Dir(newMK), 0o700); err != nil {
		return fmt.Errorf("mkdir new master key dir: %w", err)
	}
	if err := (store.FileKeyProvider{Path: newMK}).Set(mk); err != nil {
		return fmt.Errorf("write new master key: %w", err)
	}
	// Clear any stale WAL/SHM sidecars at the destination BEFORE copying the
	// main store file. A leftover -wal/-shm from a previous run could make the
	// copied store.db appear empty or inconsistent to the next Open (modernc
	// reconciles them on open). Removing them when the main file is being
	// (re)written from a clean source is safe and idempotent.
	_ = os.Remove(newStore + "-wal")
	_ = os.Remove(newStore + "-shm")
	if err := copyFile(oldStore, newStore, 0o600); err != nil {
		return fmt.Errorf("copy store.db: %w", err)
	}
	fmt.Fprintf(w, "copied vault: %s -> %s\n", oldDir, filepath.Dir(newStore))

	// --- N/N self-check: decrypt every server credential under the copied key. ---
	if err := selfCheckDecryptAll(newStore, newMK, oldCount); err != nil {
		// Self-check failed — the new vault is incomplete or corrupt. Remove the
		// partial new files so a re-run doesn't trip the "already populated"
		// guard, then surface the error. We do NOT delete the old files here
		// (the source is untouched regardless of --keep-old: --keep-old means
		// "preserve source after success", not "keep a broken partial dest").
		// The just-written new master.key is also stray at this point (nothing
		// references it once newStore is gone) — remove it unconditionally.
		_ = os.Remove(newStore)
		_ = os.Remove(newStore + "-wal")
		_ = os.Remove(newStore + "-shm")
		_ = (store.FileKeyProvider{Path: newMK}).Delete()
		return fmt.Errorf("N/N self-check FAILED (new vault rolled back; old vault untouched): %w", err)
	}
	fmt.Fprintf(w, "self-check: %d/%d servers decrypt OK\n", oldCount, oldCount)

	// --- delete old (unless --keep-old). ---
	if !opts.KeepOld {
		if err := removeOldVault(oldStore, oldMK); err != nil {
			// Migration succeeded but cleanup failed — surface it but do NOT
			// roll back the new vault (that would destroy migrated data over a
			// housekeeping error). The user can rm the old files by hand.
			fmt.Fprintf(w, "WARNING: migration succeeded but failed to delete old files: %v (please remove %s, %s by hand)\n", err, oldStore, oldMK)
		} else {
			fmt.Fprintf(w, "removed old vault files at %s\n", oldDir)
		}
	} else {
		fmt.Fprintf(w, "kept old vault files at %s (--keep-old)\n", oldDir)
	}

	fmt.Fprintf(w, "migration complete: %d servers relocated to %s\n", oldCount, newStore)
	return nil
}

// GuidanceUnreadableBackendMsg returns the user-facing guidance string for an
// unreadable old backend at mkPath. Exported as a helper so callers building
// related error messages can compose with it consistently.
func GuidanceUnreadableBackendMsg(mkPath string) string {
	return fmt.Sprintf(guidanceUnreadableBackend, mkPath)
}

// fileExists reports true if path exists as any filesystem entry. A stat error
// other than ErrNotExist is surfaced to the caller.
func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// samePath reports whether two paths refer to the same file (best-effort:
// absolute + cleaned + OS-aware slash). Used to short-circuit same-location
// migrations.
func samePath(a, b string) bool {
	aa, errA := filepath.Abs(a)
	bb, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return filepath.Clean(aa) == filepath.Clean(bb)
}

// storeHasServers opens the store at path with the given master key and reports
// whether it contains any server row. Used to guard the destination against
// silent clobbering. A missing store.db → false, nil.
func storeHasServers(storePath string, mk []byte) (bool, error) {
	exists, err := fileExists(storePath)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	n, err := storeServerCount(storePath, mk)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// storeServerCount opens the store at path and returns the server count.
// Used both for the destination guard and for the N/N self-check (N before).
// The source store is Checkpoint'd (TRUNCATE) before close so the on-disk
// store.db file contains every committed row (the caller will byte-copy that
// file). Checkpoint failure is returned, not swallowed: an un-checkpointed WAL
// could hold UPDATE-only changes that the byte-copy would miss while the
// decrypt self-check still passes on the stale snapshot.
func storeServerCount(storePath string, mk []byte) (int, error) {
	st, err := store.Open(storePath, mk)
	if err != nil {
		return 0, err
	}
	defer st.Close()
	servers, err := st.ListServers()
	if err != nil {
		return 0, err
	}
	if err := st.Checkpoint(); err != nil {
		return 0, fmt.Errorf("checkpoint source store before copy: %w", err)
	}
	return len(servers), nil
}

// selfCheckDecryptAll opens the new store, lists every server, and decrypts
// each server's credential under the copied master key. Returns nil iff the
// server count matches expected AND every credential decrypts.
//
// This is the N/N check from spec §5.3: "迁移后 servers ls 数量 == 迁移前, 每条
// AuthForServer 可解". We decrypt directly via GetCredential (the same primitive
// AuthForServer uses) rather than building an ssh.AuthMethod — building the auth
// method would parse the private key bytes, which can spuriously fail on a valid
// password credential. Decrypt-or-fail is the real integrity signal.
// Credential-less servers (Plan 20 C0) carry no secret to verify and are skipped.
func selfCheckDecryptAll(storePath, mkPath string, expected int) error {
	mk, err := os.ReadFile(mkPath)
	if err != nil {
		return fmt.Errorf("read copied master key: %w", err)
	}
	st, err := store.Open(storePath, mk)
	if err != nil {
		return fmt.Errorf("open new store: %w", err)
	}
	defer st.Close()
	servers, err := st.ListServers()
	if err != nil {
		return fmt.Errorf("list servers: %w", err)
	}
	if len(servers) != expected {
		return fmt.Errorf("server count %d != expected %d", len(servers), expected)
	}
	for _, srv := range servers {
		// Credential-less servers (Plan 20 C0) have nothing to decrypt — skip
		// them, same as an absent sudo credential below.
		if srv.CredentialID == "" {
			continue
		}
		cred, err := st.GetCredential(srv.CredentialID)
		if err != nil {
			return fmt.Errorf("decrypt credential for server %q (%s): %w", srv.Name, srv.CredentialID, err)
		}
		if cred == nil {
			return fmt.Errorf("credential %s missing for server %q", srv.CredentialID, srv.Name)
		}
		// sudo credential (optional) must also decrypt if present
		if srv.SudoCredentialID != "" {
			sudoCred, err := st.GetCredential(srv.SudoCredentialID)
			if err != nil {
				return fmt.Errorf("decrypt sudo credential for server %q (%s): %w", srv.Name, srv.SudoCredentialID, err)
			}
			if sudoCred == nil {
				return fmt.Errorf("sudo credential %s missing for server %q", srv.SudoCredentialID, srv.Name)
			}
		}
	}
	return nil
}

// copyFile copies src to dst with the given mode (the mode is applied via Chmod
// after write so it's correct even when the source had different bits). Used
// for store.db — the master key is written through FileKeyProvider.Set instead.
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Chmod(mode); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// removeOldVault deletes the old store.db (+ WAL/SHM sidecars) and the old
// master key. Best-effort on sidecars: WAL/SHM may or may not exist depending
// on whether the store was opened this session. Idempotent: ErrNotExist on any
// of them is ignored.
func removeOldVault(storePath, mkPath string) error {
	for _, p := range []string{storePath, storePath + "-wal", storePath + "-shm", mkPath} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", p, err)
		}
	}
	return nil
}
