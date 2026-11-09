package store

import (
	"database/sql"
	"strings"

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
