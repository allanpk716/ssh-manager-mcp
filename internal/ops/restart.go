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
	if entry.Result != "ok" {
		row.Status = "error"
	}
	return m.st.WriteAudit(row)
}

// Request 不直接重启；它把重启动作交回调用方传入的「响应结束后调度器」。
// 调度器触发动作后，服务管理器才被构造和调用，重启结果写入审计。
func (s *RestartService) Request(source Source, afterResponse func(restart func())) error {
	if afterResponse == nil {
		return errors.New("缺少响应结束后的调度器")
	}
	afterResponse(func() { s.restart(source) })
	return nil
}

func (s *RestartService) restart(source Source) {
	write := func(entry RestartAuditEntry) bool {
		if s.deps.Audit == nil {
			return true
		}
		return s.deps.Audit(source, entry) == nil
	}
	if s.deps.NewService == nil {
		_ = write(RestartAuditEntry{Action: "service.restart", Result: "error", Detail: "服务管理器未接线"})
		return
	}
	manager, err := s.deps.NewService()
	if err != nil {
		_ = write(RestartAuditEntry{Action: "service.restart", Result: "error", Detail: err.Error()})
		return
	}
	// 成功审计先落盘再触发系统服务管理器，避免旧进程被重启终止后来不
	// 及写审计；若管理器随后返回失败，再补一条失败结果。
	if !write(RestartAuditEntry{Action: "service.restart", Result: "ok"}) {
		return
	}
	if restartErr := manager.Restart(); restartErr != nil {
		_ = write(RestartAuditEntry{Action: "service.restart", Result: "error", Detail: restartErr.Error()})
	}
}
