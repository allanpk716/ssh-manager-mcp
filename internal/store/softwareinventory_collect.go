package store

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"ssh-manager-mcp/internal/models"
)

// softwareCollectionSchema 是软件清单实测采集的表。全部以设备为外键，设
// 备删除时级联删除；sudo 开关以服务器条目为键存在软件清单自己的设置表
// 里，不加到服务器条目表上，因此切换开关不会触发快照修订号变化。这些
// 表不加入客户端快照。
const softwareCollectionSchema = `
CREATE TABLE IF NOT EXISTS software_observed_items (
  id TEXT PRIMARY KEY,
  device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  stable_key TEXT NOT NULL,
  category TEXT NOT NULL CHECK(category IN ('operating_system','service_program','container_image','driver_firmware','system_package','unidentified_process')),
  name TEXT NOT NULL,
  name_lower TEXT NOT NULL,
  version TEXT NOT NULL DEFAULT '',
  package_url TEXT NOT NULL DEFAULT '',
  belongs_to TEXT NOT NULL DEFAULT '',
  architecture TEXT NOT NULL DEFAULT '',
  container_tag TEXT NOT NULL DEFAULT '',
  container_digest TEXT NOT NULL DEFAULT '',
  container_running INTEGER NOT NULL DEFAULT 0,
  executable_path TEXT NOT NULL DEFAULT '',
  process_name TEXT NOT NULL DEFAULT '',
  process_id INTEGER NOT NULL DEFAULT 0,
  collected_at INTEGER NOT NULL,
  UNIQUE(device_id, stable_key)
);
CREATE INDEX IF NOT EXISTS software_observed_items_device_category
  ON software_observed_items(device_id, category, name_lower, version);
CREATE TABLE IF NOT EXISTS software_observed_ports (
  id TEXT PRIMARY KEY,
  item_id TEXT NOT NULL REFERENCES software_observed_items(id) ON DELETE CASCADE,
  device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  address TEXT NOT NULL,
  port INTEGER NOT NULL CHECK(port BETWEEN 1 AND 65535),
  protocol TEXT NOT NULL CHECK(protocol IN ('tcp','udp')),
  exposure TEXT NOT NULL CHECK(exposure IN ('local_machine','specific_address','all_addresses')),
  UNIQUE(item_id, address, port, protocol)
);
CREATE TABLE IF NOT EXISTS software_collection_status (
  device_id TEXT PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
  collector TEXT NOT NULL DEFAULT '',
  preferred_server_id TEXT NOT NULL DEFAULT '',
  last_server_id TEXT NOT NULL DEFAULT '',
  last_success_at INTEGER NOT NULL DEFAULT 0,
  last_failure_at INTEGER NOT NULL DEFAULT 0,
  last_failure_reason TEXT NOT NULL DEFAULT '',
  last_incomplete INTEGER NOT NULL DEFAULT 0,
  command_set_version TEXT NOT NULL DEFAULT '',
  system_type TEXT NOT NULL DEFAULT '',
  pending_immediate INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS software_collection_settings (
  server_id TEXT PRIMARY KEY REFERENCES servers(id) ON DELETE CASCADE,
  sudo_enabled INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL
);
`

// SoftwareObservedWrite 是一次成功采集要整体写入的数据。
type SoftwareObservedWrite struct {
	Items []models.SoftwareObservedItem
	// Collector 是这次结果的采集执行者；空值表示权威端。
	Collector         models.Collector
	ServerID          string
	Sudo              bool
	CommandSetVersion string
	SystemType        string
	Incomplete        bool
}

