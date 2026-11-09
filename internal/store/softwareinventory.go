package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"ssh-manager-mcp/internal/models"
)

// softwareInventorySchema 是软件清单声明表。两张表都直接挂设备外键，
// 设备删除时随设备一起删除；它们不进入快照修订号触发表。
const softwareInventorySchema = `
CREATE TABLE IF NOT EXISTS software_declarations (
  id TEXT PRIMARY KEY,
  device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  category TEXT NOT NULL CHECK(category IN ('operating_system','service_program','container_image','driver_firmware','system_package','unidentified_process')),
  name TEXT NOT NULL,
  name_lower TEXT NOT NULL,
  version TEXT NOT NULL,
  package_url TEXT NOT NULL DEFAULT '',
  cpe TEXT NOT NULL DEFAULT '',
  belongs_to TEXT NOT NULL DEFAULT '',
  note TEXT NOT NULL DEFAULT '',
  claimed_observed_key TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'active' CHECK(status IN ('active','draft')),
  confirmed_at INTEGER NOT NULL,
  created_source TEXT NOT NULL DEFAULT '',
  last_source TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS software_declarations_device_category_name
  ON software_declarations(device_id, category, name_lower, version);
CREATE TABLE IF NOT EXISTS software_declaration_ports (
  id TEXT PRIMARY KEY,
  declaration_id TEXT NOT NULL REFERENCES software_declarations(id) ON DELETE CASCADE,
  device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  address TEXT NOT NULL,
  port INTEGER NOT NULL CHECK(port BETWEEN 1 AND 65535),
  protocol TEXT NOT NULL CHECK(protocol IN ('tcp','udp')),
  exposure TEXT NOT NULL CHECK(exposure IN ('local_machine','specific_address','all_addresses','local_area_network','virtual_local_area_network','public_network')),
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE(declaration_id, address, port, protocol)
);
`

var ErrSoftwareDeviceNotVisible = errors.New("设备不在这次软件清单查询的可见范围内")

// SoftwareDeviceScope 是软件清单查询的可见设备范围。owner 使用全部设备；
// profile 使用它被授权的服务器条目集合，只有这些条目所在的受管设备可见。
type SoftwareDeviceScope struct {
	all       bool
	serverIDs map[string]bool
}

// AllSoftwareDevices 返回 owner 的可见范围：包括全部受管设备与不受管设备。
func AllSoftwareDevices() SoftwareDeviceScope {
	return SoftwareDeviceScope{all: true}
}

// SoftwareDevicesForServers 返回 profile 的可见范围。serverIDs 是这个
// profile 被授权的服务器条目标识符；至少有一个条目在同一设备上时，那台
// 设备的软件清单可见。
func SoftwareDevicesForServers(serverIDs ...string) SoftwareDeviceScope {
	ids := make(map[string]bool, len(serverIDs))
	for _, id := range serverIDs {
		if id != "" {
			ids[id] = true
		}
	}
	return SoftwareDeviceScope{serverIDs: ids}
}

