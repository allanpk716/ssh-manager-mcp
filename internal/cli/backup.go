package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vaultio"
)

const backupMarkerName = ".ssh-manager-backup-marker"

// newBackupCmd builds the `backup` command tree (create + verify).
func newBackupCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "backup",
		Short: "Manage vault backups (plaintext NAS JSON and passphrase-encrypted .sme)",
	}
	c.AddCommand(newBackupCreateCmd(), newBackupVerifyCmd())
	return c
}

func newBackupVerifyCmd() *cobra.Command {
	var passphraseSrc string
	c := &cobra.Command{
		Use:   "verify <file>",
		Short: "Verify a backup file (plaintext: SHA256 sidecar + JSON; encrypted: decrypt + JSON)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBackupVerify(cmd, args[0], passphraseSrc)
		},
	}
	c.Flags().StringVar(&passphraseSrc, "passphrase-file", "", "passphrase file for encrypted (.sme) backups; ignored for plaintext JSON")
	return c
}

// runBackupVerify verifies a backup file. Encrypted (.sme / SSHMGRV1) backups:
// --passphrase-file is REQUIRED (an encrypted file cannot be checked without
// the key), then two gates — vaultio.Decrypt + re-unmarshal into
// store.Snapshot. Plaintext backups keep the unchanged sidecar path: read the
// .sha256 sidecar, recompute the on-disk SHA256, assert a match, and
// re-unmarshal. (A --passphrase-file passed on a plaintext file is ignored,
// mirroring `import`'s sniff semantics.)
func runBackupVerify(cmd *cobra.Command, file, passphraseSrc string) error {
	blob, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	if vaultio.IsEncrypted(blob) {
		return verifyEncryptedFile(cmd, file, blob, passphraseSrc)
	}
	wantSHA, ok := parseSidecar(file + ".sha256")
	if !ok {
		return fmt.Errorf("missing or unreadable sidecar %s.sha256", file)
	}
	if err := verifyWritten(file, wantSHA); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "ok: %s (sha256 verified, json structurally valid)\n", filepath.Base(file))
	return nil
}