// SetSoftwareCollectSudo 设置一个服务器条目的 sudo 采集开关。开关存在软
// 件清单自己的设置表里；修改与审计在同一个事务里提交。
func (s *Store) SetSoftwareCollectSudo(serverRef string, enabled bool, source WriteSource) error {
	if s.readOnly {
		return ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	server, err := serverForRefTx(tx, serverRef)
	if err != nil {
		return err
	}
	at := s.nowTime()
	if _, err := tx.Exec(`
		INSERT INTO software_collection_settings(server_id,sudo_enabled,updated_at)
		VALUES(?,?,?)
		ON CONFLICT(server_id) DO UPDATE SET sudo_enabled=excluded.sudo_enabled,updated_at=excluded.updated_at`,
		server.ID, enabled, at.Unix(),
	); err != nil {
		return err
	}
	if err := writeSoftwareAuditTx(tx, at, "software-collect-sudo", map[string]any{
		"server": server.Name, "sudo": enabled,
	}, source); err != nil {
		return err
	}
	return tx.Commit()
}

// SoftwareCollectSudoEnabled 返回服务器条目标识符到 sudo 采集开关状态的
// 映射；未设置的条目不在映射里（默认关闭）。
func (s *Store) SoftwareCollectSudoEnabled() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT server_id,sudo_enabled FROM software_collection_settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var serverID string
		var enabled bool
		if err := rows.Scan(&serverID, &enabled); err != nil {
			return nil, err
		}
		out[serverID] = enabled
	}
	return out, rows.Err()
}

// FullServersForDevice 返回一台设备的全部服务器条目完整字段，按条目名字
// 排序。软件清单采集需要 shell 类型与 sudo 凭据字段，摘要视图不够用。
func (s *Store) FullServersForDevice(deviceName string) ([]*models.Server, error) {
	device, err := s.GetDevice(deviceName)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`
		SELECT id,name,host,port,user,COALESCE(device_id,''),auth_method,credential_id,sudo_credential_id,shell_type,tags,description,location,hardware,services,role,caveats,expose_host,created_at,updated_at,revision
		FROM servers WHERE device_id=? ORDER BY name,id`, device.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*models.Server
	for rows.Next() {
		server, err := scanServer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, server)
	}
	return out, rows.Err()
}

