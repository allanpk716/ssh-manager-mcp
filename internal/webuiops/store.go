// Package webuiops 把运维模块适配成 Web UI 的窄接口，页面处理器因此不接
// 触数据库结构和文件布局。
package webuiops

import (
	"errors"

	"ssh-manager-mcp/internal/ops"
	"ssh-manager-mcp/internal/webui"
)

type managerStore struct {
	manager *ops.Manager
}

type openerStore struct {
	open func() (*ops.Manager, func(), error)
}

// FromManager 把一个已挂在解锁存储上的运维模块适配给 Web UI。
func FromManager(manager *ops.Manager) webui.OpsStore {
	return managerStore{manager: manager}
}

// FromOpener 每次页面请求重新打开存储并挂上运维模块，调用结束后关闭。单
// 机回环 Web UI 没有常驻进程持有存储句柄，用这个入口避免长期占用。
func FromOpener(open func() (*ops.Manager, func(), error)) webui.OpsStore {
	return openerStore{open: open}
}

// clientAuditStore 只提供客户端本机审计读取，给客户端角色的回环 Web UI 使
// 用；权威 vault 审计在客户端角色没有入口，调用时返回明确错误。
type clientAuditStore struct{}

// ClientAuditStore 返回客户端角色的运维窄接口适配器。
func ClientAuditStore() webui.OpsStore {
	return clientAuditStore{}
}

func (clientAuditStore) QueryAudit(query webui.OpsAuditQuery) ([]webui.OpsAuditRow, error) {
	return nil, errors.New("客户端角色没有权威 vault 审计入口")
}

func (clientAuditStore) QueryClientAudit(instance string, limit int) ([]webui.OpsAuditRow, error) {
	return clientAuditRows(instance, limit)
}

func (clientAuditStore) ListSettings() ([]webui.OpsSetting, error) {
	return nil, errors.New("客户端角色没有设置管理入口")
}

func (clientAuditStore) ValidateSetting(key, value string) error {
	return errors.New("客户端角色没有设置管理入口")
}

func (clientAuditStore) SetSetting(key, value string, source webui.OpsWriteSource) error {
	return errors.New("客户端角色没有设置管理入口")
}

func (clientAuditStore) UnsetSetting(key string, source webui.OpsWriteSource) error {
	return errors.New("客户端角色没有设置管理入口")
}

func (clientAuditStore) ReadRole() (webui.OpsRole, error) {
	return webui.OpsRole{}, errors.New("客户端角色没有设置管理入口")
}

func (clientAuditStore) BackupStatus() (webui.OpsBackupStatus, error) {
	return webui.OpsBackupStatus{}, errors.New("客户端角色没有备份管理入口")
}

func (clientAuditStore) CreateBackup(source webui.OpsWriteSource) (webui.OpsBackupCreateResult, error) {
	return webui.OpsBackupCreateResult{}, errors.New("客户端角色没有备份管理入口")
}

func (clientAuditStore) VerifyLatestBackup(source webui.OpsWriteSource) (webui.OpsBackupVerifyResult, error) {
	return webui.OpsBackupVerifyResult{}, errors.New("客户端角色没有备份管理入口")
}

func (s managerStore) QueryClientAudit(instance string, limit int) ([]webui.OpsAuditRow, error) {
	return clientAuditRows(instance, limit)
}

func (s openerStore) QueryClientAudit(instance string, limit int) ([]webui.OpsAuditRow, error) {
	return clientAuditRows(instance, limit)
}

// clientAuditRows 把运维模块读到的客户端本机审计行转换成 Web UI 展示行。
func clientAuditRows(instance string, limit int) ([]webui.OpsAuditRow, error) {
	rows, err := ops.ReadClientAudit(instance, limit)
	if err != nil {
		return nil, err
	}
	out := make([]webui.OpsAuditRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, webui.OpsAuditRow{
			ID: row.ID, TS: row.TS, ProjectID: row.ProjectID, ServerID: row.ServerID,
			Action: row.Action, Command: row.Command, Sudo: row.Sudo, Status: row.Status,
			ExitCode: row.ExitCode, DurationMS: row.DurationMS,
			Source: row.Source, SourceIP: row.SourceIP,
		})
	}
	return out, nil
}

