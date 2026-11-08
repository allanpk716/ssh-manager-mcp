package ops

import (
	"errors"

	"ssh-manager-mcp/internal/store"
)

// ServiceRestarter 是系统服务管理器的最小操作面；生产实现来自现有的
// kardianos/service 抽象，测试用桩代替，不触碰本机真实服务。
type ServiceRestarter interface {
	Restart() error
}

// RestartAuditEntry 是重启服务的审计摘要；只记录结果与失败说明，不含令牌。
type RestartAuditEntry struct {
	Action string
	Result string
	Detail string
}

// RestartAuditWriteError 表示「已发起」审计没有写入。调用方必须把它当作
// 未重启处理，不能再触发服务管理器。
type RestartAuditWriteError struct {
	Err error
}

func (e *RestartAuditWriteError) Error() string {
	return "写入 service.restart 审计失败：" + e.Err.Error()
}

func (e *RestartAuditWriteError) Unwrap() error { return e.Err }

// RestartDependencies 是重启服务的可注入依赖。
type RestartDependencies struct {
	NewService func() (ServiceRestarter, error)
	Audit      func(source Source, entry RestartAuditEntry) error
}

// RestartService 负责在调用方确认响应完成后发起系统服务重启，并统一写
// service.restart 审计。
type RestartService struct {
	deps RestartDependencies
}

// NewRestartService 构造重启服务。
func NewRestartService(deps RestartDependencies) *RestartService {
	return &RestartService{deps: deps}
}

// WriteRestartAudit 把服务重启结果写入权威 vault 审计表。
func (m *Manager) WriteRestartAudit(source Source, entry RestartAuditEntry) error {
	summary := map[string]any{"result": entry.Result}
	if entry.Detail != "" {
		summary["detail"] = entry.Detail
	}
	row := store.OwnerAuditRow(entry.Action, summary)
	row.Source, row.SourceIP = source.AuditFields()
	if entry.Result == "error" || entry.Result == "failed" {
		row.Status = "error"
	}
	return m.st.WriteAudit(row)
}

// Request 先同步写入「已发起」审计；写入失败时不登记任何动作，调用方
// 必须返回错误页。写入成功后，它把真正重启交回调用方传入的「响应结束
// 后调度器」，保证完整响应先到浏览器。
func (s *RestartService) Request(source Source, afterResponse func(restart func())) error {
	if afterResponse == nil {
		return errors.New("缺少响应结束后的调度器")
	}
	if s.deps.Audit != nil {
		err := s.deps.Audit(source, RestartAuditEntry{
			Action: "service.restart", Result: "requested",
		})
		if err != nil {
			return &RestartAuditWriteError{Err: err}
		}
	}
	afterResponse(func() { s.restart(source) })
	return nil
}

func (s *RestartService) restart(source Source) {
	writeFailure := func(entry RestartAuditEntry) {
		if s.deps.Audit == nil {
			return
		}
		_ = s.deps.Audit(source, entry)
	}
	if s.deps.NewService == nil {
		writeFailure(RestartAuditEntry{Action: "service.restart", Result: "failed", Detail: "服务管理器未接线"})
		return
	}
	manager, err := s.deps.NewService()
	if err != nil {
		writeFailure(RestartAuditEntry{Action: "service.restart", Result: "failed", Detail: err.Error()})
		return
	}
	if restartErr := manager.Restart(); restartErr != nil {
		writeFailure(RestartAuditEntry{Action: "service.restart", Result: "failed", Detail: restartErr.Error()})
	}
}
