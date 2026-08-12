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
)

const backupMarkerName = ".ssh-manager-backup-marker"

// newBackupCmd builds the `backup` command tree (create + verify).
func newBackupCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "backup",
		Short: "Manage NAS plaintext vault backups",
	}
	c.AddCommand(newBackupCreateCmd(), newBackupVerifyCmd())
	return c
}

func newBackupVerifyCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "verify <file>",
		Short: "Verify a backup's SHA256 sidecar and JSON structure",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBackupVerify(cmd, args[0])
		},
	}
	return c
}

// runBackupVerify reads the .sha256 sidecar for file, recomputes the on-disk
// SHA256, asserts a match, and re-unmarshals into store.Snapshot to catch
// structural corruption / bit-rot. No passphrase (plaintext).
func runBackupVerify(cmd *cobra.Command, file string) error {
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

func newBackupCreateCmd() *cobra.Command {
	var dir string
	var keep int
	var prefix string
	c := &cobra.Command{
		Use:   "create --dir <backup-dir> [--keep 7] [--prefix vault]",
		Short: "Write a plaintext vault snapshot to --dir (skip if unchanged)",
		Long: `Create a plaintext JSON snapshot of the entire vault in --dir. Skips writing
if the latest existing backup's SHA256 matches (idle/static-vault optimization —
on an active server the audit log changes every run, so skip mostly fires only
in idle windows). Rotates to --keep most-recent files. Requires a marker file
(.ssh-manager-backup-marker) inside --dir as a mount-present guard.

The backup is PLAINTEXT (credentials in cleartext). Only safe on a trusted NAS
with no Cloud Sync / public sharing. See docs/backup-restore.md.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBackupCreate(cmd, dir, keep, prefix)
		},
	}
	c.Flags().StringVar(&dir, "dir", "", "backup target directory (must contain the marker file)")
	c.MarkFlagRequired("dir")
	c.Flags().IntVar(&keep, "keep", 7, "number of most-recent backups to keep (0 = no rotation)")
	c.Flags().StringVar(&prefix, "prefix", "vault", "backup filename prefix")
	return c
}

func runBackupCreate(cmd *cobra.Command, dir string, keep int, prefix string) error {
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
		return fmt.Errorf("--dir %s contains a .git directory — refusing to write plaintext credentials into a git working tree", dir)
	}
	// 3. lock
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
