package store

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
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

// containerLayoutSchema 存拓扑图在各个视图里的站点与网络坐标。站点与网络
// 的删除路径显式清理本表；表同样不进入快照修订号触发清单。
const containerLayoutSchema = `
CREATE TABLE IF NOT EXISTS container_layout_coordinates (
  view_key TEXT NOT NULL,
  entity_type TEXT NOT NULL,
  entity_id TEXT NOT NULL,
  x REAL NOT NULL,
  y REAL NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (view_key, entity_type, entity_id)
);
`

// 布局视图的固定键：拓扑图页面的物理视图、虚拟视图与叠加视图。
const (
	LayoutViewPhysical = "physical"
	LayoutViewVirtual  = "virtual"
	LayoutViewOverlay  = "overlay"
)

// 容器布局实体的固定类型：站点区域与网络分组框。
const (
	LayoutEntitySite    = "site"
	LayoutEntityNetwork = "network"
)

// 布局保存与重置的稳定错误；报错文本直接面向 owner。
var (
	ErrLayoutViewInvalid       = errors.New("布局视图只能是物理视图、虚拟视图或叠加视图")
	ErrLayoutEntityInvalid     = errors.New("布局实体类型只能是站点或网络")
	ErrLayoutEntityDuplicate   = errors.New("同一次布局保存里出现了重复的站点、网络或设备")
	ErrLayoutCoordinateInvalid = errors.New("布局坐标必须是有限的横坐标与纵坐标")
	ErrLayoutEntityMissing     = errors.New("布局引用的站点、网络或设备不存在")
	ErrLayoutIdentifierInvalid = errors.New("布局条目缺少站点、网络或设备的标识")
)

// DeviceLayoutInput 是一次保存里一台设备的坐标。
type DeviceLayoutInput struct {
	DeviceID string
	X        float64
	Y        float64
}

// ContainerLayoutInput 是一次保存里一个站点区域或网络分组框的坐标。
type ContainerLayoutInput struct {
	EntityType string
	EntityID   string
	X          float64
	Y          float64
}

// ViewLayoutInput 是一个视图的完整布局：保存时整体替换该视图此前保存的
// 全部坐标。
type ViewLayoutInput struct {
	Devices    []DeviceLayoutInput
	Containers []ContainerLayoutInput
}

// SnapshotDeviceLayoutCoordinate 是备份与导出全量快照里的布局坐标段。
type SnapshotDeviceLayoutCoordinate struct {
	ViewKey   string  `json:"view_key"`
	DeviceID  string  `json:"device_id"`
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	CreatedAt int64   `json:"created_at"`
	UpdatedAt int64   `json:"updated_at"`
}

// SnapshotContainerLayoutCoordinate 是备份与导出快照里的站点与网络坐标段。
// 旧快照没有该段时解码为零值切片，导入零行。
type SnapshotContainerLayoutCoordinate struct {
	ViewKey    string  `json:"view_key"`
	EntityType string  `json:"entity_type"`
	EntityID   string  `json:"entity_id"`
	X          float64 `json:"x"`
	Y          float64 `json:"y"`
	CreatedAt  int64   `json:"created_at"`
	UpdatedAt  int64   `json:"updated_at"`
}

// validLayoutView 判断视图键是否是拓扑图页面的三种视图之一。
func validLayoutView(view string) bool {
	return view == LayoutViewPhysical || view == LayoutViewVirtual || view == LayoutViewOverlay
}

// validateViewLayout 先在数据库外完成输入校验：视图键、实体类型、标识符、
// 重复条目与有限坐标。引用是否存在在事务里查询。
func validateViewLayout(view string, input ViewLayoutInput) error {
	if !validLayoutView(view) {
		return ErrLayoutViewInvalid
	}
	seenDevices := make(map[string]bool, len(input.Devices))
	for _, coordinate := range input.Devices {
		identifier := strings.TrimSpace(coordinate.DeviceID)
		if identifier == "" {
			return ErrLayoutIdentifierInvalid
		}
		if seenDevices[identifier] {
			return fmt.Errorf("%w：%s", ErrLayoutEntityDuplicate, identifier)
		}
		seenDevices[identifier] = true
		if !finiteCoordinate(coordinate.X, coordinate.Y) {
			return ErrLayoutCoordinateInvalid
		}
	}
	seenContainers := make(map[string]bool, len(input.Containers))
	for _, coordinate := range input.Containers {
		if coordinate.EntityType != LayoutEntitySite && coordinate.EntityType != LayoutEntityNetwork {
			return ErrLayoutEntityInvalid
		}
		identifier := strings.TrimSpace(coordinate.EntityID)
		if identifier == "" {
			return ErrLayoutIdentifierInvalid
		}
		key := coordinate.EntityType + "/" + identifier
		if seenContainers[key] {
			return fmt.Errorf("%w：%s", ErrLayoutEntityDuplicate, key)
		}
		seenContainers[key] = true
		if !finiteCoordinate(coordinate.X, coordinate.Y) {
			return ErrLayoutCoordinateInvalid
		}
	}
	return nil
}

