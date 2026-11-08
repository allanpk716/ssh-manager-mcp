package ops

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// BackupConfigError 标明备份配置的哪个字段没有通过校验；界面据此把错误放
// 回对应表单项。
type BackupConfigError struct {
	Field string
	Err   error
}

func (e *BackupConfigError) Error() string { return e.Err.Error() }
func (e *BackupConfigError) Unwrap() error { return e.Err }

// BackupConfigErrorField 从错误里取备份配置字段名；不是配置字段错误时返回
// 空串。
func BackupConfigErrorField(err error) string {
	var configErr *BackupConfigError
	if errors.As(err, &configErr) {
		return configErr.Field
	}
	return ""
}

// ReadBackupConfig 读取标准位置的备份配置。配置缺失或有误时返回错误；
// 调用方需要引导信息时可改用 BackupStatus。
func (m *Manager) ReadBackupConfig() (BackupConfig, string, error) {
	path, err := m.backupConfigPath()
	if err != nil {
		return BackupConfig{}, "", fmt.Errorf("定位备份配置失败：%w", err)
	}
	cfg, err := LoadBackupConfig(path)
	if err != nil {
		return BackupConfig{}, path, err
	}
	if cfg == nil {
		return BackupConfig{}, path, errors.New("backup config path is empty")
	}
	return *cfg, path, nil
}

// SaveBackupConfig 校验并保存备份配置，然后写 backup.config 审计行。修改
// 口令文件路径时，会先用新口令试解现有最新备份，避免后续备份分属两套口令。
func (m *Manager) SaveBackupConfig(config BackupConfig, source Source) error {
	if err := validateBackupConfigFields(config); err != nil {
		return err
	}
	path, err := m.backupConfigPath()
	if err != nil {
		return fmt.Errorf("定位备份配置失败：%w", err)
	}
	// 当前配置有误时也允许按表单值修复；没有旧值时按新目录寻找既有备份。
	current, _ := LoadBackupConfig(path)
	previous := config
	if current != nil {
		previous = *current
	}
	if current == nil || previous.PassphraseFile != config.PassphraseFile {
		if err := checkNewPassphraseForExistingBackups(previous, config); err != nil {
			return &BackupConfigError{Field: "passphrase_file", Err: err}
		}
	}
	if current != nil && *current == config {
		return nil
	}
	if err := SaveBackupConfig(path, config.Dir, config.Keep, config.PassphraseFile); err != nil {
		return err
	}
	return m.writeAudit("backup.config", backupConfigChanges(current, config), nil, source)
}

// validateBackupConfigFields 把三项配置的值校验包成带字段名的错误。
func validateBackupConfigFields(config BackupConfig) error {
	if config.Keep < 0 {
		return &BackupConfigError{
			Field: "keep",
			Err:   fmt.Errorf("backup config: %q must be zero or a positive integer, got %d", "keep", config.Keep),
		}
	}
	if err := ValidateBackupConfigValues(config.Dir, config.PassphraseFile); err != nil {
		field := "dir"
		if config.Dir != "" && filepath.IsAbs(config.Dir) {
			field = "passphrase_file"
		}
		return &BackupConfigError{Field: field, Err: err}
	}
	return nil
}

// checkNewPassphraseForExistingBackups 用新口令文件试解旧配置目录与新配置
// 目录里的最新备份。目录里没有备份时不拦截，允许首次配置。
func checkNewPassphraseForExistingBackups(current, next BackupConfig) error {
	dirs := []string{current.Dir}
	if next.Dir != current.Dir {
		dirs = append(dirs, next.Dir)
	}
	for _, dir := range dirs {
		latest, ok := LatestEncryptedBackup(dir, backupPrefix)
		if !ok {
			continue
		}
		blob, err := os.ReadFile(latest.Path)
		if err != nil {
			return fmt.Errorf("读取现有最新备份 %s 失败：%w", latest.Name, err)
		}
		if err := VerifyEncryptedBackupBlob(latest.Path, blob, next.PassphraseFile); err != nil {
			return fmt.Errorf("新口令无法解开现有最新备份 %s，已拒绝修改配置：%w", latest.Name, err)
		}
	}
	return nil
}

// backupConfigChanges 只把发生变化的字段放进审计摘要。口令文件路径可以记，
// 但绝不读取或记录口令内容。
func backupConfigChanges(current *BackupConfig, next BackupConfig) map[string]any {
	changes := make(map[string]any)
	if current == nil || current.Dir != next.Dir {
		changes["dir"] = next.Dir
	}
	if current == nil || current.Keep != next.Keep {
		changes["keep"] = next.Keep
	}
	if current == nil || current.PassphraseFile != next.PassphraseFile {
		changes["passphrase_file"] = next.PassphraseFile
	}
	return changes
}
