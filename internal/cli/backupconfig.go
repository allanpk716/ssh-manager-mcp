package cli

import "ssh-manager-mcp/internal/ops"

// backupConfig 与运维模块共用同一个备份配置类型；命令行在这里保留薄包
// 装，让既有调用点继续使用原名。
type backupConfig = ops.BackupConfig

// loadBackupConfig 委托给运维模块的备份配置读取与校验。
func loadBackupConfig(path string) (*backupConfig, error) {
	return ops.LoadBackupConfig(path)
}
