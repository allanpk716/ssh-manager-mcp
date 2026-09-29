package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"ssh-manager-mcp/internal/tui"
)

// backupConfig is a parsed and validated backup.json — the single source of
// truth for the backup target directory, retention count, and passphrase file
// location, shared by the scheduled task, the TUI, and manual CLI runs.
type backupConfig struct {
	Dir            string
	Keep           int
	PassphraseFile string
}

// backupConfigFile mirrors the on-disk JSON with pointer fields so a MISSING
// field stays distinguishable from a zero value (keep: 0 is legal — it means
// "no rotation" — while an absent keep is a config defect).
type backupConfigFile struct {
	Dir            *string `json:"dir"`
	Keep           *int    `json:"keep"`
	PassphraseFile *string `json:"passphrase_file"`
}

// loadBackupConfig reads and validates a backup.json. An empty path means "no
// --config given" and returns (nil, nil) — the caller keeps built-in defaults.
// Every defect (file missing, bad JSON, a missing required field, a relative
// path, the passphrase file inside the backup dir) is a hard error: fail
// closed, nothing runs.
func loadBackupConfig(path string) (*backupConfig, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("backup config: file not found: %s", path)
		}
		return nil, fmt.Errorf("backup config: reading %s: %w", path, err)
	}
	var cf backupConfigFile
	if err := json.Unmarshal(raw, &cf); err != nil {
		return nil, fmt.Errorf("backup config: %s is not valid JSON: %v", path, err)
	}
	var missing []string
	if cf.Dir == nil {
		missing = append(missing, `"dir"`)
	}
	if cf.Keep == nil {
		missing = append(missing, `"keep"`)
	}
	if cf.PassphraseFile == nil {
		missing = append(missing, `"passphrase_file"`)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("backup config: missing required field(s) %s in %s",
			strings.Join(missing, ", "), path)
	}
	if !filepath.IsAbs(*cf.Dir) {
		return nil, fmt.Errorf("backup config: %q must be an absolute path, got %q in %s",
			"dir", *cf.Dir, path)
	}
	if !filepath.IsAbs(*cf.PassphraseFile) {
		return nil, fmt.Errorf("backup config: %q must be an absolute path, got %q in %s",
			"passphrase_file", *cf.PassphraseFile, path)
	}
	if err := validatePassphraseNotInDir(*cf.Dir, *cf.PassphraseFile); err != nil {
		return nil, fmt.Errorf("backup config: %w", err)
	}
	return &backupConfig{Dir: *cf.Dir, Keep: *cf.Keep, PassphraseFile: *cf.PassphraseFile}, nil
}

// validatePassphraseNotInDir enforces the passphrase-location invariant: the
// passphrase file must never live inside the backup directory (or BE the
// directory), otherwise the key would be synced alongside the very ciphertext
// it unlocks.
func validatePassphraseNotInDir(dir, passphraseFile string) error {
	if pathInsideDirTree(filepath.Clean(passphraseFile), filepath.Clean(dir)) {
		return fmt.Errorf("passphrase_file %s must not be inside the backup dir %s — the passphrase file cannot live in the directory being backed up; move it outside the backup directory",
			passphraseFile, dir)
	}
	return nil
}

// pathInsideDirTree reports whether child is dir itself or lies anywhere under
// it. Both sides are expected cleaned; the volume (drive letter / UNC share)
// must match before the path-prefix test applies, and the comparison is
// case-folded on Windows where paths are case-insensitive.
func pathInsideDirTree(child, dir string) bool {
	volChild, volDir := filepath.VolumeName(child), filepath.VolumeName(dir)
	if !strings.EqualFold(volChild, volDir) {
		return false
	}
	restChild := strings.TrimPrefix(child, volChild)
	restDir := strings.TrimPrefix(dir, volDir)
	if runtime.GOOS == "windows" {
		restChild = strings.ToLower(restChild)
		restDir = strings.ToLower(restDir)
	}
	if restChild == restDir {
		return true // child IS the dir — counted as inside
	}
	if !strings.HasSuffix(restDir, string(filepath.Separator)) {
		restDir += string(filepath.Separator)
	}
	return strings.HasPrefix(restChild, restDir)
}

