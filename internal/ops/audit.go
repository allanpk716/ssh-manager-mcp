package ops

import (
	"time"

	"ssh-manager-mcp/internal/store"
)

// AuditQuery 是权威 vault 审计表的查询条件。Servers 与 Projects 既接受名
// 字，也接受历史行里保留的标识符；名字解析不到时按原串过滤，已删实体仍可
// 查到。
type AuditQuery struct {
	Since     int64
	Servers   []string
	Projects  []string
	OwnerOnly bool
	Actions   []string
	Statuses  []string
	Source    string
	SourceIP  string
	BeforeID  int64
	Limit     int
}

// AuditRow 是运维模块返回给命令行与 Web UI 的审计行，带数据库编号和项目、
// 服务器的显示名。
type AuditRow struct {
	ID          int64
	TS          time.Time
	ProjectID   string
	ProjectName string
	ServerID    string
	ServerName  string
	Action      string
	Command     string
	Sudo        bool
	Status      string
	ExitCode    int
	DurationMS  int64
	Source      string
	SourceIP    string
	// InjectedEnvironmentNames 是命令执行审计行附带的环境变量名，不含值。
	InjectedEnvironmentNames []string
	// InjectedShareNames 是命令执行审计行附带的网络盘符共享名，不含地
	// 址、挂载账号或密码。
	InjectedShareNames []string
}

// QueryAudit 查询权威 vault 审计表，按编号从新到旧返回。
func (m *Manager) QueryAudit(query AuditQuery) ([]AuditRow, error) {
	rows, err := m.st.QueryAudit(store.AuditFilter{
		Since:      query.Since,
		ServerIDs:  m.resolveServerIDs(query.Servers),
		ProjectIDs: m.resolveProjectIDs(query.Projects),
		OwnerOnly:  query.OwnerOnly,
		Actions:    query.Actions,
		Statuses:   query.Statuses,
		Source:     query.Source,
		SourceIP:   query.SourceIP,
		BeforeID:   query.BeforeID,
		Limit:      query.Limit,
	})
	if err != nil {
		return nil, err
	}
	projectNames, serverNames := m.auditNameMaps()
	out := make([]AuditRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, AuditRow{
			ID: row.ID, TS: row.TS,
			ProjectID: row.ProjectID, ProjectName: projectNames[row.ProjectID],
			ServerID: row.ServerID, ServerName: serverNames[row.ServerID],
			Action: row.Action, Command: row.Command, Sudo: row.Sudo,
			Status: row.Status, ExitCode: row.ExitCode, DurationMS: row.DurationMS,
			Source: row.Source, SourceIP: row.SourceIP,
			InjectedEnvironmentNames: row.InjectedEnvironmentNames,
			InjectedShareNames:       row.InjectedShareNames,
		})
	}
	return out, nil
}

// JSONMap 返回命令行 JSON 输出使用的审计行字段，字段含义与存储层一致。
func (r AuditRow) JSONMap() map[string]any {
	record := map[string]any{
		"id": r.ID, "ts": r.TS.Unix(), "project_id": r.ProjectID,
		"server_id": r.ServerID, "action": r.Action, "command": r.Command,
		"sudo": r.Sudo, "status": r.Status, "exit_code": r.ExitCode,
		"duration_ms": r.DurationMS, "source": r.Source, "source_ip": r.SourceIP,
	}
	if len(r.InjectedEnvironmentNames) > 0 {
		record["injected_environment_names"] = r.InjectedEnvironmentNames
	}
	if len(r.InjectedShareNames) > 0 {
		record["injected_share_names"] = r.InjectedShareNames
	}
	return record
}

// resolveServerIDs 把服务器名字换成标识符；解析失败时保留原串，供已删实体
// 的历史审计行继续命中。
func (m *Manager) resolveServerIDs(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if server, _ := m.st.GetServerByName(value); server != nil {
			value = server.ID
		}
		out = append(out, value)
	}
	return out
}

// resolveProjectIDs 把项目名字换成标识符；解析失败时保留原串。
func (m *Manager) resolveProjectIDs(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if project, _ := m.st.GetProjectByName(value); project != nil {
			value = project.ID
		}
		out = append(out, value)
	}
	return out
}

// auditNameMaps 生成标识符到显示名的映射；读取失败时返回空映射，查询本身
// 仍能返回原始标识符。
func (m *Manager) auditNameMaps() (projects, servers map[string]string) {
	projects, servers = map[string]string{}, map[string]string{}
	if list, err := m.st.ListProjects(); err == nil {
		for _, project := range list {
			projects[project.ID] = project.Name
		}
	}
	if list, err := m.st.ListServers(); err == nil {
		for _, server := range list {
			servers[server.ID] = server.Name
		}
	}
	return projects, servers
}