// SoftwareDeclarationView 是声明在查询结果里的完整可见形态。
type SoftwareDeclarationView struct {
	ID                 string
	DeviceID           string
	DeviceName         string
	Category           models.SoftwareCategory
	Name               string
	Version            string
	PackageURL         string
	CPE                string
	ListeningPorts     []models.SoftwareListeningPort
	BelongsTo          string
	Note               string
	ClaimedObservedKey string
	Status             string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// SoftwareDeviceInventory 是一台设备的声明集合。设备存在但没有声明时，
// Declarations 为空切片，而不是错误。
type SoftwareDeviceInventory struct {
	DeviceID     string
	DeviceName   string
	Declarations []SoftwareDeclarationView
}

// AddSoftwareDeclaration 新增一条生效声明。声明行、监听端口行与审计行在
// 同一个事务里提交；来源只进入审计与声明来源字段，不进入快照修订号。
func (s *Store) AddSoftwareDeclaration(
	declaration models.SoftwareDeclaration,
	source WriteSource,
) (string, error) {
	if s.readOnly {
		return "", ErrReadOnly
	}
	declaration.Device = strings.TrimSpace(declaration.Device)
	declaration.Name = strings.TrimSpace(declaration.Name)
	if err := models.ValidateSoftwareDeclaration(declaration); err != nil {
		return "", err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	device, err := deviceByNameTx(tx, declaration.Device)
	if err != nil {
		return "", err
	}
	at := s.nowTime()
	id := newID()
	if err := insertSoftwareDeclarationTx(tx, id, device.ID, declaration, "active", at, at, source); err != nil {
		return "", err
	}
	if err := writeSoftwareAuditTx(tx, at, "software-declaration-add", map[string]any{
		"device": device.Name, "category": declaration.Category.Label(),
		"name": declaration.Name,
	}, source); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

// snapshotSoftwareInventory 读取全量备份需要的软件声明与监听端口。结果按
// 标识符稳定排序，同一数据库两次导出的字段顺序一致。
func (s *Store) snapshotSoftwareInventory() ([]SnapshotSoftwareDeclaration, []SnapshotSoftwareDeclarationPort, error) {
	declarations := make([]SnapshotSoftwareDeclaration, 0)
	declarationRows, err := s.db.Query(`
		SELECT id,device_id,category,name,version,package_url,cpe,belongs_to,note,
		       claimed_observed_key,status,confirmed_at,created_source,last_source,created_at,updated_at
		FROM software_declarations ORDER BY id`)
	if err != nil {
		return nil, nil, err
	}
	for declarationRows.Next() {
		var row SnapshotSoftwareDeclaration
		if err := declarationRows.Scan(
			&row.ID, &row.DeviceID, &row.Category, &row.Name, &row.Version, &row.PackageURL,
			&row.CPE, &row.BelongsTo, &row.Note, &row.ClaimedObservedKey, &row.Status,
			&row.ConfirmedAt, &row.CreatedSource, &row.LastSource, &row.CreatedAt, &row.UpdatedAt,
		); err != nil {
			declarationRows.Close()
			return nil, nil, err
		}
		declarations = append(declarations, row)
	}
	if err := declarationRows.Err(); err != nil {
		declarationRows.Close()
		return nil, nil, err
	}
	if err := declarationRows.Close(); err != nil {
		return nil, nil, err
	}

	ports := make([]SnapshotSoftwareDeclarationPort, 0)
	portRows, err := s.db.Query(`
		SELECT id,declaration_id,device_id,address,port,protocol,exposure,created_at,updated_at
		FROM software_declaration_ports ORDER BY declaration_id,id`)
	if err != nil {
		return nil, nil, err
	}
	for portRows.Next() {
		var row SnapshotSoftwareDeclarationPort
		if err := portRows.Scan(
			&row.ID, &row.DeclarationID, &row.DeviceID, &row.Address, &row.Port,
			&row.Protocol, &row.Exposure, &row.CreatedAt, &row.UpdatedAt,
		); err != nil {
			portRows.Close()
			return nil, nil, err
		}
		ports = append(ports, row)
	}
	if err := portRows.Err(); err != nil {
		portRows.Close()
		return nil, nil, err
	}
	if err := portRows.Close(); err != nil {
		return nil, nil, err
	}
	return declarations, ports, nil
}

// importSoftwareInventoryTx 在 owner 全量恢复事务里写回软件清单。客户端
// 缓存导入不会调用这个函数。
func importSoftwareInventoryTx(
	tx *sql.Tx,
	declarations []SnapshotSoftwareDeclaration,
	ports []SnapshotSoftwareDeclarationPort,
) error {
	for _, row := range declarations {
		if _, err := tx.Exec(`
			INSERT INTO software_declarations(
				id,device_id,category,name,name_lower,version,package_url,cpe,belongs_to,note,
				claimed_observed_key,status,confirmed_at,created_source,last_source,created_at,updated_at
			) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			row.ID, row.DeviceID, row.Category, row.Name, strings.ToLower(row.Name), row.Version,
			row.PackageURL, row.CPE, row.BelongsTo, row.Note, row.ClaimedObservedKey, row.Status,
			row.ConfirmedAt, row.CreatedSource, row.LastSource, row.CreatedAt, row.UpdatedAt,
		); err != nil {
			return fmt.Errorf("insert software declaration %s: %w", row.ID, err)
		}
	}
	for _, row := range ports {
		if _, err := tx.Exec(`
			INSERT INTO software_declaration_ports(
				id,declaration_id,device_id,address,port,protocol,exposure,created_at,updated_at
			) VALUES(?,?,?,?,?,?,?,?,?)`,
			row.ID, row.DeclarationID, row.DeviceID, row.Address, row.Port,
			row.Protocol, row.Exposure, row.CreatedAt, row.UpdatedAt,
		); err != nil {
			return fmt.Errorf("insert software declaration port %s: %w", row.ID, err)
		}
	}
	return nil
}

// EditSoftwareDeclaration 按声明标识符修改字段。监听端口切片用替换语义，
// 其余指针为空表示不修改。数据与审计同事务提交。
func (s *Store) EditSoftwareDeclaration(
	id string,
	edit models.SoftwareDeclarationEdit,
	source WriteSource,
) (*SoftwareDeclarationView, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	before, err := softwareDeclarationByIDTx(tx, strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	desired := models.SoftwareDeclaration{
		Device: before.DeviceName, Category: before.Category, Name: before.Name,
		Version: before.Version, PackageURL: before.PackageURL, CPE: before.CPE,
		ListeningPorts: before.ListeningPorts, BelongsTo: before.BelongsTo, Note: before.Note,
	}
	changed := map[string]bool{}
	if edit.Device != nil {
		desired.Device = strings.TrimSpace(*edit.Device)
		changed["device"] = true
	}
	if edit.Category != nil {
		desired.Category = *edit.Category
		changed["category"] = true
	}
	if edit.Name != nil {
		desired.Name = strings.TrimSpace(*edit.Name)
		changed["name"] = true
	}
	if edit.Version != nil {
		desired.Version = *edit.Version
		changed["version"] = true
	}
	if edit.PackageURL != nil {
		desired.PackageURL = *edit.PackageURL
		changed["purl"] = true
	}
	if edit.CPE != nil {
		desired.CPE = *edit.CPE
		changed["cpe"] = true
	}
	if edit.ListeningPorts != nil {
		desired.ListeningPorts = *edit.ListeningPorts
		changed["ports"] = true
	}
	if edit.BelongsTo != nil {
		desired.BelongsTo = *edit.BelongsTo
		changed["belongs_to"] = true
	}
	if edit.Note != nil {
		desired.Note = *edit.Note
		changed["note"] = true
	}
	if err := models.ValidateSoftwareDeclaration(desired); err != nil {
		return nil, err
	}
	targetDevice, err := deviceByNameTx(tx, desired.Device)
	if err != nil {
		return nil, err
	}
	at := s.nowTime()
	if _, err := tx.Exec(`
		UPDATE software_declarations
		SET device_id=?,category=?,name=?,name_lower=?,version=?,package_url=?,cpe=?,belongs_to=?,note=?,last_source=?,updated_at=?
		WHERE id=?`,
		targetDevice.ID, desired.Category, desired.Name, strings.ToLower(desired.Name), desired.Version,
		desired.PackageURL, desired.CPE, desired.BelongsTo, desired.Note, source.kind, at.Unix(), before.ID,
	); err != nil {
		return nil, err
	}
	if targetDevice.ID != before.DeviceID {
		if _, err := tx.Exec(`
			UPDATE software_declaration_ports SET device_id=?,updated_at=? WHERE declaration_id=?`,
			targetDevice.ID, at.Unix(), before.ID,
		); err != nil {
			return nil, err
		}
	}
	if edit.ListeningPorts != nil {
		if err := replaceSoftwarePortsTx(tx, before.ID, targetDevice.ID, desired.ListeningPorts, before.CreatedAt, at); err != nil {
			return nil, err
		}
	}
	keys := make([]string, 0, len(changed))
	for key := range changed {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if err := writeSoftwareAuditTx(tx, at, "software-declaration-edit", map[string]any{
		"device": targetDevice.Name, "category": desired.Category.Label(),
		"name": desired.Name, "fields": keys,
	}, source); err != nil {
		return nil, err
	}
	after, err := softwareDeclarationByIDTx(tx, before.ID)
	if err != nil {
		return nil, err
	}
	return after, tx.Commit()
}

// DeleteSoftwareDeclaration 删除一条声明及其监听端口，并写审计。
func (s *Store) DeleteSoftwareDeclaration(id string, source WriteSource) (*SoftwareDeclarationView, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	before, err := softwareDeclarationByIDTx(tx, strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`DELETE FROM software_declarations WHERE id=?`, before.ID); err != nil {
		return nil, err
	}
	if err := writeSoftwareAuditTx(tx, s.nowTime(), "software-declaration-rm", map[string]any{
		"device": before.DeviceName, "category": before.Category.Label(), "name": before.Name,
	}, source); err != nil {
		return nil, err
	}
	return before, tx.Commit()
}

// SoftwareDeclarationsForDevice 按设备名读取生效声明。范围不足时返回
// ErrSoftwareDeviceNotVisible，不区分设备不存在与不可见。
func (s *Store) SoftwareDeclarationsForDevice(
	deviceName string,
	scope SoftwareDeviceScope,
) (*SoftwareDeviceInventory, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	device, err := deviceByNameTx(tx, strings.TrimSpace(deviceName))
	if err != nil {
		return nil, err
	}
	if err := ensureSoftwareDeviceVisibleTx(tx, device.ID, scope); err != nil {
		return nil, err
	}
	declarations, err := softwareDeclarationsWhereTx(tx, `declaration.device_id=?`, device.ID)
	if err != nil {
		return nil, err
	}
	return &SoftwareDeviceInventory{
		DeviceID: device.ID, DeviceName: device.Name, Declarations: declarations,
	}, tx.Commit()
}

// SearchSoftwareDeclarations 按软件名做大小写不敏感的子串搜索，并套用
// 可见设备范围。
func (s *Store) SearchSoftwareDeclarations(
	namePart string,
	scope SoftwareDeviceScope,
) ([]SoftwareDeclarationView, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	deviceIDs, err := visibleSoftwareDeviceIDsTx(tx, scope)
	if err != nil {
		return nil, err
	}
	if scope.all {
		return softwareDeclarationsWhereTx(tx, `declaration.name_lower LIKE ?`,
			"%"+strings.ToLower(namePart)+"%")
	}
	if len(deviceIDs) == 0 {
		return []SoftwareDeclarationView{}, tx.Commit()
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(deviceIDs)), ",")
	args := make([]any, 0, len(deviceIDs)+1)
	for _, id := range deviceIDs {
		args = append(args, id)
	}
	args = append(args, "%"+strings.ToLower(namePart)+"%")
	return softwareDeclarationsWhereTx(tx, `
		declaration.device_id IN (`+placeholders+`) AND declaration.name_lower LIKE ?`, args...)
}

func insertSoftwareDeclarationTx(
	tx *sql.Tx,
	id, deviceID string,
	declaration models.SoftwareDeclaration,
	status string,
	createdAt, confirmedAt time.Time,
	source WriteSource,
) error {
	if _, err := tx.Exec(`
		INSERT INTO software_declarations(
			id,device_id,category,name,name_lower,version,package_url,cpe,belongs_to,note,
			claimed_observed_key,status,confirmed_at,created_source,last_source,created_at,updated_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, deviceID, declaration.Category, declaration.Name, strings.ToLower(declaration.Name),
		declaration.Version, declaration.PackageURL, declaration.CPE, declaration.BelongsTo, declaration.Note,
		"", status, confirmedAt.Unix(), source.kind, source.kind, createdAt.Unix(), createdAt.Unix(),
	); err != nil {
		return err
	}
	return replaceSoftwarePortsTx(tx, id, deviceID, declaration.ListeningPorts, createdAt, createdAt)
}

func replaceSoftwarePortsTx(
	tx *sql.Tx,
	declarationID, deviceID string,
	ports []models.SoftwareListeningPort,
	createdAt, updatedAt time.Time,
) error {
	if _, err := tx.Exec(`DELETE FROM software_declaration_ports WHERE declaration_id=?`, declarationID); err != nil {
		return err
	}
	for _, port := range ports {
		if _, err := tx.Exec(`
			INSERT INTO software_declaration_ports(
				id,declaration_id,device_id,address,port,protocol,exposure,created_at,updated_at
			) VALUES(?,?,?,?,?,?,?,?,?)`,
			newID(), declarationID, deviceID, port.Address, port.Port, port.Protocol,
			port.Exposure, createdAt.Unix(), updatedAt.Unix(),
		); err != nil {
			return err
		}
	}
	return nil
}

func softwareDeclarationByIDTx(tx *sql.Tx, id string) (*SoftwareDeclarationView, error) {
	rows, err := softwareDeclarationsWhereTx(tx, `declaration.id=?`, id)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("软件声明 %q 不存在", id)
	}
	return &rows[0], nil
}

func softwareDeclarationsWhereTx(tx *sql.Tx, where string, args ...any) ([]SoftwareDeclarationView, error) {
	rows, err := tx.Query(`
		SELECT declaration.id,declaration.device_id,device.name,declaration.category,declaration.name,
		       declaration.version,declaration.package_url,declaration.cpe,declaration.belongs_to,declaration.note,
		       declaration.claimed_observed_key,declaration.status,declaration.created_at,declaration.updated_at
		FROM software_declarations declaration
		JOIN devices device ON device.id=declaration.device_id
		WHERE declaration.status='active' AND `+where+`
		ORDER BY CASE declaration.category
			WHEN 'operating_system' THEN 1
			WHEN 'service_program' THEN 2
			WHEN 'container_image' THEN 3
			WHEN 'driver_firmware' THEN 4
			WHEN 'system_package' THEN 5
			WHEN 'unidentified_process' THEN 6 END,
			declaration.name,declaration.version,declaration.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]SoftwareDeclarationView, 0)
	for rows.Next() {
		var view SoftwareDeclarationView
		var createdAt, updatedAt int64
		if err := rows.Scan(
			&view.ID, &view.DeviceID, &view.DeviceName, &view.Category, &view.Name,
			&view.Version, &view.PackageURL, &view.CPE, &view.BelongsTo, &view.Note,
			&view.ClaimedObservedKey, &view.Status, &createdAt, &updatedAt,
		); err != nil {
			return nil, err
		}
		view.CreatedAt, view.UpdatedAt = time.Unix(createdAt, 0), time.Unix(updatedAt, 0)
		out = append(out, view)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := attachSoftwarePortsTx(tx, out); err != nil {
		return nil, err
	}
	return out, nil
}

func attachSoftwarePortsTx(tx *sql.Tx, views []SoftwareDeclarationView) error {
	for i := range views {
		rows, err := tx.Query(`
			SELECT address,port,protocol,exposure FROM software_declaration_ports
			WHERE declaration_id=? ORDER BY port,protocol,address`, views[i].ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var port models.SoftwareListeningPort
			if err := rows.Scan(&port.Address, &port.Port, &port.Protocol, &port.Exposure); err != nil {
				rows.Close()
				return err
			}
			views[i].ListeningPorts = append(views[i].ListeningPorts, port)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	return nil
}

func ensureSoftwareDeviceVisibleTx(tx *sql.Tx, deviceID string, scope SoftwareDeviceScope) error {
	if scope.all {
		return nil
	}
	rows, err := tx.Query(`SELECT id FROM servers WHERE device_id=?`, deviceID)
	if err != nil {
		return err
	}
	defer rows.Close()
	visible := false
	for rows.Next() {
		var serverID string
		if err := rows.Scan(&serverID); err != nil {
			return err
		}
		if scope.serverIDs[serverID] {
			visible = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !visible {
		return ErrSoftwareDeviceNotVisible
	}
	return nil
}

func visibleSoftwareDeviceIDsTx(tx *sql.Tx, scope SoftwareDeviceScope) ([]string, error) {
	if scope.all {
		return nil, nil
	}
	rows, err := tx.Query(`
		SELECT DISTINCT device_id FROM servers
		WHERE device_id IS NOT NULL AND id IN (`+placeholdersForIDs(scope.serverIDs)+`)`, idsFromSet(scope.serverIDs)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func placeholdersForIDs(ids map[string]bool) string {
	return strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
}

func idsFromSet(ids map[string]bool) []any {
	out := make([]any, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].(string) < out[j].(string) })
	return out
}

func writeSoftwareAuditTx(
	tx *sql.Tx,
	at time.Time,
	action string,
	summary map[string]any,
	source WriteSource,
) error {
	if source.deviceCode != "" {
		if summary == nil {
			summary = map[string]any{}
		}
		summary["device_code"] = source.deviceCode
	}
	return writeAuditTx(tx, AuditRow{
		TS: at, Action: action, Command: MarshalAuditSummary(summary),
		Status: "ok", Source: source.kind, SourceIP: source.address,
	})
}