// ---------------------------------------------------------------------------
// TUI bridge (Plan 49 票 03). The broker console's 备份 page must share this
// file's validation and backup.go's create/verify chains byte-for-byte, but
// internal/tui cannot import internal/cli (cli imports tui for the `tui`
// command — import cycle), so the entry points are injected through
// tui.SetBackupEntry — the same bridge pattern as tui.SetServeInstaller
// (Plan 19 T4). The init here keeps the whole bridge inside this one file.
func init() {
	tui.SetBackupEntry(tui.BackupEntry{
		LoadConfig:     loadBackupConfigForTUI,
		ValidateValues: validateBackupConfigValuesForTUI,
		SaveConfig:     saveBackupConfigForTUI,
		Create:         runBackupCreateForTUI,
		Verify:         runBackupVerifyForTUI,
	})
}

// loadBackupConfigForTUI adapts loadBackupConfig to the console: the exact
// same validation, surfaced through the exported view type.
func loadBackupConfigForTUI(path string) (*tui.BackupConfig, error) {
	cfg, err := loadBackupConfig(path)
	if err != nil || cfg == nil {
		return nil, err
	}
	return &tui.BackupConfig{Dir: cfg.Dir, Keep: cfg.Keep, PassphraseFile: cfg.PassphraseFile}, nil
}

// validateBackupConfigValuesForTUI applies loadBackupConfig's value rules to
// in-memory form values — both paths absolute plus the passphrase-subtree
// invariant — so the console's save gate IS the CLI's gate, not a
// re-statement of it.
func validateBackupConfigValuesForTUI(dir, passphraseFile string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("backup config: %q must be an absolute path, got %q", "dir", dir)
	}
	if !filepath.IsAbs(passphraseFile) {
		return fmt.Errorf("backup config: %q must be an absolute path, got %q", "passphrase_file", passphraseFile)
	}
	return validatePassphraseNotInDir(dir, passphraseFile)
}

// saveBackupConfigForTUI writes the three fields through the SAME on-disk
// shape loadBackupConfig reads (backupConfigFile) and the same atomic write
// the backups themselves use (temp + fsync + rename, 0600).
func saveBackupConfigForTUI(configPath, dir string, keep int, passphraseFile string) error {
	data, err := json.Marshal(backupConfigFile{Dir: &dir, Keep: &keep, PassphraseFile: &passphraseFile})
	if err != nil {
		return err
	}
	_, err = atomicWriteFile(filepath.Dir(configPath), filepath.Base(configPath), data)
	return err
}

// runBackupCreateForTUI runs the exact `backup create --config <path>` chain
// (no other flag overridden — the config file is the single source, same as
// the scheduled task's invocation). Output lands in the given writers; the
// console shows the last line on its status bar.
func runBackupCreateForTUI(configPath string, stdout, stderr io.Writer) error {
	c := newBackupCreateCmd()
	c.SetOut(stdout)
	c.SetErr(stderr)
	if err := c.Flags().Set("config", configPath); err != nil {
		return err
	}
	return c.RunE(c, nil)
}

// runBackupVerifyForTUI runs the exact `backup verify <file>
// --passphrase-file <path>` gates (decrypt + re-parse for .sme) — the
// console's [v] and its save-time generation check both ride it.
func runBackupVerifyForTUI(file, passphraseSrc string, stdout io.Writer) error {
	c := newBackupVerifyCmd()
	c.SetOut(stdout)
	if err := c.Flags().Set("passphrase-file", passphraseSrc); err != nil {
		return err
	}
	return c.RunE(c, []string{file})
}
