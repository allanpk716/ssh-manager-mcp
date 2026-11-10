package store

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"time"

	"ssh-manager-mcp/internal/models"
)

// ErrDeviceDeletePreviewChanged 表示 owner 确认的删除预览与执行时重新组装的
// 清单不一致。删除必须整体中止，由 owner 重新查看新预览后再确认。
var ErrDeviceDeletePreviewChanged = errors.New(
	"设备删除预览已变化；未删除任何数据，请重新执行命令查看新的预览",
)

// DeviceDeleteSecretAllow 是设备删除时会从某个机密项的可写入服务器列表
// 移出的服务器条目名单。
type DeviceDeleteSecretAllow struct {
	SecretName  string
	ServerNames []string
}

// DeviceDeletePreview 是 vault 层组装好的设备删除影响清单。命令行只打印
// 这份结构，不自行查询级联对象；后续共享存储也能在同一结构里追加一组。
type DeviceDeletePreview struct {
	Device             *models.Device
	Servers            []*models.DeviceServer
	SharedStorages     []*models.SharedStorage
	SecretServerAllows []*DeviceDeleteSecretAllow
	Memberships        []*models.NetworkMembership
	LayoutCoordinates  []*models.DeviceLayoutCoordinate
}

// DeviceDeletePreview 按设备名字组装删除预览。预览只读数据库，不改变
// 任何行，也不写审计。
func (s *Store) DeviceDeletePreview(name string) (*DeviceDeletePreview, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	return deviceDeletePreviewTx(tx, name)
}

