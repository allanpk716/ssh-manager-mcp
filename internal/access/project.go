package access

import (
	"fmt"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// ProjectSummary 是项目令牌列表的一行。令牌本体从不出现，只有前缀；这
// 是签发与轮换响应以外唯一可展示的令牌线索。
type ProjectSummary struct {
	ID          string
	Name        string
	TokenPrefix string
	Status      models.ProjectStatus
	ProfileID   string
	ProfileName string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ListProjects 按名字顺序列出项目令牌。includeRevoked 为假时不显示已吊
// 销的项目（与命令行默认一致）；ProfileName 在 profile 已删除时为空，
// ProfileID 仍保留原始绑定标识符。
func (m *Manager) ListProjects(includeRevoked bool) ([]ProjectSummary, error) {
	projects, err := m.st.ListProjects()
	if err != nil {
		return nil, err
	}
	profiles, err := m.st.ListProfiles()
	if err != nil {
		return nil, err
	}
	profileNames := make(map[string]string, len(profiles))
	for _, p := range profiles {
		profileNames[p.ID] = p.Name
	}
	out := make([]ProjectSummary, 0, len(projects))
	for _, p := range projects {
		if !includeRevoked && p.Status == models.ProjectRevoked {
			continue
		}
		out = append(out, ProjectSummary{
			ID:          p.ID,
			Name:        p.Name,
			TokenPrefix: p.TokenPrefix,
			Status:      p.Status,
			ProfileID:   p.ProfileID,
			ProfileName: profileNames[p.ProfileID],
			CreatedAt:   p.CreatedAt,
			UpdatedAt:   p.UpdatedAt,
		})
	}
	return out, nil
}

// ProjectTokenResult 是签发或轮换项目令牌的结果：一次性令牌只在这一次
// 调用里返回，Snippet 是已代入令牌、可直接粘贴进 `.mcp.json` 的片段。
// 调用方必须在同一次响应里展示，离开页面后不能再查看。
type ProjectTokenResult struct {
	ID      string
	Name    string
	Token   string
	Snippet string
}

// ProjectTokenSnippet 返回已代入令牌的 `.mcp.json` 片段，格式是单机或权
// 威端本机 agent 使用的 stdio 形态。命令行与 Web UI 共用这个函数，保证
// 两个入口给 owner 抄的是同一段配置。
func ProjectTokenSnippet(token string) string {
	return fmt.Sprintf(`{"mcpServers":{"ssh":{"command":"sshmgr","args":["mcp"],"env":{"SSHMGR_TOKEN":"%s"}}}}`, token)
}

// resolveProject 按项目名字或标识符解析（名字在存储里唯一，Web UI 的表
// 单与链接用标识符）。找不到时返回与既有命令一致的错误。
func (m *Manager) resolveProject(identifier string) (id, name string, err error) {
	projects, err := m.st.ListProjects()
	if err != nil {
		return "", "", err
	}
	for _, p := range projects {
		if p.Name == identifier || p.ID == identifier {
			return p.ID, p.Name, nil
		}
	}
	return "", "", fmt.Errorf("project %q not found", identifier)
}

// AddProject 签发一个新的项目令牌并写 `project.add` 审计行。profile 与
// 名称重复等失败也写失败审计行；审计摘要只有项目名字（成功行加项目标
// 识符），绝不含令牌。
func (m *Manager) AddProject(name, profile string, src Source) (ProjectTokenResult, error) {
	var result ProjectTokenResult
	var projectID string
	summary := map[string]any{"name": name}
	err := func() error {
		p, err := m.resolveProfile(profile)
		if err != nil {
			return err
		}
		id, token, err := m.st.AddProject(name, p.ID)
		if err != nil {
			return err
		}
		result = ProjectTokenResult{ID: id, Name: name, Token: token, Snippet: ProjectTokenSnippet(token)}
		summary["id"] = id
		projectID = id
		return nil
	}()
	if aerr := m.writeProjectAudit("project.add", summary, projectID, err, src); aerr != nil && err == nil {
		err = aerr
	}
	return result, err
}

// RotateProject 原地轮换项目令牌并写 `project.rotate` 审计行：旧令牌立
// 刻失效，新令牌只在这一个返回值里出现。轮换已吊销项目或不存在的项目
// 被拒绝并写失败审计行；摘要只有名字和标识符，绝不含令牌。
func (m *Manager) RotateProject(project string, src Source) (ProjectTokenResult, error) {
	var result ProjectTokenResult
	var projectID string
	summary := map[string]any{"name": project}
	err := func() error {
		id, name, err := m.resolveProject(project)
		if err != nil {
			return err
		}
		summary["id"] = id
		summary["name"] = name
		projectID = id
		token, err := m.st.RotateProject(id)
		if err != nil {
			return err
		}
		result = ProjectTokenResult{ID: id, Name: name, Token: token, Snippet: ProjectTokenSnippet(token)}
		return nil
	}()
	if aerr := m.writeProjectAudit("project.rotate", summary, projectID, err, src); aerr != nil && err == nil {
		err = aerr
	}
	return result, err
}

// setProjectStatus 执行一次生命周期状态变化并写审计行。项目不存在、已
// 吊销后试图复活等拒绝都写失败审计行；摘要只有名字和标识符。
func (m *Manager) setProjectStatus(project string, status models.ProjectStatus, action string, src Source) error {
	var projectID string
	summary := map[string]any{"name": project}
	err := func() error {
		id, name, err := m.resolveProject(project)
		if err != nil {
			return err
		}
		summary["id"] = id
		summary["name"] = name
		projectID = id
		return m.st.SetProjectStatus(id, status)
	}()
	if aerr := m.writeProjectAudit(action, summary, projectID, err, src); aerr != nil && err == nil {
		err = aerr
	}
	return err
}

// DisableProject 停用项目令牌（可逆：下一次启用恢复），写 `project.disable`
// 审计行。已吊销的项目不能停用。
func (m *Manager) DisableProject(project string, src Source) error {
	return m.setProjectStatus(project, models.ProjectDisabled, "project.disable", src)
}

// EnableProject 重新启用已停用的项目令牌，写 `project.enable` 审计行。
// 已吊销的项目不能启用。
func (m *Manager) EnableProject(project string, src Source) error {
	return m.setProjectStatus(project, models.ProjectActive, "project.enable", src)
}

// RevokeProject 永久吊销项目令牌（不可逆），写 `project.revoke` 审计
// 行。重复吊销保持幂等，与存储层语义一致。
func (m *Manager) RevokeProject(project string, src Source) error {
	return m.setProjectStatus(project, models.ProjectRevoked, "project.revoke", src)
}

// DeleteProject 删除已吊销的项目行，写 `project.delete` 审计行；未吊销
// 的项目必须先吊销再删除。删除后审计行仍按项目标识符保留历史。
func (m *Manager) DeleteProject(project string, src Source) error {
	var projectID string
	summary := map[string]any{"name": project}
	err := func() error {
		id, name, err := m.resolveProject(project)
		if err != nil {
			return err
		}
		summary["id"] = id
		summary["name"] = name
		projectID = id
		return m.st.DeleteProject(id)
	}()
	if aerr := m.writeProjectAudit("project.delete", summary, projectID, err, src); aerr != nil && err == nil {
		err = aerr
	}
	return err
}

// writeProjectAudit 记录项目令牌生命周期动作的审计行：与既有命令行一
// 样挂上项目标识符（删除后审计仍能按标识符追溯），成功写 Status=ok、
// 失败写 Status=error；摘要由调用方按白名单构造，绝不含令牌。
func (m *Manager) writeProjectAudit(action string, summary map[string]any, projectID string, opErr error, src Source) error {
	row := store.OwnerAuditRow(action, summary)
	row.ProjectID = projectID
	row.Source = src.kind
	row.SourceIP = src.addr
	if opErr != nil {
		row.Status = "error"
	}
	return m.st.WriteAudit(row)
}