// verifyEncryptedFile runs the two verify gates on an encrypted backup: the
// passphrase must decrypt it, and the plaintext must re-parse as a Snapshot.
func verifyEncryptedFile(cmd *cobra.Command, file string, blob []byte, passphraseSrc string) error {
	if passphraseSrc == "" {
		return fmt.Errorf("%s is a passphrase-encrypted backup; --passphrase-file is required", filepath.Base(file))
	}
	pw, err := readPassphraseFile(passphraseSrc)
	if err != nil {
		return err
	}
	plain, err := vaultio.Decrypt(pw, blob)
	if err != nil {
		return fmt.Errorf("decrypt failed (wrong passphrase, or the file is corrupted): %w", err)
	}
	if err := jsonUnmarshalSnapshot(plain); err != nil {
		return fmt.Errorf("re-unmarshal failed: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "ok: %s (decrypted and json structurally valid)\n", filepath.Base(file))
	return nil
}

func newBackupCreateCmd() *cobra.Command {
	var dir string
	var keep int
	var prefix string
	var passphraseSrc string
	c := &cobra.Command{
		Use:   "create --dir <backup-dir> [--passphrase-file <file>] [--keep 7] [--prefix vault]",
		Short: "Write a vault snapshot to --dir (plaintext JSON, or passphrase-encrypted .sme with --passphrase-file)",
		Long: `Create a snapshot of the entire vault in --dir.

Two modes, selected by --passphrase-file:
  - ENCRYPTED (--passphrase-file set): writes vault-<UTC>.sme — the same full
    snapshot JSON sealed in the SSHMGRV1 passphrase envelope (Argon2id +
    AES-256-GCM), the same format ` + "`export`" + ` writes and ` + "`import`" + ` reads. Before
    writing, the passphrase is checked against the newest existing .sme in --dir
    (a changed passphrase or corrupted file aborts the run — no silent lineage
    fork). After writing, the file is re-read, decrypted, and re-parsed. No
    .sha256 sidecar (the envelope is self-authenticating); rotation keeps the
    --keep most-recent .sme files only.
  - PLAINTEXT (default): writes vault-<UTC>.json + a .sha256 sidecar. Skips
    writing if the latest existing backup's SHA256 matches (idle/static-vault
    optimization — on an active server the audit log changes every run, so skip
    mostly fires only in idle windows). Rotates to --keep most-recent files.

    The backup is PLAINTEXT (credentials in cleartext). Only safe on a trusted
    NAS with no Cloud Sync / public sharing. See docs/backup-restore.md.

Both modes require a marker file (.ssh-manager-backup-marker) inside --dir as a
mount-present guard, refuse a .git working tree, and take the same lock.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBackupCreate(cmd, dir, keep, prefix, passphraseSrc)
		},
	}
	c.Flags().StringVar(&dir, "dir", "", "backup target directory (must contain the marker file)")
	c.MarkFlagRequired("dir")
	c.Flags().IntVar(&keep, "keep", 7, "number of most-recent backups to keep (0 = no rotation)")
	c.Flags().StringVar(&prefix, "prefix", "vault", "backup filename prefix")
	c.Flags().StringVar(&passphraseSrc, "passphrase-file", "", "read the encryption passphrase from this file (non-interactive); presence switches to encrypted .sme mode")
	return c
}

func runBackupCreate(cmd *cobra.Command, dir string, keep int, prefix string, passphraseSrc string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	dir = abs

	// 1. marker (mount-present guard)
	if !markerExists(dir) {
		return fmt.Errorf("marker file %s not found in --dir %s — refusing to write (is the NAS mounted? create the marker ON the mounted share after mounting)",
			backupMarkerName, dir)
	}
	// 2. .git guardrail (self only, no ancestor walk)
	if dirContainsGit(dir) {
		return fmt.Errorf("--dir %s contains a .git directory — refusing to write credentials into a git working tree", dir)
	}
	// 3. encrypted mode: --passphrase-file present => passphrase-encrypted .sme
	if passphraseSrc != "" {
		return runBackupCreateEncrypted(cmd, dir, prefix, keep, passphraseSrc)
	}
	// 4. lock
	lk, err := acquireBackupLock(dir)
	if err != nil {
		if errors.Is(err, ErrConcurrentBackup) {
			fmt.Fprintln(cmd.OutOrStdout(), "another backup in progress; skipping")
			return nil
		}
		return err
	}
	defer lk.Release()

	// 4. snapshot + marshal + hash (reuse same []byte)
	data, err := computeSnapshotJSON()
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	fileSHA := hex.EncodeToString(sum[:])

	// 5. skip check
	if shouldSkip(dir, prefix, fileSHA) {
		fmt.Fprintln(cmd.OutOrStdout(), "vault unchanged; skipping")
		return nil
	}

	// 6. atomic write
	name, err := nextBackupName(dir, prefix)
	if err != nil {
		return err
	}
	finalPath, err := atomicWriteFile(dir, name, data)
	if err != nil {
		return err
	}
	// 7. sidecar
	if err := writeSidecar(finalPath+".sha256", fileSHA); err != nil {
		return err
	}
	// 9. post-write verify (unmarshal + hash round-trip)
	if err := verifyWritten(finalPath, fileSHA); err != nil {
		return fmt.Errorf("post-write verification failed for %s: %w", finalPath, err)
	}
	// 10. rotation (incl. orphan sidecar sweep)
	if keep > 0 {
		if err := rotateBackups(dir, prefix, keep); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: rotation error (backups left in place): %v\n", err)
		}
	}
	fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", filepath.Base(finalPath))
	return nil
}

// runBackupCreateEncrypted writes prefix-<UTC>.sme: the same full-vault
// snapshot JSON as the plaintext path, sealed in the SSHMGRV1 passphrase
// envelope (Argon2id + AES-256-GCM, vaultio.Encrypt — the same format `export`
// writes and `import` reads). Marker/.git guards and the lock are shared with
// the plaintext path; it differs in: no skip-unchanged (the ciphertext differs
// every run), no .sha256 sidecar (the envelope is self-authenticating), a
// pre-flight passphrase generation check against the newest existing .sme, a
// post-write decrypt-verify, and rotation scoped to *.sme (a cohabiting
// .json/.sha256 lineage is neither rotated nor touched).
func runBackupCreateEncrypted(cmd *cobra.Command, dir, prefix string, keep int, passphraseSrc string) error {
	// resolve the passphrase BEFORE anything else — an unreadable or empty
	// passphrase must never produce (or clobber) a backup file.
	pw, err := readPassphraseFile(passphraseSrc)
	if err != nil {
		return err
	}
	if len(pw) == 0 {
		return fmt.Errorf("passphrase must not be empty")
	}

	lk, err := acquireBackupLock(dir)
	if err != nil {
		if errors.Is(err, ErrConcurrentBackup) {
			fmt.Fprintln(cmd.OutOrStdout(), "another backup in progress; skipping")
			return nil
		}
		return err
	}
	defer lk.Release()

	// pre-flight generation check: if this dir already holds encrypted backups
	// and the CURRENT passphrase cannot decrypt the newest one, the passphrase
	// has changed (or the newest file is corrupted) — fail closed rather than
	// writing a backup that silently forks from an unreadable lineage.
	if err := checkEncryptedGeneration(dir, prefix, pw); err != nil {
		return err
	}

	// snapshot + seal (same snapshot bytes as the plaintext path)
	data, err := computeSnapshotJSON()
	if err != nil {
		return err
	}
	blob, err := vaultio.Encrypt(pw, data)
	if err != nil {
		return err
	}

	name, err := nextEncryptedBackupName(dir, prefix)
	if err != nil {
		return err
	}
	finalPath, err := atomicWriteFile(dir, name, blob)
	if err != nil {
		return err
	}
	// post-write verification: re-read + decrypt + re-parse (the encrypted
	// counterpart of verifyWritten's hash + re-parse)
	if err := verifyEncryptedWritten(finalPath, pw); err != nil {
		return fmt.Errorf("post-write verification failed for %s: %w", finalPath, err)
	}
	// rotation scoped to *.sme — .json backups sharing the dir are untouched
	if keep > 0 {
		if err := rotateEncryptedBackups(dir, prefix, keep); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: rotation error (backups left in place): %v\n", err)
		}
	}
	fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", filepath.Base(finalPath))
	return nil
}

// latestEncryptedBackup returns the lexicographically-greatest <prefix>-*.sme
// path — the same newest-first ordering rule the rotation uses.
func latestEncryptedBackup(dir, prefix string) (string, bool) {
	matches, err := filepath.Glob(filepath.Join(dir, prefix+"-*.sme"))
	if err != nil || len(matches) == 0 {
		return "", false
	}
	sort.Strings(matches)
	return matches[len(matches)-1], true
}

// checkEncryptedGeneration implements the pre-flight passphrase generation
// check: with existing *.sme files present, the current passphrase must decrypt
// the newest one. No prior .sme => nothing to check, return nil.
func checkEncryptedGeneration(dir, prefix string, pw []byte) error {
	latest, ok := latestEncryptedBackup(dir, prefix)
	if !ok {
		return nil
	}
	blob, err := os.ReadFile(latest)
	if err != nil {
		return fmt.Errorf("generation check: reading the latest encrypted backup %s: %w", filepath.Base(latest), err)
	}
	// Decrypt cannot distinguish a wrong passphrase from a tampered blob (GCM
	// auth failure) — name both causes, per spec.
	if _, err := vaultio.Decrypt(pw, blob); err != nil {
		return fmt.Errorf("generation check failed: cannot decrypt the latest encrypted backup %s with this passphrase (the passphrase changed, or the file is corrupted) — refusing to write a new encrypted backup", filepath.Base(latest))
	}
	return nil
}

// nextEncryptedBackupName picks vault-<UTC>.sme, appending -2/-3 on
// same-second collision — the exact mechanism nextBackupName uses for .json.
func nextEncryptedBackupName(dir, prefix string) (string, error) {
	base := prefix + "-" + time.Now().UTC().Format("20060102-150405")
	name := base + ".sme"
	for n := 2; ; n++ {
		if _, err := os.Stat(filepath.Join(dir, name)); os.IsNotExist(err) {
			return name, nil
		} else if err != nil {
			return "", err
		}
		name = fmt.Sprintf("%s-%d.sme", base, n)
		if n > 99 {
			return "", fmt.Errorf("too many same-second collisions for %s", base)
		}
	}
}

// verifyEncryptedWritten re-reads the just-written .sme, decrypts it with pw,
// and re-unmarshals the plaintext into store.Snapshot — catching truncation,
// corruption, or a passphrase mismatch before the run reports success.
func verifyEncryptedWritten(path string, pw []byte) error {
	blob, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	plain, err := vaultio.Decrypt(pw, blob)
	if err != nil {
		return fmt.Errorf("decrypt failed: %w", err)
	}
	if err := jsonUnmarshalSnapshot(plain); err != nil {
		return fmt.Errorf("re-unmarshal failed: %w", err)
	}
	return nil
}

// rotateEncryptedBackups keeps the `keep` lexicographically-greatest
// <prefix>-*.sme and deletes the rest. Mirrors rotateBackups but scoped to the
// .sme extension (no sidecars exist to sweep): .json/.sha256 backups sharing
// the dir are neither rotated nor touched.
func rotateEncryptedBackups(dir, prefix string, keep int) error {
	matches, err := filepath.Glob(filepath.Join(dir, prefix+"-*.sme"))
	if err != nil {
		return err
	}
	if len(matches) <= keep {
		return nil
	}
	sort.Sort(sort.Reverse(sort.StringSlice(matches))) // newest first
	for _, p := range matches[keep:] {
		// No sidecar pairing here: a failed remove (real error — e.g. EACCES,
		// or another process holds an open handle on Windows) just leaves the
		// .sme in place and moves on.
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			continue
		}
	}
	return nil
}

// computeSnapshotJSON opens the unlocked vault, exports, and marshals with the
// SAME indent as export.go (2-space) for determinism.
func computeSnapshotJSON() ([]byte, error) {
	st, err := openUnlockedStore()
	if err != nil {
		return nil, err
	}
	defer st.Close()
	snap, err := st.ExportSnapshot()
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(snap, "", "  ")
}

func markerExists(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, backupMarkerName))
	return err == nil
}

func dirContainsGit(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// shouldSkip reports whether the latest existing backup's sidecar matches fileSHA.
// Missing/unreadable sidecar => false (fail-open: write a new backup).
func shouldSkip(dir, prefix, fileSHA string) bool {
	latest, ok := latestBackup(dir, prefix)
	if !ok {
		return false
	}
	stored, ok := parseSidecar(latest + ".sha256")
	if !ok {
		return false
	}
	return stored == fileSHA
}

// latestBackup returns the lexicographically-greatest <prefix>-*.json path.
func latestBackup(dir, prefix string) (string, bool) {
	matches, err := filepath.Glob(filepath.Join(dir, prefix+"-*.json"))
	if err != nil || len(matches) == 0 {
		return "", false
	}
	sort.Strings(matches)
	return matches[len(matches)-1], true
}

// nextBackupName picks vault-<UTC>.json, appending -2/-3 on same-second collision.
func nextBackupName(dir, prefix string) (string, error) {
	base := prefix + "-" + time.Now().UTC().Format("20060102-150405")
	name := base + ".json"
	for n := 2; ; n++ {
		if _, err := os.Stat(filepath.Join(dir, name)); os.IsNotExist(err) {
			return name, nil
		} else if err != nil {
			return "", err
		}
		name = fmt.Sprintf("%s-%d.json", base, n)
		if n > 99 {
			return "", fmt.Errorf("too many same-second collisions for %s", base)
		}
	}
}

// atomicWriteFile writes data to a temp file (0600), fsyncs, renames to final.
// Cleans up the temp on any failure. fsyncs the parent dir on non-Windows.
func atomicWriteFile(dir, name string, data []byte) (string, error) {
	final := filepath.Join(dir, name)
	tmp, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op after successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil { // file fsync — all platforms
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpPath, final); err != nil {
		return "", err
	}
	// parent dir fsync — Linux/macOS; Windows has no dir-sync semantics.
	if runtime.GOOS != "windows" {
		if d, err := os.Open(dir); err == nil {
			_ = d.Sync() // best-effort
			d.Close()
		}
	}
	return final, nil
}

func writeSidecar(path, fileSHA string) error {
	content := "file_sha256=" + fileSHA + "\n"
	return os.WriteFile(path, []byte(content), 0o600)
}

func parseSidecar(path string) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "file_sha256=") {
			return strings.TrimPrefix(line, "file_sha256="), true
		}
	}
	return "", false
}

// verifyWritten re-reads the on-disk file, re-hashes it, and re-unmarshals it
// into store.Snapshot to catch structural corruption / half-writes.
func verifyWritten(path, wantSHA string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != wantSHA {
		return fmt.Errorf("sha256 mismatch: on-disk changed since write")
	}
	// structural unmarshal into the real Snapshot type (spec §5.2.9)
	if err := jsonUnmarshalSnapshot(b); err != nil {
		return fmt.Errorf("re-unmarshal failed: %w", err)
	}
	return nil
}

// jsonUnmarshalSnapshot validates that the on-disk bytes still parse as a
// store.Snapshot — catches truncation / corrupt JSON that a bare hash check
// alone would miss if the hash sidecar were also corrupted.
func jsonUnmarshalSnapshot(b []byte) error {
	var snap store.Snapshot
	return json.Unmarshal(b, &snap)
}

// rotateBackups keeps the `keep` lexicographically-greatest <prefix>-*.json
// (i.e. the newest by UTC timestamp), deleting the rest along with their
// sidecars. Then sweeps orphan sidecars (no matching .json).
func rotateBackups(dir, prefix string, keep int) error {
	matches, err := filepath.Glob(filepath.Join(dir, prefix+"-*.json"))
	if err != nil {
		return err
	}
	if len(matches) <= keep {
		// still sweep orphans even when no json rotation needed
		return sweepOrphanSidecars(dir, prefix, matches)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(matches))) // newest first
	for _, p := range matches[keep:] {
		// Delete .json first. If THAT remove fails (real error — e.g. EACCES,
		// or another process holds an open handle on Windows), we MUST also
		// skip the sidecar remove: keeping .json + .sha256 as a consistent
		// pair is strictly better than an orphan .json whose sidecar was just
		// deleted (the orphan would force the next run's sidecar read to
		// fail-open and write a redundant backup). sweepOrphanSidecars below
		// only removes sidecars whose .json is gone, so a surviving .json
		// also shields its sidecar from the sweep.
		if err := os.Remove(p); err != nil {
			if !os.IsNotExist(err) {
				// .json delete failed — keep the pair in place, move on.
				continue
			}
			// .json was already gone — fall through to clean up its now-orphan sidecar.
		}
		if err := os.Remove(p + ".sha256"); err != nil && !os.IsNotExist(err) {
			continue
		}
	}
	kept := matches[:keep]
	return sweepOrphanSidecars(dir, prefix, kept)
}

// sweepOrphanSidecars deletes any <prefix>-*.json.sha256 whose .json is absent.
func sweepOrphanSidecars(dir, prefix string, kept []string) error {
	keptSet := make(map[string]bool, len(kept))
	for _, p := range kept {
		keptSet[p] = true
	}
	sidecars, err := filepath.Glob(filepath.Join(dir, prefix+"-*.json.sha256"))
	if err != nil {
		return err
	}
	for _, sc := range sidecars {
		jsonPath := strings.TrimSuffix(sc, ".sha256")
		if !keptSet[jsonPath] {
			// json absent (either rotated above or pre-existing orphan) => remove sidecar
			if _, err := os.Stat(jsonPath); os.IsNotExist(err) {
				os.Remove(sc) // best-effort
			}
		}
	}
	return nil
}
