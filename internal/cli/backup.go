package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/ops"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vaultio"
)

// validatePassphraseNotInDir 委托给运维模块的口令文件位置校验。
func validatePassphraseNotInDir(dir, passphraseFile string) error {
	return ops.ValidatePassphraseNotInDir(dir, passphraseFile)
}

// atomicWriteFile 委托给运维模块的原子写文件实现。
func atomicWriteFile(dir, name string, data []byte) (string, error) {
	return ops.AtomicWriteFile(dir, name, data)
}

// newBackupCmd builds the `backup` command tree (create + verify).
func newBackupCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "backup",
		Short: "Manage vault backups (plaintext NAS JSON and passphrase-encrypted .sme)",
	}
	c.AddCommand(newBackupCreateCmd(), newBackupVerifyCmd(), newBackupStatusCmd(), newBackupConfigCmd())
	return c
}

func newBackupVerifyCmd() *cobra.Command {
	var passphraseSrc string
	var latest bool
	c := &cobra.Command{
		Use:   "verify (<file> | --latest)",
		Short: "Verify a backup file (plaintext: SHA256 sidecar + JSON; encrypted: decrypt + JSON)",
		Args: func(cmd *cobra.Command, args []string) error {
			if latest {
				return cobra.NoArgs(cmd, args)
			}
			return cobra.ExactArgs(1)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if latest {
				return runBackupVerifyLatest(cmd)
			}
			return runBackupVerify(cmd, args[0], passphraseSrc)
		},
	}
	c.Flags().StringVar(&passphraseSrc, "passphrase-file", "", "passphrase file for encrypted (.sme) backups; ignored for plaintext JSON")
	c.Flags().BoolVar(&latest, "latest", false, "verify the newest backup selected by backup.json (passphrase file comes from the config)")
	return c
}

// newBackupStatusCmd 构建 `backup status`：输出与备份页相同的备份状态，
// 支持 --json 供脚本与监控使用。
func newBackupStatusCmd() *cobra.Command {
	var jsonOut bool
	c := &cobra.Command{
		Use:   "status [--json]",
		Short: "Show backup config, latest file, freshness and last verification",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBackupStatus(cmd, jsonOut)
		},
	}
	c.Flags().BoolVar(&jsonOut, "json", false, "output machine-readable JSON")
	return c
}

func runBackupStatus(cmd *cobra.Command, jsonOut bool) error {
	st, err := openUnlockedStore()
	if err != nil {
		return err
	}
	defer st.Close()
	status, err := ops.New(st).BackupStatus()
	if err != nil {
		return err
	}
	if jsonOut {
		data, err := json.MarshalIndent(backupStatusJSON(status), "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return nil
	}
	out := cmd.OutOrStdout()
	switch {
	case status.ConfigMissing:
		fmt.Fprintf(out, "config: missing (%s)\n", status.ConfigError)
	case status.Config == nil:
		fmt.Fprintf(out, "config: error (%s)\n", status.ConfigError)
	default:
		fmt.Fprintln(out, "config: ok")
		fmt.Fprintf(out, "dir: %s\n", status.Config.Dir)
		fmt.Fprintf(out, "keep: %d\n", status.Config.Keep)
		fmt.Fprintf(out, "passphrase_file: %s\n", status.Config.PassphraseFile)
	}
	fmt.Fprintf(out, "scheduled task: %s\n", backupStatusTaskText(status))
	// 配置缺失或有误时备份目录未知，数量与最新文件显示为未知。
	if status.Config == nil {
		return nil
	}
	fmt.Fprintf(out, "backups: %d\n", status.Total)
	if status.HasLatest {
		fmt.Fprintf(out, "latest: %s (generated %s)\n",
			status.LatestName, status.LatestTime.Format("2006-01-02 15:04:05"))
		if status.Stale {
			fmt.Fprintln(out, "freshness: stale (>25h since the newest backup)")
		} else {
			fmt.Fprintln(out, "freshness: ok")
		}
	} else {
		fmt.Fprintln(out, "latest: none")
		fmt.Fprintln(out, "freshness: n/a")
	}
	fmt.Fprintf(out, "verify: %s\n", backupStatusVerifyText(status))
	return nil
}

func backupStatusTaskText(status ops.BackupStatus) string {
	switch {
	case !status.TaskProbed:
		return "not probed (windows only)"
	case status.TaskInstalled:
		return "installed"
	default:
		return "not installed"
	}
}

func backupStatusVerifyText(status ops.BackupStatus) string {
	if status.Verify == nil {
		return "not verified"
	}
	at := status.Verify.At.Format("2006-01-02 15:04:05")
	if status.Verify.OK {
		return "ok at " + at
	}
	kind := status.Verify.FailureKind
	if kind == "" {
		kind = "unknown"
	}
	return "failed (" + kind + ") at " + at
}

func backupStatusJSON(status ops.BackupStatus) map[string]any {
	out := map[string]any{
		"config_path":    status.ConfigPath,
		"config_missing": status.ConfigMissing,
		"config_error":   status.ConfigError,
		"task_installed": status.TaskInstalled,
		"task_probed":    status.TaskProbed,
		"total":          status.Total,
	}
	if status.Config != nil {
		out["config"] = map[string]any{
			"dir":             status.Config.Dir,
			"keep":            status.Config.Keep,
			"passphrase_file": status.Config.PassphraseFile,
		}
	}
	if status.HasLatest {
		out["latest"] = status.LatestName
		out["latest_time"] = status.LatestTime.Unix()
		out["stale"] = status.Stale
	}
	if status.Verify != nil {
		out["verify"] = map[string]any{
			"file":         status.Verify.File,
			"ok":           status.Verify.OK,
			"failure_kind": status.Verify.FailureKind,
			"at":           status.Verify.At.Unix(),
		}
	}
	return out
}

func runBackupVerifyLatest(cmd *cobra.Command) error {
	st, err := openUnlockedStore()
	if err != nil {
		return err
	}
	defer st.Close()
	result, err := ops.New(st).VerifyLatestBackup(ops.CLISource())
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "ok: %s (decrypted and json structurally valid)\n", result.File)
	return nil
}

