package servermanage

import (
	"ssh-manager-mcp/internal/models"
)

// Detail 是一台服务器的详情：完整条目（含连接信息、服务器元数据、标签与
// 主机可见性）加不落凭据内容的凭据摘要、所属设备名与注意原因。
type Detail struct {
	Server           *models.Server
	AttentionReasons []string
	// CredentialType 是登录凭据类型；空串表示未挂凭据。
	CredentialType models.CredentialType
	// HasKeyPassphrase 表示登录私钥凭据已存私钥口令。
	HasKeyPassphrase bool
	// HasSudoPassword 表示已设 sudo 密码。
	HasSudoPassword bool
	// DeviceName / HasDevice 描述所属设备；设备在删除服务器条目时保留。
	DeviceName string
	HasDevice  bool
}

// Detail 按名字或标识符返回一台服务器的详情；找不到时返回 (nil, nil)。
func (m *Manager) Detail(identifier string) (*Detail, error) {
	srv, err := m.st.ResolveServer(identifier)
	if err != nil || srv == nil {
		return nil, err
	}
	detail := &Detail{
		Server:           srv,
		AttentionReasons: models.ServerAttentionReasons(srv.CredentialID, srv.Role, srv.Tags),
		HasSudoPassword:  srv.SudoCredentialID != "",
	}
	if srv.CredentialID != "" {
		kind, hasPass, found, err := m.st.CredentialSummary(srv.CredentialID)
		if err != nil {
			return nil, err
		}
		if found {
			detail.CredentialType = kind
			detail.HasKeyPassphrase = hasPass
		}
	}
	if srv.DeviceID != "" {
		device, err := m.st.GetDeviceByID(srv.DeviceID)
		if err != nil {
			return nil, err
		}
		if device != nil {
			detail.DeviceName = device.Name
			detail.HasDevice = true
		}
	}
	return detail, nil
}
