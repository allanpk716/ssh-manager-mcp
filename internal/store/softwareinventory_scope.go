package store

import (
	"database/sql"
	"strings"
	"time"

	"ssh-manager-mcp/internal/models"
)

// SoftwareDeviceOverview 是统一查询与待办计算需要的设备形态。Managed 表示
// 这台设备是否至少挂着一个服务器条目；不受管设备为 false。
type SoftwareDeviceOverview struct {
	ID      string
	Name    string
	Managed bool
}

// SoftwareObservedInventory 是一台设备的实测条目集合。
type SoftwareObservedInventory struct {
	DeviceID   string
	DeviceName string
	Items      []models.SoftwareObservedItem
}

// SoftwareDeviceOverviewForDevice 返回一台设备并套用可见范围。设备不存在
// 与范围不足同样返回 ErrSoftwareDeviceNotVisible，避免向查询方泄露设备名。
func (s *Store) SoftwareDeviceOverviewForDevice(
	deviceName string,
	scope SoftwareDeviceScope,
) (*SoftwareDeviceOverview, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	device, err := deviceByNameTx(tx, strings.TrimSpace(deviceName))
	if err != nil {
		return nil, ErrSoftwareDeviceNotVisible
	}
	if err := ensureSoftwareDeviceVisibleTx(tx, device.ID, scope); err != nil {
		return nil, err
	}
	overview, err := softwareDeviceOverviewTx(tx, device.ID)
	if err != nil {
		return nil, err
	}
	return overview, tx.Commit()
}

// SoftwareDeviceOverviews 返回可见设备；owner 范围包含不受管设备，profile
// 范围只包含至少有一个被授权服务器条目的受管设备。
func (s *Store) SoftwareDeviceOverviews(
	scope SoftwareDeviceScope,
) ([]SoftwareDeviceOverview, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var rows *sql.Rows
	if scope.all {
		rows, err = tx.Query(`
			SELECT d.id,d.name,EXISTS(SELECT 1 FROM servers server WHERE server.device_id=d.id)
			FROM devices d ORDER BY d.name,d.id`)
	} else {
		ids := idsFromSet(scope.serverIDs)
		if len(ids) == 0 {
			return []SoftwareDeviceOverview{}, tx.Commit()
		}
		placeholders := placeholdersForIDs(scope.serverIDs)
		rows, err = tx.Query(`
			SELECT DISTINCT d.id,d.name,1
			FROM devices d JOIN servers server ON server.device_id=d.id
			WHERE server.id IN (`+placeholders+`)
			ORDER BY d.name,d.id`, ids...)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SoftwareDeviceOverview{}
	for rows.Next() {
		var row SoftwareDeviceOverview
		if err := rows.Scan(&row.ID, &row.Name, &row.Managed); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// SoftwareObservedItemsForDeviceInScope 读回一台设备的实测条目，并先校验
// 可见设备范围。
func (s *Store) SoftwareObservedItemsForDeviceInScope(
	deviceName string,
	scope SoftwareDeviceScope,
) (*SoftwareObservedInventory, error) {
	overview, err := s.SoftwareDeviceOverviewForDevice(deviceName, scope)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	items, err := softwareObservedItemsForDeviceTx(tx, overview.ID)
	if err != nil {
		return nil, err
	}
	return &SoftwareObservedInventory{
		DeviceID: overview.ID, DeviceName: overview.Name, Items: items,
	}, tx.Commit()
}

// SoftwareObservedItemsForScope 读回全部可见设备的实测条目。只有实测、没
// 有任何声明的设备也会出现在结果里。
func (s *Store) SoftwareObservedItemsForScope(
	scope SoftwareDeviceScope,
) (map[string]SoftwareObservedInventory, error) {
	overviews, err := s.SoftwareDeviceOverviews(scope)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out := make(map[string]SoftwareObservedInventory, len(overviews))
	for _, overview := range overviews {
		items, err := softwareObservedItemsForDeviceTx(tx, overview.ID)
		if err != nil {
			return nil, err
		}
		out[overview.ID] = SoftwareObservedInventory{
			DeviceID: overview.ID, DeviceName: overview.Name, Items: items,
		}
	}
	return out, tx.Commit()
}

func softwareDeviceOverviewTx(tx *sql.Tx, deviceID string) (*SoftwareDeviceOverview, error) {
	var overview SoftwareDeviceOverview
	if err := tx.QueryRow(`
		SELECT d.id,d.name,EXISTS(SELECT 1 FROM servers server WHERE server.device_id=d.id)
		FROM devices d WHERE d.id=?`, deviceID,
	).Scan(&overview.ID, &overview.Name, &overview.Managed); err != nil {
		return nil, err
	}
	return &overview, nil
}

// softwareObservedItemsForDeviceTx 在一个已有事务里读回实测条目与监听端口。
func softwareObservedItemsForDeviceTx(
	tx *sql.Tx,
	deviceID string,
) ([]models.SoftwareObservedItem, error) {
	rows, err := tx.Query(`
		SELECT id,category,name,version,package_url,belongs_to,architecture,container_tag,
		       container_digest,container_running,executable_path,process_name,process_id,stable_key,collected_at
		FROM software_observed_items WHERE device_id=?
		ORDER BY CASE category
			WHEN 'operating_system' THEN 1
			WHEN 'service_program' THEN 2
			WHEN 'container_image' THEN 3
			WHEN 'driver_firmware' THEN 4
			WHEN 'system_package' THEN 5
			WHEN 'unidentified_process' THEN 6 END,
			name,version,stable_key`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []models.SoftwareObservedItem{}
	ids := []string{}
	for rows.Next() {
		var item models.SoftwareObservedItem
		var id string
		var collectedAt int64
		if err := rows.Scan(
			&id, &item.Category, &item.Name, &item.Version, &item.PackageURL,
			&item.BelongsTo, &item.Architecture, &item.ContainerTag,
			&item.ContainerDigest, &item.ContainerRunning, &item.ExecutablePath,
			&item.ProcessName, &item.ProcessID, &item.StableKey, &collectedAt,
		); err != nil {
			return nil, err
		}
		item.CollectedAt = time.Unix(collectedAt, 0).UTC()
		items = append(items, item)
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range items {
		portRows, err := tx.Query(`
			SELECT address,port,protocol,exposure FROM software_observed_ports
			WHERE item_id=? ORDER BY port,protocol,address`, ids[i])
		if err != nil {
			return nil, err
		}
		for portRows.Next() {
			var port models.SoftwareListeningPort
			if err := portRows.Scan(&port.Address, &port.Port, &port.Protocol, &port.Exposure); err != nil {
				portRows.Close()
				return nil, err
			}
			items[i].ListeningPorts = append(items[i].ListeningPorts, port)
		}
		if err := portRows.Err(); err != nil {
			portRows.Close()
			return nil, err
		}
		if err := portRows.Close(); err != nil {
			return nil, err
		}
	}
	return items, nil
}