// runBackupVerify verifies a backup file. Encrypted (.sme / SSHMGRV1) backups:
// --passphrase-file is REQUIRED (an encrypted file cannot be checked without
// the key), then two gates — vaultio.Decrypt + re-unmarshal into
// store.Snapshot. Plaintext backups keep the unchanged sidecar path: read the
// .sha256 sidecar, recompute the on-disk SHA256, assert a match, and
// re-unmarshal. (A --passphrase-file passed on a plaintext file is ignored,
// mirroring `import`'s sniff semantics.)
func runBackupVerify(cmd *cobra.Command, file, passphraseSrc string) error {
	record := func(result, kind string, runErr error) {
		recordBackupVerifyAudit(cmd, filepath.Base(file), result, kind, runErr)
	}
	blob, err := os.ReadFile(file)
	if err != nil {
		record("error", "", err)
		return err
	}
	if vaultio.IsEncrypted(blob) {
		if err := ops.VerifyEncryptedBackupBlob(file, blob, passphraseSrc); err != nil {
			record("error", ops.BackupVerifyFailureKind(err), err)
			return err
		}
		record("ok", "", nil)
		fmt.Fprintf(cmd.OutOrStdout(), "ok: %s (decrypted and json structurally valid)\n", filepath.Base(file))
		return nil
	}
	wantSHA, ok := parseSidecar(file + ".sha256")
	if !ok {
		err := fmt.Errorf("missing or unreadable sidecar %s.sha256", file)
		record("error", "", err)
		return err
	}
	if err := verifyWritten(file, wantSHA); err != nil {
		record("error", "", err)
		return err
	}
	record("ok", "", nil)
	fmt.Fprintf(cmd.OutOrStdout(), "ok: %s (sha256 verified, json structurally valid)\n", filepath.Base(file))
	return nil
}

