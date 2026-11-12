package store

import (
	"database/sql"
	"fmt"
	"time"

	"ssh-manager-mcp/internal/models"
)

// NetworkMembershipWrite 是存储层一次写入的网络成员。
type NetworkMembershipWrite struct {
	Network string
	Address string
}

// AddServerRelationsRequest 描述一次原子新增：服务器条目、登录与提权凭据、
// 所属设备、网络成员与 profile 授权都在同一个事务里写入。
type AddServerRelationsRequest struct {
	Server          models.Server
	LoginCredential *models.Credential
	SudoCredential  *models.Credential
	DeviceName      string
	ExistingDevice  bool
	Memberships     []NetworkMembershipWrite
	ProfileIDs      []string
	Source          WriteSource
}

// AddServerRelationsResult 是原子新增成功后的数据；ProfileNames 按请求顺
// 序返回实际授权的 profile 名字。
type AddServerRelationsResult struct {
	ServerID      string
	Device        *models.Device
	CreatedDevice bool
	ProfileNames  []string
	AnchorCopy    *ServerAnchorCopy
}

// AddOtherDeviceRequest 描述一次原子新增不受管设备：设备与它的全部网络成
// 员在同一个事务里写入。
type AddOtherDeviceRequest struct {
	Name        string
	Note        string
	Memberships []NetworkMembershipWrite
	Source      WriteSource
}

// AddServerWithRelations 保存服务器条目以及它的设备、网络成员和 profile
// 授权。任意一步失败时，本次请求产生的数据与审计行全部回滚。
func (s *Store) AddServerWithRelations(
	request AddServerRelationsRequest,
) (*AddServerRelationsResult, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	if err := validateServerText(&request.Server); err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	added, err := s.addServerWithCredentialsToDeviceTx(
		tx, &request.Server, request.LoginCredential, request.SudoCredential,
		request.DeviceName, !request.ExistingDevice, request.Source,
	)
	if err != nil {
		return nil, err
	}
	for _, membership := range request.Memberships {
		if _, err := s.addNetworkMembershipTx(
			tx, added.Device.Name, membership.Network, membership.Address, request.Source,
		); err != nil {
			return nil, err
		}
	}
	profileNames, err := grantProfilesToServerTx(tx, request.ProfileIDs, added.ServerID, request.Source, s.nowTime())
	if err != nil {
		return nil, err
	}
	result := &AddServerRelationsResult{
		ServerID: added.ServerID, Device: added.Device, CreatedDevice: added.CreatedDevice,
		ProfileNames: profileNames, AnchorCopy: added.AnchorCopy,
	}
	return result, tx.Commit()
}

// AddOtherDeviceWithMemberships 保存不受管设备与全部网络成员；任意一步失
// 败时整体回滚。
func (s *Store) AddOtherDeviceWithMemberships(request AddOtherDeviceRequest) (*models.Device, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	device, err := s.addDeviceTx(tx, request.Name, request.Note, "", request.Source)
	if err != nil {
		return nil, err
	}
	for _, membership := range request.Memberships {
		if _, err := s.addNetworkMembershipTx(
			tx, device.Name, membership.Network, membership.Address, request.Source,
		); err != nil {
			return nil, err
		}
	}
	return device, tx.Commit()
}

func grantProfilesToServerTx(
	tx *sql.Tx, profileIDs []string, serverID string, source WriteSource, auditAt time.Time,
) ([]string, error) {
	names := make([]string, 0, len(profileIDs))
	for _, profileID := range profileIDs {
		var profileName string
		err := tx.QueryRow(`SELECT name FROM profiles WHERE id=?`, profileID).Scan(&profileName)
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("profile %q not found (grant aborted, nothing changed)", profileID)
		}
		if err != nil {
			return nil, err
		}
		res, err := tx.Exec(
			`INSERT OR IGNORE INTO profile_servers (profile_id, server_id) VALUES (?,?)`,
			profileID, serverID,
		)
		if err != nil {
			return nil, err
		}
		if inserted, _ := res.RowsAffected(); inserted > 0 {
			if _, err := tx.Exec(`UPDATE profiles SET updated_at=? WHERE id=?`, now(), profileID); err != nil {
				return nil, err
			}
		}
		audit := OwnerAuditRowAt(auditAt, "profile.grant", map[string]any{
			"profile": profileID,
			"servers": fmt.Sprintf("1台:%s", serverID),
		})
		audit.Source = source.kind
		audit.SourceIP = source.address
		if err := writeAuditTx(tx, audit); err != nil {
			return nil, err
		}
		names = append(names, profileName)
	}
	return names, nil
}