// ReplaceSoftwareObservedItems 在一个事务里整体替换一台设备的实测条目与监
// 听端口、更新采集状态并写一行采集审计。
func (s *Store) ReplaceSoftwareObservedItems(
	deviceName string,
	write SoftwareObservedWrite,
	source WriteSource,
) error {
	if s.readOnly {
		return ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	device, err := deviceByNameTx(tx, deviceName)
	if err != nil {
		return err
	}
	serverName := ""
	if write.ServerID != "" {
		server, err := serverByIDTx(tx, write.ServerID)
		if err != nil {
			return err
		}
		serverName = server.Name
	}
	at := s.nowTime()
	previous, err := softwareObservedItemsForDeviceTx(tx, device.ID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM software_observed_items WHERE device_id=?`, device.ID); err != nil {
		return err
	}
	for i := range write.Items {
		item := &write.Items[i]
		if item.CollectedAt.IsZero() {
			item.CollectedAt = at
		}
		if item.StableKey == "" {
			item.StableKey = softwareObservedStableKey(item)
		}
		itemID := newID()
		if _, err := tx.Exec(`
			INSERT INTO software_observed_items(
				id,device_id,stable_key,category,name,name_lower,version,package_url,belongs_to,
				architecture,container_tag,container_digest,container_running,executable_path,
				process_name,process_id,collected_at
			) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			itemID, device.ID, item.StableKey, item.Category, item.Name, strings.ToLower(item.Name),
			item.Version, item.PackageURL, item.BelongsTo, item.Architecture,
			item.ContainerTag, item.ContainerDigest, item.ContainerRunning,
			item.ExecutablePath, item.ProcessName, item.ProcessID, item.CollectedAt.Unix(),
		); err != nil {
			return err
		}
		for _, port := range item.ListeningPorts {
			if _, err := tx.Exec(`
				INSERT INTO software_observed_ports(
					id,item_id,device_id,address,port,protocol,exposure
				) VALUES(?,?,?,?,?,?,?)`,
				newID(), itemID, device.ID, port.Address, port.Port,
				port.Protocol, port.Exposure,
			); err != nil {
				return err
			}
		}
	}
	if err := writeSoftwareChangeHistoryTx(tx, device.ID, softwareChangesBetween(
		previous, write.Items, write.Incomplete, at,
	), at); err != nil {
		return err
	}
	result := "ok"
	if write.Incomplete {
		result = "incomplete"
	}
	if err := writeSoftwareCollectionStatusTx(tx, device.ID, write.ServerID, write.Collector, &softwareCollectionSuccess{
		CommandSetVersion: write.CommandSetVersion, SystemType: write.SystemType,
		Incomplete: write.Incomplete, SuccessAt: at,
	}, softwareCollectionFailure{}); err != nil {
		return err
	}
	if err := writeSoftwareAuditTx(tx, at, "software-collect", map[string]any{
		"device": device.Name, "server": serverName,
		"collector": write.Collector.CacheTokenID,
		"sudo":      write.Sudo, "result": result,
	}, source); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordSoftwareCollectionFailure 记录一次采集失败：条目保留不动，失败时
// 间与原因写入采集状态，并写一行采集审计。
func (s *Store) RecordSoftwareCollectionFailure(
	deviceName, reason, serverID string,
	sudo bool,
	source WriteSource,
) error {
	if s.readOnly {
		return ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	device, err := deviceByNameTx(tx, deviceName)
	if err != nil {
		return err
	}
	serverName := ""
	if serverID != "" {
		server, err := serverByIDTx(tx, serverID)
		if err != nil {
			return err
		}
		serverName = server.Name
	}
	at := s.nowTime()
	if err := writeSoftwareCollectionStatusTx(tx, device.ID, "", models.AuthorityCollector(), nil, softwareCollectionFailure{
		At: at, Reason: reason,
	}); err != nil {
		return err
	}
	if err := writeSoftwareAuditTx(tx, at, "software-collect", map[string]any{
		"device": device.Name, "server": serverName,
		"collector": "",
		"sudo":      sudo, "result": "failed", "reason": reason,
	}, source); err != nil {
		return err
	}
	return tx.Commit()
}

// SoftwareCollectionStatusForDevice 返回一台设备的采集状态；从未采集过时
// 返回零值状态而不是错误。
func (s *Store) SoftwareCollectionStatusForDevice(deviceName string) (*models.SoftwareCollectionStatus, error) {
	device, err := s.GetDevice(deviceName)
	if err != nil {
		return nil, err
	}
	status := &models.SoftwareCollectionStatus{
		DeviceID: device.ID, DeviceName: device.Name, Collector: models.AuthorityCollector(),
	}
	if err := s.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM servers WHERE device_id=?)`, device.ID,
	).Scan(&status.Managed); err != nil {
		return nil, err
	}
	row := s.db.QueryRow(`
		SELECT COALESCE(status.collector,''),COALESCE(token.name,''),
		       EXISTS(SELECT 1 FROM cache_tokens revoked
		              WHERE revoked.id=status.collector AND revoked.status='revoked'),
		       COALESCE(status.preferred_server_id,''),COALESCE(status.last_server_id,''),
		       last_success_at,last_failure_at,last_failure_reason,last_incomplete,
		       command_set_version,system_type,pending_immediate
		FROM software_collection_status status
		LEFT JOIN cache_tokens token ON token.id=status.collector
		WHERE status.device_id=?`, device.ID)
	var lastServerID string
	var successAt, failureAt int64
	if err := row.Scan(
		&status.Collector.CacheTokenID, &status.CollectorName, &status.CollectorRevoked,
		&status.PreferredServerID, &lastServerID,
		&successAt, &failureAt, &status.LastFailureReason, &status.LastIncomplete,
		&status.CommandSetVersion, &status.SystemType, &status.PendingImmediate,
	); err != nil {
		if err == sql.ErrNoRows {
			return status, nil
		}
		return nil, err
	}
	status.LastSuccessAt, status.LastFailureAt = time.Unix(successAt, 0), time.Unix(failureAt, 0)
	if lastServerID != "" {
		if server, err := s.GetServer(lastServerID); err == nil {
			status.LastServerID, status.LastServerName = server.ID, server.Name
		}
	}
	return status, nil
}

// SoftwareDeviceVisible 判断一台设备是否在软件清单的可见设备范围内。设
// 备不存在时返回原错误；只是不可见时返回 false 且不报错。
func (s *Store) SoftwareDeviceVisible(
	deviceName string,
	scope SoftwareDeviceScope,
) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	device, err := deviceByNameTx(tx, deviceName)
	if err != nil {
		return false, err
	}
	if err := ensureSoftwareDeviceVisibleTx(tx, device.ID, scope); err != nil {
		if err == ErrSoftwareDeviceNotVisible {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// SoftwareCollectionStatuses 返回全部设备（或 deviceName 非空时的一台设
// 备）的采集状态，按设备名稳定排序。从未采集过的设备返回零值时间与默认
// 采集执行者；Managed 表示设备是否有服务器条目，供清单过期判断使用。
func (s *Store) SoftwareCollectionStatuses(deviceName string) ([]models.SoftwareCollectionStatus, error) {
	query := `
		SELECT device.id,device.name,
		       EXISTS(SELECT 1 FROM servers WHERE servers.device_id=device.id),
		       COALESCE(status.collector,''),COALESCE(token.name,''),
		       EXISTS(SELECT 1 FROM cache_tokens revoked
		              WHERE revoked.id=status.collector AND revoked.status='revoked'),
		       COALESCE(status.preferred_server_id,''),
		       COALESCE(status.last_server_id,''),
		       COALESCE(status.last_success_at,0),
		       COALESCE(status.last_failure_at,0),
		       COALESCE(status.last_failure_reason,''),
		       COALESCE(status.last_incomplete,0),
		       COALESCE(status.command_set_version,''),
		       COALESCE(status.system_type,''),
		       COALESCE(status.pending_immediate,0)
		FROM devices device
		LEFT JOIN software_collection_status status ON status.device_id=device.id
		LEFT JOIN cache_tokens token ON token.id=status.collector`
	args := []any{}
	if name := strings.TrimSpace(deviceName); name != "" {
		query += ` WHERE device.name=?`
		args = append(args, name)
	}
	query += ` ORDER BY device.name,device.id`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]models.SoftwareCollectionStatus, 0)
	lastServerIDs := make([]string, 0)
	for rows.Next() {
		var status models.SoftwareCollectionStatus
		var lastServerID string
		var successAt, failureAt int64
		if err := rows.Scan(
			&status.DeviceID, &status.DeviceName, &status.Managed, &status.Collector,
			&status.CollectorName, &status.CollectorRevoked,
			&status.PreferredServerID, &lastServerID, &successAt, &failureAt,
			&status.LastFailureReason, &status.LastIncomplete, &status.CommandSetVersion,
			&status.SystemType, &status.PendingImmediate,
		); err != nil {
			return nil, err
		}
		status.LastSuccessAt, status.LastFailureAt = time.Unix(successAt, 0), time.Unix(failureAt, 0)
		out = append(out, status)
		lastServerIDs = append(lastServerIDs, lastServerID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	// 单连接 SQLite 下，外层结果集未关闭时不能再发查询；这里先收完全部
	// 行，再补采集用条目名。
	for i, lastServerID := range lastServerIDs {
		if lastServerID == "" {
			continue
		}
		if server, err := s.GetServer(lastServerID); err == nil {
			out[i].LastServerID, out[i].LastServerName = server.ID, server.Name
		}
	}
	return out, nil
}

// SoftwareObservedItemsForDevice 读回一台设备的全部实测条目与监听端口，
// 供查询接口与测试观察整体替换的结果。
func (s *Store) SoftwareObservedItemsForDevice(deviceName string) ([]models.SoftwareObservedItem, error) {
	device, err := s.GetDevice(deviceName)
	if err != nil {
		return nil, err
	}
	return softwareObservedItemsForDeviceTx(s.db, device.ID)
}

// softwareCollectionSuccess 是一次成功采集要写进状态的新字段。
type softwareCollectionSuccess struct {
	CommandSetVersion string
	SystemType        string
	Incomplete        bool
	SuccessAt         time.Time
}

// softwareCollectionFailure 是一次失败采集要写进状态的新字段。
type softwareCollectionFailure struct {
	At     time.Time
	Reason string
}

// writeSoftwareCollectionStatusTx 写入或更新采集状态。成功与失败各自只覆
// 盖自己的字段，保留对方的历史值。
func writeSoftwareCollectionStatusTx(
	tx *sql.Tx,
	deviceID, lastServerID string,
	collector models.Collector,
	success *softwareCollectionSuccess,
	failure softwareCollectionFailure,
) error {
	if success != nil {
		_, err := tx.Exec(`
			INSERT INTO software_collection_status(
				device_id,collector,last_server_id,last_success_at,last_incomplete,
				command_set_version,system_type
			) VALUES(?,?,?,?,?,?,?)
			ON CONFLICT(device_id) DO UPDATE SET
				last_server_id=excluded.last_server_id,
				last_success_at=excluded.last_success_at,
				last_incomplete=excluded.last_incomplete,
				command_set_version=excluded.command_set_version,
				system_type=excluded.system_type,
				pending_immediate=0`,
			deviceID, collector.CacheTokenID, lastServerID, success.SuccessAt.Unix(),
			success.Incomplete, success.CommandSetVersion, success.SystemType,
		)
		return err
	}
	_, err := tx.Exec(`
		INSERT INTO software_collection_status(
			device_id,collector,last_failure_at,last_failure_reason
		) VALUES(?,?,?,?)
		ON CONFLICT(device_id) DO UPDATE SET
			last_failure_at=excluded.last_failure_at,
			last_failure_reason=excluded.last_failure_reason,
			pending_immediate=0`,
		deviceID, collector.CacheTokenID, failure.At.Unix(), failure.Reason,
	)
	return err
}

func softwareObservedStableKey(item *models.SoftwareObservedItem) string {
	key := string(item.Category) + "\x1f" + strings.ToLower(item.Name) + "\x1f" + item.BelongsTo
	if item.Category == models.SoftwareCategoryUnidentified {
		protocol, port := "", ""
		if len(item.ListeningPorts) > 0 {
			protocol = item.ListeningPorts[0].Protocol
			port = strconv.Itoa(item.ListeningPorts[0].Port)
		}
		key += "\x1f" + port + "\x1f" + protocol + "\x1f" + item.ExecutablePath
	}
	if item.Category == models.SoftwareCategoryContainerImage {
		key += "\x1f" + item.ContainerTag + "\x1f" + item.ContainerDigest
	}
	return key
}

func serverForRefTx(tx *sql.Tx, ref string) (*models.Server, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("服务器条目不能为空")
	}
	server, err := serverByIDTx(tx, ref)
	if err == nil {
		return server, nil
	}
	return serverByNameTx(tx, ref)
}

func serverByIDTx(tx *sql.Tx, id string) (*models.Server, error) {
	return serverWhereTx(tx, `id=?`, id)
}

func serverByNameTx(tx *sql.Tx, name string) (*models.Server, error) {
	return serverWhereTx(tx, `name=? COLLATE NOCASE`, name)
}

func serverWhereTx(tx *sql.Tx, where string, args ...any) (*models.Server, error) {
	rows, err := tx.Query(`
		SELECT id,name,host,port,user,COALESCE(device_id,''),auth_method,credential_id,sudo_credential_id,shell_type,tags,description,location,hardware,services,role,caveats,expose_host,created_at,updated_at,revision
		FROM servers WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, fmt.Errorf("服务器条目不存在")
	}
	server, err := scanServer(rows)
	if err != nil {
		return nil, err
	}
	return server, rows.Err()
}