func finiteCoordinate(x, y float64) bool {
	return !math.IsNaN(x) && !math.IsNaN(y) && !math.IsInf(x, 0) && !math.IsInf(y, 0)
}

// SaveViewLayout 保存一个视图的完整布局，整体替换该视图此前保存的设备、
// 站点与网络坐标。写入与一条来源为 Web UI 的审计行在同一个事务里提交，
// 不推动快照修订号，也不产生变更提示。
func (s *Store) SaveViewLayout(view string, input ViewLayoutInput, sourceIP string) error {
	if s.readOnly {
		return ErrReadOnly
	}
	if err := validateViewLayout(view, input); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	deviceIDs := make([]string, 0, len(input.Devices))
	for _, coordinate := range input.Devices {
		deviceIDs = append(deviceIDs, coordinate.DeviceID)
	}
	siteIDs, networkIDs := splitContainerInput(input.Containers)
	if err := ensureLayoutEntitiesExistTx(tx, deviceIDs, siteIDs, networkIDs); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM device_layout_coordinates WHERE view_key=?`, view); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM container_layout_coordinates WHERE view_key=?`, view); err != nil {
		return err
	}
	at := s.nowTime().Unix()
	for _, coordinate := range input.Devices {
		if _, err := tx.Exec(
			`INSERT INTO device_layout_coordinates(view_key,device_id,x,y,created_at,updated_at)
			 VALUES(?,?,?,?,?,?)`,
			view, coordinate.DeviceID, coordinate.X, coordinate.Y, at, at,
		); err != nil {
			return fmt.Errorf("insert device layout coordinate %s/%s: %w", view, coordinate.DeviceID, err)
		}
	}
	for _, coordinate := range input.Containers {
		if _, err := tx.Exec(
			`INSERT INTO container_layout_coordinates(view_key,entity_type,entity_id,x,y,created_at,updated_at)
			 VALUES(?,?,?,?,?,?,?)`,
			view, coordinate.EntityType, coordinate.EntityID, coordinate.X, coordinate.Y, at, at,
		); err != nil {
			return fmt.Errorf("insert container layout coordinate %s/%s/%s: %w",
				view, coordinate.EntityType, coordinate.EntityID, err)
		}
	}
	summary := map[string]any{
		"view":    view,
		"devices": len(input.Devices),
	}
	siteCount, networkCount := 0, 0
	for _, coordinate := range input.Containers {
		if coordinate.EntityType == LayoutEntitySite {
			siteCount++
		} else {
			networkCount++
		}
	}
	summary["sites"] = siteCount
	summary["networks"] = networkCount
	if err := writeLayoutAuditTx(tx, s.nowTime(), "topology.layout.save", summary, sourceIP); err != nil {
		return err
	}
	return tx.Commit()
}

// ResetViewLayout 清空一个视图保存的全部坐标，让拓扑图回到自动排布。清空
// 与一条来源为 Web UI 的审计行在同一个事务里提交。
func (s *Store) ResetViewLayout(view string, sourceIP string) error {
	if s.readOnly {
		return ErrReadOnly
	}
	if !validLayoutView(view) {
		return ErrLayoutViewInvalid
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM device_layout_coordinates WHERE view_key=?`, view); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM container_layout_coordinates WHERE view_key=?`, view); err != nil {
		return err
	}
	summary := map[string]any{"view": view}
	if err := writeLayoutAuditTx(tx, s.nowTime(), "topology.layout.reset", summary, sourceIP); err != nil {
		return err
	}
	return tx.Commit()
}

// writeLayoutAuditTx 写一条布局操作的 owner 审计行。布局只经拓扑图页面保
// 存，来源固定为 Web UI，并携带这次请求的来源地址。
func writeLayoutAuditTx(tx *sql.Tx, at time.Time, action string, summary map[string]any, sourceIP string) error {
	row := OwnerAuditRowAt(at, action, summary)
	row.Source = AuditSourceWebUI
	row.SourceIP = sourceIP
	return writeAuditTx(tx, row)
}

func splitContainerInput(containers []ContainerLayoutInput) (sites, networks []string) {
	for _, coordinate := range containers {
		if coordinate.EntityType == LayoutEntitySite {
			sites = append(sites, coordinate.EntityID)
		} else {
			networks = append(networks, coordinate.EntityID)
		}
	}
	return sites, networks
}

