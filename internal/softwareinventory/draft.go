package softwareinventory

import (
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// AgentSource 返回 MCP 工具来源。权威端与单机上的工具直接写 vault 时使
// 用；客户端经受审计转发写提交草稿时使用 ForwardedSource。
func AgentSource() Source { return Source{origin: store.AgentSource()} }

// ForwardedSource 返回受审计转发写来源。deviceCode 是发起这次转发写的
// 设备码编号，会随审计落库。
func ForwardedSource(deviceCode string) Source {
	return Source{origin: store.ForwardedWriteSource(deviceCode)}
}

// SubmitDeclarationDrafts 一次提交多条声明草稿，按输入顺序返回每条草稿
// 编号。可见范围不足时整批拒绝。
func (m *Manager) SubmitDeclarationDrafts(
	drafts []models.SoftwareDeclarationDraft,
	visibility Visibility,
	source Source,
) ([]string, error) {
	return m.st.AddSoftwareDeclarationDrafts(drafts, visibility, source.origin)
}

// DeclarationDrafts 列出可见范围内的声明草稿，供 owner 审阅。
func (m *Manager) DeclarationDrafts(visibility Visibility) ([]SoftwareDeclarationView, error) {
	return m.st.SoftwareDeclarationDrafts(visibility)
}

// ConfirmDeclarationDraft 确认一条草稿。edit 为空表示按草稿原样确认；非
// 空表示修改后确认。
func (m *Manager) ConfirmDeclarationDraft(
	draftID string,
	edit *DeclarationEdit,
	source Source,
) error {
	_, err := m.st.ConfirmSoftwareDeclarationDraft(draftID, edit, source.origin)
	return err
}

// RejectDeclarationDraft 驳回一条草稿；驳回后草稿消失，不进入生效声明。
func (m *Manager) RejectDeclarationDraft(draftID string, source Source) error {
	_, err := m.st.RejectSoftwareDeclarationDraft(draftID, source.origin)
	return err
}
