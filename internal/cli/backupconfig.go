package cli

import (
	"io"

	"ssh-manager-mcp/internal/ops"
	"ssh-manager-mcp/internal/tui"
)

// backupConfig 与运维模块共用同一个备份配置类型；命令行在这里保留薄包
// 装，让既有调用点继续使用原名。
type backupConfig = ops.BackupConfig

// loadBackupConfig 委托给运维模块的备份配置读取与校验。
func loadBackupConfig(path string) (*backupConfig, error) {
	return ops.LoadBackupConfig(path)
}

// ---------------------------------------------------------------------------
// TUI bridge (Plan 49 票 03). The broker console's 备份 page must share this
// file's validation and the backup chain byte-for-byte, but internal/tui cannot
// import internal/cli (cli imports tui for the `tui` command — import cycle),
// so the entry points are injected through tui.SetBackupEntry — the same bridge
// pattern as tui.SetServeInstaller (Plan 19 T4). The init here keeps the whole
// bridge inside this one file.
func init() {
	tui.SetBackupEntry(tui.BackupEntry{
		LoadConfig:     loadBackupConfigForTUI,
		ValidateValues: validateBackupConfigValuesForTUI,
		SaveConfig:     saveBackupConfigForTUI,
		Create:         runBackupCreateForTUI,
		Verify:         runBackupVerifyForTUI,
	})
}

// loadBackupConfigForTUI adapts LoadBackupConfig to the console: the exact
// same validation, surfaced through the exported view type.
func loadBackupConfigForTUI(path string) (*tui.BackupConfig, error) {
	cfg, err := ops.LoadBackupConfig(path)
	if err != nil || cfg == nil {
		return nil, err
	}
	return &tui.BackupConfig{Dir: cfg.Dir, Keep: cfg.Keep, PassphraseFile: cfg.PassphraseFile}, nil
}

// validateBackupConfigValuesForTUI applies LoadBackupConfig's value rules to
// in-memory form values — both paths absolute plus the passphrase-subtree
// invariant — so the console's save gate IS the CLI's gate, not a
// re-statement of it.
func validateBackupConfigValuesForTUI(dir, passphraseFile string) error {
	return ops.ValidateBackupConfigValues(dir, passphraseFile)
}

// saveBackupConfigForTUI writes the three fields through the SAME on-disk
// shape LoadBackupConfig reads and the same atomic write the backups themselves
// use (temp + fsync + rename, 0600).
func saveBackupConfigForTUI(configPath, dir string, keep int, passphraseFile string) error {
	return ops.SaveBackupConfig(configPath, dir, keep, passphraseFile)
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