// ensureLayoutEntitiesExistTx 校验坐标引用的设备、站点与网络都存在，避免
// 保存孤儿坐标。
func ensureLayoutEntitiesExistTx(tx *sql.Tx, devices, sites, networks []string) error {
	checks := []struct {
		kind  string
		table string
		ids   []string
	}{
		{"设备", "devices", devices},
		{"站点", "sites", sites},
		{"网络", "networks", networks},
	}
	for _, check := range checks {
		if len(check.ids) == 0 {
			continue
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(check.ids)), ",")
		query := `SELECT COUNT(*) FROM ` + check.table + ` WHERE id IN (` + placeholders + `)`
		args := make([]any, 0, len(check.ids))
		for _, id := range check.ids {
			args = append(args, id)
		}
		var found int
		if err := tx.QueryRow(query, args...).Scan(&found); err != nil {
			return err
		}
		if found != len(check.ids) {
			return fmt.Errorf("%w：%s", ErrLayoutEntityMissing, check.kind)
		}
	}
	return nil
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

// snapshotContainerLayoutCoordinates 读取全部站点与网络坐标，按视图、实体
// 类型与实体排序。
func (s *Store) snapshotContainerLayoutCoordinates() ([]SnapshotContainerLayoutCoordinate, error) {
	rows, err := s.db.Query(`
		SELECT view_key,entity_type,entity_id,x,y,created_at,updated_at
		FROM container_layout_coordinates ORDER BY view_key,entity_type,entity_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]SnapshotContainerLayoutCoordinate, 0)
	for rows.Next() {
		var row SnapshotContainerLayoutCoordinate
		if err := rows.Scan(&row.ViewKey, &row.EntityType, &row.EntityID, &row.X, &row.Y,
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

// importContainerLayoutCoordinatesTx 在导入快照的同一个事务里恢复站点与网
// 络坐标。旧快照没有该段时写入零行。
func importContainerLayoutCoordinatesTx(tx *sql.Tx, rows []SnapshotContainerLayoutCoordinate) error {
	for _, row := range rows {
		if _, err := tx.Exec(
			`INSERT INTO container_layout_coordinates(view_key,entity_type,entity_id,x,y,created_at,updated_at)
			 VALUES(?,?,?,?,?,?,?)`,
			row.ViewKey, row.EntityType, row.EntityID, row.X, row.Y, row.CreatedAt, row.UpdatedAt,
		); err != nil {
			return fmt.Errorf("insert container layout coordinate %s/%s/%s: %w",
				row.ViewKey, row.EntityType, row.EntityID, err)
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

// ListDeviceLayoutCoordinates 返回全部视图的全部设备坐标，按视图与设备排
// 序，供拓扑图页面只读呈现；读取不改变快照修订号。
func (s *Store) ListDeviceLayoutCoordinates() ([]models.DeviceLayoutCoordinate, error) {
	rows, err := s.db.Query(`
		SELECT view_key,device_id,x,y,created_at,updated_at
		FROM device_layout_coordinates ORDER BY view_key,device_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]models.DeviceLayoutCoordinate, 0)
	for rows.Next() {
		var coordinate models.DeviceLayoutCoordinate
		var created, updated int64
		if err := rows.Scan(&coordinate.ViewKey, &coordinate.DeviceID, &coordinate.X,
			&coordinate.Y, &created, &updated); err != nil {
			return nil, err
		}
		coordinate.CreatedAt, coordinate.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
		out = append(out, coordinate)
	}
	return out, rows.Err()
}

// ListContainerLayoutCoordinates 返回全部视图的站点与网络坐标，按视图、
// 实体类型与实体排序；读取不改变快照修订号。
func (s *Store) ListContainerLayoutCoordinates() ([]models.ContainerLayoutCoordinate, error) {
	rows, err := s.db.Query(`
		SELECT view_key,entity_type,entity_id,x,y,created_at,updated_at
		FROM container_layout_coordinates ORDER BY view_key,entity_type,entity_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]models.ContainerLayoutCoordinate, 0)
	for rows.Next() {
		var coordinate models.ContainerLayoutCoordinate
		var created, updated int64
		if err := rows.Scan(&coordinate.ViewKey, &coordinate.EntityType, &coordinate.EntityID,
			&coordinate.X, &coordinate.Y, &created, &updated); err != nil {
			return nil, err
		}
		coordinate.CreatedAt, coordinate.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
		out = append(out, coordinate)
	}
	return out, rows.Err()
}