func newBackupCreateCmd() *cobra.Command {
	var dir string
	var keep int
	var prefix string
	var passphraseSrc string
	var configPath string
	c := &cobra.Command{
		Use:   "create [--config <backup.json>] [--dir <backup-dir>] [--passphrase-file <file>] [--keep 7] [--prefix vault]",
		Short: "Write a vault snapshot to the backup dir (plaintext JSON, or passphrase-encrypted .sme with a passphrase file)",
		Long: `Create a snapshot of the entire vault in the backup target directory.

The target directory and mode come from --config and/or --dir: --config reads a
backup.json (fields: dir, keep, passphrase_file — all three required, both
paths absolute) whose values act as defaults for the corresponding flags.
Priority: explicit flag > config value > built-in default. A passphrase file
in effect (flag or config) switches to encrypted mode; the passphrase file must
never live inside the backup directory (enforced on the merged values).

Two modes, selected by the passphrase file:
  - ENCRYPTED (passphrase file in effect): writes vault-<UTC>.sme — the same
    full snapshot JSON sealed in the SSHMGRV1 passphrase envelope (Argon2id +
    AES-256-GCM), the same format ` + "`export`" + ` writes and ` + "`import`" + ` reads. Before
    writing, the passphrase is checked against the newest existing .sme in the
    dir (a changed passphrase or corrupted file aborts the run — no silent
    lineage fork). After writing, the file is re-read, decrypted, and
    re-parsed. No .sha256 sidecar (the envelope is self-authenticating);
    rotation keeps the --keep most-recent .sme files only.
  - PLAINTEXT (default): writes vault-<UTC>.json + a .sha256 sidecar. Skips
    writing if the latest existing backup's SHA256 matches (idle/static-vault
    optimization — on an active server the audit log changes every run, so skip
    mostly fires only in idle windows). Rotates to --keep most-recent files.

    The backup is PLAINTEXT (credentials in cleartext). Only safe on a trusted
    NAS with no Cloud Sync / public sharing. See docs/backup-restore.md.

Both modes require a marker file (.ssh-manager-backup-marker) inside the dir as
a mount-present guard, refuse a .git working tree, and take the same lock.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBackupCreate(cmd, dir, keep, prefix, passphraseSrc, configPath)
		},
	}
	c.Flags().StringVar(&configPath, "config", "", "backup config JSON (fields: dir, keep, passphrase_file) used as defaults for the corresponding flags; flag > config > built-in default")
	c.Flags().StringVar(&dir, "dir", "", "backup target directory (must contain the marker file; may also come from --config)")
	c.Flags().IntVar(&keep, "keep", 7, "number of most-recent backups to keep (0 = no rotation)")
	c.Flags().StringVar(&prefix, "prefix", "vault", "backup filename prefix")
	c.Flags().StringVar(&passphraseSrc, "passphrase-file", "", "read the encryption passphrase from this file (non-interactive); presence switches to encrypted .sme mode")
	return c
}

// runBackupCreate resolves the effective settings (--config loaded and
// validated first, then merged with priority explicit flag > config value >
// built-in default) and dispatches to the encrypted or plaintext chain.
func runBackupCreate(cmd *cobra.Command, dir string, keep int, prefix string, passphraseSrc string, configPath string) error {
	cfg, err := loadBackupConfig(configPath)
	if err != nil {
		recordBackupCreateAuditFailure(cmd, "", ops.BackupFailureConfig, err)
		return err
	}
	if cfg != nil {
		if !cmd.Flags().Changed("dir") {
			dir = cfg.Dir
		}
		if !cmd.Flags().Changed("keep") {
			keep = cfg.Keep
		}
		if !cmd.Flags().Changed("passphrase-file") {
			passphraseSrc = cfg.PassphraseFile
		}
	}
	// --dir carries no cobra required-annotation anymore (a config file can
	// supply it), so the requirement is enforced here on the merged value.
	if dir == "" {
		err := fmt.Errorf("--dir is required (pass --dir, or --config with a dir field)")
		recordBackupCreateAuditFailure(cmd, "", ops.BackupFailureConfig, err)
		return err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		recordBackupCreateAuditFailure(cmd, "", "", err)
		return err
	}
	dir = abs
	// passphrase-location invariant on the EFFECTIVE values: an overriding
	// --passphrase-file must not smuggle the key into the backup dir either.
	// Abs first — a RELATIVE passphrase path resolves against cwd and would
	// short-circuit the volume-name comparison inside the subtree check
	// (ticket-02 review finding; lexical bypass, fixed 2026-09-29).
	if passphraseSrc != "" {
		passAbs, err := filepath.Abs(passphraseSrc)
		if err != nil {
			recordBackupCreateAuditFailure(cmd, "", "", err)
			return err
		}
		if err := validatePassphraseNotInDir(dir, passAbs); err != nil {
			recordBackupCreateAuditFailure(cmd, "", "", err)
			return err
		}
	}

	// 1. marker (mount-present guard) + 2. .git guardrail (self only, no
	// ancestor walk) — shared with the ops module's immediate-backup path.
	if err := ops.CheckBackupTargetDir(dir); err != nil {
		recordBackupCreateAuditFailure(cmd, "", "", err)
		return err
	}
	// 3. encrypted mode: --passphrase-file present => passphrase-encrypted .sme
	if passphraseSrc != "" {
		outcome, err := ops.CreateEncryptedBackup(ops.EncryptedBackupRequest{
			Dir: dir, Prefix: prefix, Keep: keep, PassphraseFile: passphraseSrc,
			SnapshotJSON: computeSnapshotJSON,
		})
		switch {
		case errors.Is(err, ops.ErrConcurrentBackup):
			recordBackupCreateAudit(cmd, "", "skipped", nil)
			fmt.Fprintln(cmd.OutOrStdout(), "another backup in progress; skipping")
			return nil
		case err != nil:
			recordBackupCreateAudit(cmd, outcome.Name, "error", err)
			return err
		}
		if outcome.RotationErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", outcome.RotationErr)
		}
		recordBackupCreateAudit(cmd, outcome.Name, "ok", nil)
		fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", outcome.Name)
		return nil
	}
	// 4. lock
	lk, err := ops.AcquireBackupLock(dir)
	if err != nil {
		if errors.Is(err, ops.ErrConcurrentBackup) {
			recordBackupCreateAudit(cmd, "", "skipped", nil)
			fmt.Fprintln(cmd.OutOrStdout(), "another backup in progress; skipping")
			return nil
		}
		recordBackupCreateAudit(cmd, "", "error", err)
		return err
	}
	defer lk.Release()

	// 4. snapshot + marshal + hash (reuse same []byte)
	data, err := computeSnapshotJSON()
	if err != nil {
		recordBackupCreateAudit(cmd, "", "error", err)
		return err
	}
	sum := sha256.Sum256(data)
	fileSHA := hex.EncodeToString(sum[:])

	// 5. skip check
	if shouldSkip(dir, prefix, fileSHA) {
		recordBackupCreateAudit(cmd, "", "skipped", nil)
		fmt.Fprintln(cmd.OutOrStdout(), "vault unchanged; skipping")
		return nil
	}

	// 6. atomic write
	name, err := nextBackupName(dir, prefix)
	if err != nil {
		recordBackupCreateAudit(cmd, "", "error", err)
		return err
	}
	finalPath, err := atomicWriteFile(dir, name, data)
	if err != nil {
		recordBackupCreateAudit(cmd, name, "error", err)
		return err
	}
	// 7. sidecar
	if err := writeSidecar(finalPath+".sha256", fileSHA); err != nil {
		return err
	}
	// 9. post-write verify (unmarshal + hash round-trip)
	if err := verifyWritten(finalPath, fileSHA); err != nil {
		recordBackupCreateAudit(cmd, name, "error", err)
		return fmt.Errorf("post-write verification failed for %s: %w", finalPath, err)
	}
	// 10. rotation (incl. orphan sidecar sweep)
	if keep > 0 {
		if err := rotateBackups(dir, prefix, keep); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: rotation error (backups left in place): %v\n", err)
		}
	}
	recordBackupCreateAudit(cmd, filepath.Base(finalPath), "ok", nil)
	fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", filepath.Base(finalPath))
	return nil
}

// recordBackupCreateAudit 把 `backup create` 的结果写成 backup.create 审计
// 行；计划任务无人值守执行时也经过这里，来源固定为命令行。审计只是附属
// 记录，写入失败时提示而不改变备份命令本身的退出结果。
func recordBackupCreateAudit(cmd *cobra.Command, file, result string, runErr error) {
	st, err := openUnlockedStore()
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: cannot open vault to write backup audit: %v\n", err)
		return
	}
	defer st.Close()
	if err := ops.New(st).RecordBackupCreate(ops.CLISource(), file, result, runErr); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: backup audit write failed: %v\n", err)
	}
}

// recordBackupCreateAuditFailure 记录尚未进入备份执行链的失败。vault 本身
// 打不开时没有审计表可写，只能向标准错误提示，这种路径保持只报错。
func recordBackupCreateAuditFailure(cmd *cobra.Command, file, kind string, runErr error) {
	st, err := openUnlockedStore()
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: cannot open vault to write backup audit: %v\n", err)
		return
	}
	defer st.Close()
	if err := ops.New(st).RecordBackupCreateFailure(ops.CLISource(), file, kind, runErr); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: backup audit write failed: %v\n", err)
	}
}

// recordBackupVerifyAudit 记录按路径校验的结果。vault 本身打不开时没有审
// 计表可写，只能向标准错误提示，这种路径保持只报错。
func recordBackupVerifyAudit(cmd *cobra.Command, file, result, kind string, runErr error) {
	st, err := openUnlockedStore()
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: cannot open vault to write backup audit: %v\n", err)
		return
	}
	defer st.Close()
	if err := ops.New(st).RecordBackupVerify(ops.CLISource(), file, result, kind, runErr); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: backup audit write failed: %v\n", err)
	}
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
