// Package webuiops 把运维模块适配成 Web UI 的窄接口，页面处理器因此不接
// 触数据库结构和文件布局。
package webuiops

import (
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

func opsAuditQuery(query webui.OpsAuditQuery) ops.AuditQuery {
	return ops.AuditQuery{
		Since: query.Since, Servers: query.Servers, Projects: query.Projects,
		OwnerOnly: query.OwnerOnly, Actions: query.Actions, Statuses: query.Statuses,
		Source: query.Source, SourceIP: query.SourceIP, BeforeID: query.BeforeID,
		Limit: query.Limit,
	}
}