func (s openerStore) withManager(operation func(*ops.Manager) error) error {
	manager, closeStore, err := s.open()
	if err != nil {
		return err
	}
	defer closeStore()
	return operation(manager)
}

func (s managerStore) QueryAudit(query webui.OpsAuditQuery) ([]webui.OpsAuditRow, error) {
	rows, err := s.manager.QueryAudit(opsAuditQuery(query))
	if err != nil {
		return nil, err
	}
	out := make([]webui.OpsAuditRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, webui.OpsAuditRow{
			ID: row.ID, TS: row.TS, ProjectID: row.ProjectID, ProjectName: row.ProjectName,
			ServerID: row.ServerID, ServerName: row.ServerName, Action: row.Action,
			Command: row.Command, Sudo: row.Sudo, Status: row.Status,
			ExitCode: row.ExitCode, DurationMS: row.DurationMS,
			Source: row.Source, SourceIP: row.SourceIP,
		})
	}
	return out, nil
}

func (s managerStore) ListSettings() ([]webui.OpsSetting, error) {
	rows, err := s.manager.ListSettings()
	if err != nil {
		return nil, err
	}
	out := make([]webui.OpsSetting, 0, len(rows))
	for _, row := range rows {
		out = append(out, webui.OpsSetting{
			Key: row.Key, Type: webui.OpsSettingType(row.Type),
			StoredValue: row.StoredValue, CurrentValue: row.CurrentValue,
			DefaultValue: row.DefaultValue, Source: webui.OpsSettingSource(row.Source),
			Overridden: row.Overridden, CanBeOverridden: row.CanBeOverridden,
			Effect: row.Effect, Choices: opsChoices(row.Choices),
		})
	}
	return out, nil
}

func (s managerStore) ValidateSetting(key, value string) error {
	return s.manager.ValidateSetting(key, value)
}

func (s managerStore) SetSetting(key, value string, source webui.OpsWriteSource) error {
	return s.manager.SetSetting(key, value, ops.WebUISource(source.RemoteAddress))
}

func (s managerStore) UnsetSetting(key string, source webui.OpsWriteSource) error {
	return s.manager.UnsetSetting(key, ops.WebUISource(source.RemoteAddress))
}

func (s managerStore) ReadRole() (webui.OpsRole, error) {
	role, err := ops.ReadRole()
	if err != nil {
		return webui.OpsRole{}, err
	}
	return webui.OpsRole{
		RoleName: string(role.Role), SetupComplete: role.SetupComplete, Path: role.Path,
	}, nil
}

func (s openerStore) QueryAudit(query webui.OpsAuditQuery) ([]webui.OpsAuditRow, error) {
	var rows []webui.OpsAuditRow
	err := s.withManager(func(manager *ops.Manager) error {
		got, err := managerStore{manager: manager}.QueryAudit(query)
		rows = got
		return err
	})
	return rows, err
}

func (s managerStore) BackupStatus() (webui.OpsBackupStatus, error) {
	status, err := s.manager.BackupStatus()
	if err != nil {
		return webui.OpsBackupStatus{}, err
	}
	return webuiOpsBackupStatus(status), nil
}

func (s openerStore) BackupStatus() (webui.OpsBackupStatus, error) {
	var status webui.OpsBackupStatus
	err := s.withManager(func(manager *ops.Manager) error {
		got, err := managerStore{manager: manager}.BackupStatus()
		status = got
		return err
	})
	return status, err
}

func (s managerStore) CreateBackup(source webui.OpsWriteSource) (webui.OpsBackupCreateResult, error) {
	result, err := s.manager.CreateBackup(ops.WebUISource(source.RemoteAddress))
	return webui.OpsBackupCreateResult{File: result.File, Skipped: result.Skipped}, err
}

