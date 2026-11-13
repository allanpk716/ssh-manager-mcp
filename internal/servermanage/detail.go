package servermanage

import (
	"sort"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
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
	// Secrets 是可写入服务器列表包含这台服务器条目、且没有被共享存储
	// 用作挂载密码的机密项。
	Secrets []DetailSecret
	// SharedStorages 是密码机密项可写入这台服务器条目的共享存储；同一个
	// 密码机密项驱动多个共享时逐项出现。
	SharedStorages []DetailSharedStorage
	// Profiles 是授予这台服务器条目的访问配置组。
	Profiles []DetailProfile
	// DeliveryRecords 是写到这台服务器条目的最新下发记录，只读展示。
	DeliveryRecords []store.SecretDeliveryRecord
	// SiblingSharedStorages 是同一台设备的其他服务器条目已授权、因此
	// 在这台机器上也可见的共享存储；每项注明来源服务器条目。
	SiblingSharedStorages []DetailSiblingSharedStorage
	// SiblingDeliveryRecords 是同一台设备的其他服务器条目写出的文件，仅
	// 作为整台机器上的可见性提示。
	SiblingDeliveryRecords []store.SecretDeliveryRecord
}

// DetailSecret 是服务器详情页机密项组的一行，只携带名字、备注与环境变量
// 名，不携带值或值的版本信息。
type DetailSecret struct {
	ID              string
	Name            string
	Note            string
	EnvironmentName string
}

// DetailSharedStorage 是服务器详情页共享存储组的一行，字段沿用共享存储
// 列表与详情页的识别信息，并给出这台服务器条目的挂载点。
type DetailSharedStorage struct {
	ID                 string
	Name               string
	ProviderDeviceName string
	MountUser          string
	PasswordSecretID   string
	PasswordSecretName string
	Drive              string
	MountPoint         string
}

// DetailProfile 是服务器详情页 profile 授权组的一行。
type DetailProfile struct {
	ID   string
	Name string
}

// DetailSiblingSharedStorage 是同设备可见性提示区的一行：兄弟服务器条目
// 授权后，这台机器也能看到的共享存储。
type DetailSiblingSharedStorage struct {
	DetailSharedStorage
	SourceServerID   string
	SourceServerName string
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
	if err := m.fillReverseView(detail); err != nil {
		return nil, err
	}
	return detail, nil
}

// fillReverseView 从现有机密项授权、共享存储登记、profile 授权与下发记录
// 装配反向视图。读取路径只调用存储层的查询入口，不写任何审计行。
func (m *Manager) fillReverseView(detail *Detail) error {
	srv := detail.Server
	shares, err := m.st.ListSharedStorages(store.SharedStorageFilter{})
	if err != nil {
		return err
	}
	shareSecretIDs := make(map[string]bool, len(shares))
	for _, share := range shares {
		shareSecretIDs[share.PasswordSecretID] = true
	}

	secrets, err := m.st.ListSecretsInScope(store.ScopeToServers(srv.ID))
	if err != nil {
		return err
	}
	for _, secret := range secrets {
		if shareSecretIDs[secret.ID] {
			continue
		}
		detail.Secrets = append(detail.Secrets, DetailSecret{
			ID: secret.ID, Name: secret.Name, Note: secret.Note,
			EnvironmentName: secret.EnvironmentName,
		})
	}

	profiles, err := m.st.ListProfiles()
	if err != nil {
		return err
	}
	for _, profile := range profiles {
		ids, err := m.st.ServersForProfile(profile.ID)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if id == srv.ID {
				detail.Profiles = append(detail.Profiles, DetailProfile{ID: profile.ID, Name: profile.Name})
				break
			}
		}
	}

	records, err := m.st.SecretDeliveryRecords(store.SecretDeliveryRecordFilter{ServerID: srv.ID})
	if err != nil {
		return err
	}
	detail.DeliveryRecords = records

	allRecords, err := m.st.SecretDeliveryRecords(store.SecretDeliveryRecordFilter{})
	if err != nil {
		return err
	}
	if srv.DeviceID != "" {
		servers, err := m.st.ListServers()
		if err != nil {
			return err
		}
		serverDevices := make(map[string]string, len(servers))
		for _, server := range servers {
			serverDevices[server.ID] = server.DeviceID
		}
		for _, record := range allRecords {
			if record.ServerID != srv.ID && serverDevices[record.ServerID] == srv.DeviceID {
				detail.SiblingDeliveryRecords = append(detail.SiblingDeliveryRecords, record)
			}
		}
	}

	for _, share := range shares {
		for _, mountable := range share.MountableServers {
			if mountable.ID != srv.ID {
				continue
			}
			detail.SharedStorages = append(detail.SharedStorages, detailSharedStorage(share, mountable.MountPoint))
			break
		}
		if srv.DeviceID == "" {
			continue
		}
		for _, mountable := range share.MountableServers {
			if mountable.ID == srv.ID || mountable.DeviceName != detail.DeviceName {
				continue
			}
			detail.SiblingSharedStorages = append(detail.SiblingSharedStorages, DetailSiblingSharedStorage{
				DetailSharedStorage: detailSharedStorage(share, mountable.MountPoint),
				SourceServerID:      mountable.ID,
				SourceServerName:    mountable.Name,
			})
		}
	}
	sort.Slice(detail.SiblingSharedStorages, func(i, j int) bool {
		left, right := detail.SiblingSharedStorages[i], detail.SiblingSharedStorages[j]
		if left.SourceServerName != right.SourceServerName {
			return left.SourceServerName < right.SourceServerName
		}
		return left.Name < right.Name
	})
	return nil
}

func detailSharedStorage(share *models.SharedStorage, mountPoint string) DetailSharedStorage {
	return DetailSharedStorage{
		ID: share.ID, Name: share.Name, ProviderDeviceName: share.DeviceName,
		MountUser: share.MountUser, PasswordSecretID: share.PasswordSecretID,
		PasswordSecretName: share.PasswordSecretName,
		Drive:              share.Drive, MountPoint: mountPoint,
	}
}
