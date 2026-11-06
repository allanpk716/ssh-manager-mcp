package store

import (
	"database/sql"
	"fmt"
	"time"

	"ssh-manager-mcp/internal/models"
)

// deviceLayoutSchema 存拓扑图在各个视图里的设备坐标。表不进入客户端快照，
// 也不加入快照修订号触发清单；本票只提供备份与导出的读写。
const deviceLayoutSchema = `
CREATE TABLE IF NOT EXISTS device_layout_coordinates (
  view_key TEXT NOT NULL,
  device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  x REAL NOT NULL,
  y REAL NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (view_key, device_id)
);
`

// SnapshotDeviceLayoutCoordinate 是备份与导出全量快照里的布局坐标段。
type SnapshotDeviceLayoutCoordinate struct {
	ViewKey   string  `json:"view_key"`
	DeviceID  string  `json:"device_id"`
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	CreatedAt int64   `json:"created_at"`
	UpdatedAt int64   `json:"updated_at"`
}

// snapshotDeviceLayoutCoordinates 读取全部布局坐标，按视图与设备排序。
func (s *Store) snapshotDeviceLayoutCoordinates() ([]SnapshotDeviceLayoutCoordinate, error) {
	rows, err := s.db.Query(`
		SELECT view_key,device_id,x,y,created_at,updated_at
		FROM device_layout_coordinates ORDER BY view_key,device_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]SnapshotDeviceLayoutCoordinate, 0)
	for rows.Next() {
		var row SnapshotDeviceLayoutCoordinate
		if err := rows.Scan(&row.ViewKey, &row.DeviceID, &row.X, &row.Y,
			&row.CreatedAt, &row.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// importDeviceLayoutCoordinatesTx 在导入快照的同一个事务里恢复布局坐标。
func importDeviceLayoutCoordinatesTx(tx *sql.Tx, rows []SnapshotDeviceLayoutCoordinate) error {
	for _, row := range rows {
		if _, err := tx.Exec(
			`INSERT INTO device_layout_coordinates(view_key,device_id,x,y,created_at,updated_at)
			 VALUES(?,?,?,?,?,?)`,
			row.ViewKey, row.DeviceID, row.X, row.Y, row.CreatedAt, row.UpdatedAt,
		); err != nil {
			return fmt.Errorf("insert device layout coordinate %s/%s: %w",
				row.ViewKey, row.DeviceID, err)
		}
	}
	return nil
}

// deviceLayoutCoordinatesForDeviceTx 返回一台设备在全部视图里的坐标。
func deviceLayoutCoordinatesForDeviceTx(tx *sql.Tx, deviceID string) ([]*models.DeviceLayoutCoordinate, error) {
	rows, err := tx.Query(`
		SELECT view_key,device_id,x,y,created_at,updated_at
		FROM device_layout_coordinates WHERE device_id=? ORDER BY view_key`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*models.DeviceLayoutCoordinate, 0)
	for rows.Next() {
		var coordinate models.DeviceLayoutCoordinate
		var created, updated int64
		if err := rows.Scan(&coordinate.ViewKey, &coordinate.DeviceID, &coordinate.X,
			&coordinate.Y, &created, &updated); err != nil {
			return nil, err
		}
		coordinate.CreatedAt, coordinate.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
		out = append(out, &coordinate)
	}
	return out, rows.Err()
}
