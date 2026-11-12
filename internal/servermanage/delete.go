package servermanage

import (
	"errors"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// ErrDeletePreviewChanged 由存储层错误包装而来，界面据此提示重新确认。
var ErrDeletePreviewChanged = store.ErrServerDeletePreviewChanged

// CredentialImpact 是删除预览里的一份凭据影响。
type CredentialImpact = store.ServerDeleteCredential

// DeliveryRecordImpact 是删除预览里的一条机密项下发记录。
type DeliveryRecordImpact = store.DeliveryRecordImpact

// DeletePreview 是与界面无关的删除影响清单加可比对摘要。设备在删除服务器
// 条目时保留。
type DeletePreview struct {
	Server          *models.Server
	DeviceName      string
	HasDevice       bool
	Anchors         []store.PinMeta
	ProfileNames    []string
	SecretNames     []string
	Credentials     []CredentialImpact
	DeliveryRecords []DeliveryRecordImpact
	Digest          string
}

// DeletePreview 按名字或标识符返回删除影响清单；找不到时返回 (nil, nil)。
func (m *Manager) DeletePreview(identifier string) (*DeletePreview, error) {
	srv, err := m.st.ResolveServer(identifier)
	if err != nil || srv == nil {
		return nil, err
	}
	preview, err := m.st.ServerDeletePreview(srv.ID)
	if err != nil || preview == nil {
		return nil, err
	}
	return deletePreview(preview), nil
}

// deletePreview 把存储层的删除预览换成模块的删除预览。
func deletePreview(preview *store.ServerDeletePreview) *DeletePreview {
	out := &DeletePreview{
		Server: preview.Server, HasDevice: preview.HasDevice,
		Anchors: preview.Anchors, ProfileNames: preview.ProfileNames,
		SecretNames: preview.SecretNames, Credentials: preview.Credentials,
		DeliveryRecords: preview.DeliveryRecords, Digest: preview.Digest(),
	}
	if preview.Device != nil {
		out.DeviceName = preview.Device.Name
	}
	return out
}

// Delete 带已确认的预览摘要删除一台服务器。执行事务里会重新生成预览比对；
// 不一致时不删除任何数据并返回新预览与 ErrDeletePreviewChanged。审计动作是
// server.delete，摘要只含条目名、主机地址与端口。
func (m *Manager) Delete(identifier, digest string, src Source) (*DeletePreview, error) {
	srv, err := m.st.ResolveServer(identifier)
	if err != nil {
		return nil, err
	}
	if srv == nil {
		return nil, nil
	}
	summary := map[string]any{"name": srv.Name, "host": srv.Host, "port": srv.Port}
	preview, err := m.st.DeleteServerCascadingConfirmed(srv.ID, digest)
	if err != nil {
		if errors.Is(err, store.ErrServerDeletePreviewChanged) {
			fresh, freshErr := m.DeletePreview(identifier)
			if freshErr != nil {
				return nil, freshErr
			}
			_ = m.writeAudit("server.delete", summary, err, src)
			return fresh, err
		}
		_ = m.writeAudit("server.delete", summary, err, src)
		return nil, err
	}
	if preview == nil {
		return nil, nil
	}
	if err := m.writeAudit("server.delete", summary, nil, src); err != nil {
		return nil, err
	}
	return deletePreview(preview), nil
}
