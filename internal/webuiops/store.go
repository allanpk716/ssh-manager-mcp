// Package webuiops 把运维模块适配成 Web UI 的窄接口，页面处理器因此不接
// 触数据库结构和文件布局。
package webuiops

import (
	"context"
	"errors"

	"ssh-manager-mcp/internal/ops"
	"ssh-manager-mcp/internal/webui"
)

type managerStore struct {
	manager *ops.Manager
	updates *ops.UpdateService
}

type openerStore struct {
	open    func() (*ops.Manager, func(), error)
	updates *ops.UpdateService
}

// FromManager 把一个已挂在解锁存储上的运维模块适配给 Web UI。
func FromManager(manager *ops.Manager) webui.OpsStore {
	return managerStore{manager: manager}
}

// errUpdatesNotWired 表示适配器未挂升级服务；权威端升级页接入前不会走到。
var errUpdatesNotWired = errors.New("升级服务未接线")

// FromManagerWithUpdates 在运维模块上挂升级服务；升级服务由调用方在进程内
// 共享，保证同一时间只允许一个升级。
func FromManagerWithUpdates(manager *ops.Manager, updates *ops.UpdateService) webui.OpsStore {
	return managerStore{manager: manager, updates: updates}
}

// UpdatesOnly 提供只含升级能力的适配器；客户端没有权威 vault 审计表，其
// 审计查询按不可用处理。
func UpdatesOnly(updates *ops.UpdateService) webui.OpsStore {
	return openerStore{open: func() (*ops.Manager, func(), error) {
		return nil, func() {}, nil
	}, updates: updates}
}

// FromOpenerWithUpdates 组合按请求打开存储的运维模块与进程内共享的升级服务。
func FromOpenerWithUpdates(open func() (*ops.Manager, func(), error), updates *ops.UpdateService) webui.OpsStore {
	return openerStore{open: open, updates: updates}
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

func opsAuditQuery(query webui.OpsAuditQuery) ops.AuditQuery {
	return ops.AuditQuery{
		Since: query.Since, Servers: query.Servers, Projects: query.Projects,
		OwnerOnly: query.OwnerOnly, Actions: query.Actions, Statuses: query.Statuses,
		Source: query.Source, SourceIP: query.SourceIP, BeforeID: query.BeforeID,
		Limit: query.Limit,
	}
}

func (s managerStore) CheckUpdate(instance string) (webui.OpsUpdateStatus, error) {
	if s.updates == nil {
		return webui.OpsUpdateStatus{}, errUpdatesNotWired
	}
	status, err := s.updates.Check(context.Background(), instance)
	if err != nil {
		return webui.OpsUpdateStatus{}, err
	}
	return webuiUpdateStatus(status), nil
}

func (s managerStore) ApplyUpdate(instance, sourceIP string) (webui.OpsUpdateApplyResult, error) {
	if s.updates == nil {
		return webui.OpsUpdateApplyResult{}, errUpdatesNotWired
	}
	result, err := s.updates.Apply(context.Background(), ops.UpdateApplyRequest{
		Instance: instance, Source: ops.WebUISource(sourceIP),
	})
	if err != nil {
		return webui.OpsUpdateApplyResult{}, webuiUpdateError(err)
	}
	return webui.OpsUpdateApplyResult{
		OldVersion: result.OldVersion, NewVersion: result.NewVersion, Warning: result.Warning,
	}, nil
}

func (s openerStore) CheckUpdate(instance string) (webui.OpsUpdateStatus, error) {
	var status webui.OpsUpdateStatus
	err := s.withManager(func(manager *ops.Manager) error {
		got, err := managerStore{manager: manager, updates: s.updates}.CheckUpdate(instance)
		status = got
		return err
	})
	return status, err
}

func (s openerStore) ApplyUpdate(instance, sourceIP string) (webui.OpsUpdateApplyResult, error) {
	var result webui.OpsUpdateApplyResult
	err := s.withManager(func(manager *ops.Manager) error {
		got, err := managerStore{manager: manager, updates: s.updates}.ApplyUpdate(instance, sourceIP)
		result = got
		return err
	})
	return result, err
}

func webuiUpdateStatus(status ops.UpdateStatus) webui.OpsUpdateStatus {
	return webui.OpsUpdateStatus{
		CurrentVersion: status.CurrentVersion, RecommendedVersion: status.RecommendedVersion,
		LatestVersion: status.LatestVersion, LatestError: status.LatestError,
		TargetVersion: status.TargetVersion, AssetName: status.AssetName,
		CanUpgrade: status.CanUpgrade, Reason: status.Reason, Downgrade: status.Downgrade,
		NewerThanAuthority: status.NewerThanAuthority, IsClient: status.IsClient,
	}
}

// webuiUpdateError 把运维模块的升级错误换成 Web 窄接口的错误形态。
func webuiUpdateError(err error) error {
	if errors.Is(err, ops.ErrUpdateInProgress) {
		return &webui.OpsUpdateError{Step: webui.OpsUpdateStepInProgress, Message: "升级正在进行，请等待当前升级完成后重试。"}
	}
	var updateErr *ops.UpdateError
	if errors.As(err, &updateErr) {
		return &webui.OpsUpdateError{Step: webui.OpsUpdateStep(updateErr.Step), Message: updateErr.Err.Error()}
	}
	return err
}