// DeleteDeviceCascading 在同一个事务里删除设备的全部服务器条目、网络成
// 员与布局坐标，然后删除设备本身。confirmed 是 owner 已经确认过的预览；
// 执行前会在删除事务里重新组装预览并逐项比对，避免确认与执行之间又有
// 条目挂到这台设备时删掉未确认的内容。客户端实例只因为设备行删除而解除
// 关联。
func (s *Store) DeleteDeviceCascading(name string, confirmed *DeviceDeletePreview) (*DeviceDeletePreview, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	if confirmed == nil {
		return nil, fmt.Errorf("设备删除缺少已确认的预览；请重新执行命令查看预览")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	preview, err := deviceDeletePreviewTx(tx, name)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(confirmed, preview) {
		return nil, ErrDeviceDeletePreviewChanged
	}
	auditAt := s.nowTime()
	for _, server := range preview.Servers {
		deleted, err := s.deleteServerCascadingTx(tx, server.ID, auditAt)
		if err != nil {
			return nil, err
		}
		if deleted == nil {
			return nil, fmt.Errorf("设备删除预览与数据库状态不一致：服务器条目 %s 已不存在", server.Name)
		}
		if err := writeServerRemoveAuditTx(tx, deleted, auditAt); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(`DELETE FROM network_memberships WHERE device_id=?`, preview.Device.ID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`DELETE FROM device_layout_coordinates WHERE device_id=?`, preview.Device.ID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`DELETE FROM devices WHERE id=?`, preview.Device.ID); err != nil {
		return nil, err
	}
	if err := writeTopologyAuditTx(tx, auditAt, "device.rm", deviceDeleteAuditSummary(preview)); err != nil {
		return nil, err
	}
	return preview, tx.Commit()
}

// deviceDeletePreviewTx 在一个数据库事务里读取全部级联影响，保证预览与
// 删除主体使用同一套查询。
func deviceDeletePreviewTx(tx *sql.Tx, name string) (*DeviceDeletePreview, error) {
	device, err := deviceByNameTx(tx, name)
	if err != nil {
		return nil, err
	}
	servers, err := deviceServersForDeviceTx(tx, device.ID)
	if err != nil {
		return nil, err
	}
	sharedStorages, err := sharedStoragesForDeviceTx(tx, device.ID)
	if err != nil {
		return nil, err
	}
	secretAllows, err := deviceSecretServerAllowsTx(tx, device.ID)
	if err != nil {
		return nil, err
	}
	memberships, err := networkMembershipsForDeviceTx(tx, device.ID)
	if err != nil {
		return nil, err
	}
	coordinates, err := deviceLayoutCoordinatesForDeviceTx(tx, device.ID)
	if err != nil {
		return nil, err
	}
	return &DeviceDeletePreview{
		Device: device, Servers: servers, SharedStorages: sharedStorages,
		SecretServerAllows: secretAllows,
		Memberships:        memberships, LayoutCoordinates: coordinates,
	}, nil
}

// deviceServersForDeviceTx 返回设备关联的服务器条目摘要及锚状态，详情与
// 删除预览共用。
func deviceServersForDeviceTx(q dbtx, deviceID string) ([]*models.DeviceServer, error) {
	rows, err := q.Query(`
		SELECT server.id,server.name,server.user,server.port,
		       EXISTS(SELECT 1 FROM entry_host_keys pin WHERE pin.server_id=server.id)
		FROM servers server WHERE server.device_id=? ORDER BY server.name,server.id`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*models.DeviceServer, 0)
	for rows.Next() {
		var server models.DeviceServer
		if err := rows.Scan(&server.ID, &server.Name, &server.User, &server.Port, &server.Pinned); err != nil {
			return nil, err
		}
		out = append(out, &server)
	}
	return out, rows.Err()
}

// deviceSecretServerAllowsTx 按机密项分组列出会因设备删除而移出的服务器
// 条目名字。
func deviceSecretServerAllowsTx(tx *sql.Tx, deviceID string) ([]*DeviceDeleteSecretAllow, error) {
	rows, err := tx.Query(`
		SELECT secret.name,server.name
		FROM secret_server_allow allow
		JOIN secrets secret ON secret.id=allow.secret_id
		JOIN servers server ON server.id=allow.server_id
		WHERE server.device_id=? ORDER BY secret.name,server.name`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*DeviceDeleteSecretAllow
	bySecret := make(map[string]*DeviceDeleteSecretAllow)
	for rows.Next() {
		var secretName, serverName string
		if err := rows.Scan(&secretName, &serverName); err != nil {
			return nil, err
		}
		group := bySecret[secretName]
		if group == nil {
			group = &DeviceDeleteSecretAllow{SecretName: secretName}
			bySecret[secretName] = group
			out = append(out, group)
		}
		group.ServerNames = append(group.ServerNames, serverName)
	}
	return out, rows.Err()
}

// sharedStoragesForDeviceTx 列出设备提供的全部共享存储，并补齐每个共享的
// 可挂载服务器，供删除预览与删除前复核使用。
func sharedStoragesForDeviceTx(tx *sql.Tx, deviceID string) ([]*models.SharedStorage, error) {
	rows, err := tx.Query(
		`SELECT id FROM shared_storages WHERE device_id=? ORDER BY name,id`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]*models.SharedStorage, 0, len(ids))
	for _, id := range ids {
		share, err := sharedStorageByIDTx(tx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, share)
	}
	return out, nil
}

// writeServerRemoveAuditTx 写设备级联里的服务器条目删除审计，摘要与单独
// 执行服务器条目删除时保持一致。
func writeServerRemoveAuditTx(tx *sql.Tx, server *models.Server, at time.Time) error {
	return writeAuditTx(tx, AuditRow{
		TS: at, Action: "server.rm",
		Command: MarshalAuditSummary(map[string]any{
			"name": server.Name, "host": server.Host, "port": server.Port,
		}),
		Status: "ok",
	})
}

// deviceDeleteAuditSummary 只写设备删除审计的白名单：对象名字、被删
// 对象名字或数量，以及被解除关联的客户端实例名字。
func deviceDeleteAuditSummary(preview *DeviceDeletePreview) map[string]any {
	serverNames := make([]string, 0, len(preview.Servers))
	anchorNames := make([]string, 0)
	for _, server := range preview.Servers {
		serverNames = append(serverNames, server.Name)
		if server.Pinned {
			anchorNames = append(anchorNames, server.Name)
		}
	}
	memberSummaries := make([]map[string]any, 0, len(preview.Memberships))
	for _, membership := range preview.Memberships {
		memberSummaries = append(memberSummaries, membershipAuditSummary(membership))
	}
	sharedStorageNames := make([]string, 0, len(preview.SharedStorages))
	for _, share := range preview.SharedStorages {
		sharedStorageNames = append(sharedStorageNames, share.Name)
	}
	return map[string]any{
		"name":               preview.Device.Name,
		"client":             preview.Device.ClientName,
		"servers":            serverNames,
		"anchors":            anchorNames,
		"members":            memberSummaries,
		"layout-coordinates": len(preview.LayoutCoordinates),
		"shared-storages":    sharedStorageNames,
	}
}