func (s openerStore) CreateBackup(source webui.OpsWriteSource) (webui.OpsBackupCreateResult, error) {
	var result webui.OpsBackupCreateResult
	err := s.withManager(func(manager *ops.Manager) error {
		got, err := managerStore{manager: manager}.CreateBackup(source)
		result = got
		return err
	})
	return result, err
}

func (s managerStore) VerifyLatestBackup(source webui.OpsWriteSource) (webui.OpsBackupVerifyResult, error) {
	result, err := s.manager.VerifyLatestBackup(ops.WebUISource(source.RemoteAddress))
	return webui.OpsBackupVerifyResult{
		File: result.File, OK: result.OK, FailureKind: result.FailureKind,
	}, err
}

func (s openerStore) VerifyLatestBackup(source webui.OpsWriteSource) (webui.OpsBackupVerifyResult, error) {
	var result webui.OpsBackupVerifyResult
	err := s.withManager(func(manager *ops.Manager) error {
		got, err := managerStore{manager: manager}.VerifyLatestBackup(source)
		result = got
		return err
	})
	return result, err
}

func webuiOpsBackupStatus(status ops.BackupStatus) webui.OpsBackupStatus {
	out := webui.OpsBackupStatus{
		ConfigPath: status.ConfigPath, ConfigError: status.ConfigError,
		ConfigMissing: status.ConfigMissing,
		TaskInstalled: status.TaskInstalled, TaskProbed: status.TaskProbed,
		TotalBackups: status.Total, LatestBackup: status.LatestName,
		LatestTime: status.LatestTime, HasLatest: status.HasLatest, Stale: status.Stale,
	}
	if status.Config != nil {
		out.ConfigDir = status.Config.Dir
		out.ConfigKeep = status.Config.Keep
		out.ConfigPassphraseFile = status.Config.PassphraseFile
	}
	if status.Verify != nil {
		out.Verify = &webui.OpsBackupVerifyState{
			File: status.Verify.File, OK: status.Verify.OK,
			FailureKind: status.Verify.FailureKind, At: status.Verify.At,
		}
	}
	return out
}

func (s openerStore) ListSettings() ([]webui.OpsSetting, error) {
	var rows []webui.OpsSetting
	err := s.withManager(func(manager *ops.Manager) error {
		got, err := managerStore{manager: manager}.ListSettings()
		rows = got
		return err
	})
	return rows, err
}

func (s openerStore) ValidateSetting(key, value string) error {
	return s.withManager(func(manager *ops.Manager) error {
		return managerStore{manager: manager}.ValidateSetting(key, value)
	})
}

func (s openerStore) SetSetting(key, value string, source webui.OpsWriteSource) error {
	return s.withManager(func(manager *ops.Manager) error {
		return managerStore{manager: manager}.SetSetting(key, value, source)
	})
}

func (s openerStore) UnsetSetting(key string, source webui.OpsWriteSource) error {
	return s.withManager(func(manager *ops.Manager) error {
		return managerStore{manager: manager}.UnsetSetting(key, source)
	})
}

func (s openerStore) ReadRole() (webui.OpsRole, error) {
	var role webui.OpsRole
	err := s.withManager(func(manager *ops.Manager) error {
		got, err := managerStore{manager: manager}.ReadRole()
		role = got
		return err
	})
	return role, err
}

func opsChoices(choices []ops.SettingChoice) []webui.OpsSettingChoice {
	if len(choices) == 0 {
		return nil
	}
	out := make([]webui.OpsSettingChoice, 0, len(choices))
	for _, choice := range choices {
		out = append(out, webui.OpsSettingChoice{Value: choice.Value, Label: choice.Label})
	}
	return out
}

func opsAuditQuery(query webui.OpsAuditQuery) ops.AuditQuery {
	return ops.AuditQuery{
		Since: query.Since, Servers: query.Servers, Projects: query.Projects,
		OwnerOnly: query.OwnerOnly, Actions: query.Actions, Statuses: query.Statuses,
		Source: query.Source, SourceIP: query.SourceIP, BeforeID: query.BeforeID,
		Limit: query.Limit,
	}
}
