package store

import (
	"database/sql"
	"fmt"
	"sort"
	"time"

	"ssh-manager-mcp/internal/models"
)

// softwareChangeHistorySchema 是软件清单变化历史表。行以设备为外键，设
// 备删除时级联删除；表不进入客户端快照与快照修订号触发表。超过一年的
// 行在每次写入采集结果的同一事务里清理，不另起清理任务。
const softwareChangeHistorySchema = `
CREATE TABLE IF NOT EXISTS software_change_history (
  id TEXT PRIMARY KEY,
  device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  occurred_at INTEGER NOT NULL,
  kind TEXT NOT NULL CHECK(kind IN ('added','removed','version_changed','exposure_changed')),
  category TEXT NOT NULL,
  name TEXT NOT NULL,
  stable_key TEXT NOT NULL,
  old_value TEXT NOT NULL DEFAULT '',
  new_value TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS software_change_history_device_time
  ON software_change_history(device_id, occurred_at DESC, id DESC);
`

// softwareChangeRetention 是变化历史的保留期。
const softwareChangeRetention = 365 * 24 * time.Hour

// SoftwareChangeHistoryForDevice 返回一台设备的变化历史，按时间倒序排
// 列；同一时间的多行按行标识符倒序，保证同一事务里的行有稳定顺序。
func (s *Store) SoftwareChangeHistoryForDevice(deviceName string) ([]models.SoftwareChangeEntry, error) {
	device, err := s.GetDevice(deviceName)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`
		SELECT history.id,history.device_id,device.name,history.occurred_at,history.kind,
		       history.category,history.name,history.stable_key,history.old_value,history.new_value
		FROM software_change_history history
		JOIN devices device ON device.id=history.device_id
		WHERE history.device_id=?
		ORDER BY history.occurred_at DESC,history.id DESC`, device.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]models.SoftwareChangeEntry, 0)
	for rows.Next() {
		var row models.SoftwareChangeEntry
		var occurredAt int64
		if err := rows.Scan(
			&row.ID, &row.DeviceID, &row.DeviceName, &occurredAt, &row.Kind,
			&row.Category, &row.Name, &row.StableKey, &row.OldValue, &row.NewValue,
		); err != nil {
			return nil, err
		}
		row.OccurredAt = time.Unix(occurredAt, 0)
		out = append(out, row)
	}
	return out, rows.Err()
}

// writeSoftwareChangeHistoryTx 把一次实测比对得到的变化写入历史表，并清
// 理超过一年的行。两者与采集结果在同一个事务里提交。
func writeSoftwareChangeHistoryTx(
	tx *sql.Tx,
	deviceID string,
	entries []models.SoftwareChangeEntry,
	at time.Time,
) error {
	for _, entry := range entries {
		if _, err := tx.Exec(`
			INSERT INTO software_change_history(
				id,device_id,occurred_at,kind,category,name,stable_key,old_value,new_value
			) VALUES(?,?,?,?,?,?,?,?,?)`,
			newID(), deviceID, entry.OccurredAt.Unix(), entry.Kind, entry.Category,
			entry.Name, entry.StableKey, entry.OldValue, entry.NewValue,
		); err != nil {
			return fmt.Errorf("insert software change history: %w", err)
		}
	}
	cutoff := at.Add(-softwareChangeRetention)
	if _, err := tx.Exec(
		`DELETE FROM software_change_history WHERE device_id=? AND occurred_at<?`,
		deviceID, cutoff.Unix(),
	); err != nil {
		return fmt.Errorf("cleanup software change history: %w", err)
	}
	return nil
}

// softwareChangesBetween 比对上一次与这一次的实测条目，产生要写入的变化
// 历史。incomplete 表示这次结果不完整：没采到的条目不能被误记为移除。
func softwareChangesBetween(
	previous, current []models.SoftwareObservedItem,
	incomplete bool,
	at time.Time,
) []models.SoftwareChangeEntry {
	previousByKey := make(map[string]models.SoftwareObservedItem, len(previous))
	for _, item := range previous {
		previousByKey[item.StableKey] = item
	}
	currentByKey := make(map[string]bool, len(current))
	entries := make([]models.SoftwareChangeEntry, 0)
	for _, item := range current {
		currentByKey[item.StableKey] = true
		old, existed := previousByKey[item.StableKey]
		if !existed {
			entries = append(entries, softwareChangeEntry(
				at, models.SoftwareChangeAdded, item, "", item.Version,
			))
			continue
		}
		if old.Version != item.Version {
			entries = append(entries, softwareChangeEntry(
				at, models.SoftwareChangeVersionChanged, item, old.Version, item.Version,
			))
		}
		entries = append(entries, softwareExposureChanges(at, old, item)...)
	}
	if !incomplete {
		for _, old := range previous {
			if currentByKey[old.StableKey] {
				continue
			}
			entries = append(entries, softwareChangeEntry(
				at, models.SoftwareChangeRemoved, old, old.Version, "",
			))
		}
	}
	// 同一事务里的多行按种类与条目稳定键排序，写入顺序稳定，两次采集
	// 的输出不会因为映射遍历顺序不同而不同。
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Kind != entries[j].Kind {
			return entries[i].Kind < entries[j].Kind
		}
		if entries[i].StableKey != entries[j].StableKey {
			return entries[i].StableKey < entries[j].StableKey
		}
		return entries[i].OldValue < entries[j].OldValue
	})
	return entries
}

func softwareChangeEntry(
	at time.Time,
	kind models.SoftwareChangeKind,
	item models.SoftwareObservedItem,
	oldValue, newValue string,
) models.SoftwareChangeEntry {
	return models.SoftwareChangeEntry{
		OccurredAt: at, Kind: kind, Category: item.Category, Name: item.Name,
		StableKey: item.StableKey, OldValue: oldValue, NewValue: newValue,
	}
}

// softwareDeclarationHistoryEntry 生成一条声明生效时的历史。声明没有实测
// 稳定键，使用声明标识符保证后续编辑与删除仍能对准同一行语义。
func softwareDeclarationHistoryEntry(
	id string,
	declaration models.SoftwareDeclaration,
	at time.Time,
) models.SoftwareChangeEntry {
	return models.SoftwareChangeEntry{
		OccurredAt: at, Kind: models.SoftwareChangeAdded,
		Category: declaration.Category, Name: declaration.Name,
		StableKey: "declaration\x1f" + id, NewValue: declaration.Version,
	}
}

// softwareExposureChanges 比对同一个条目上同一个监听端点（端口加协议）
// 的暴露级别。地址变化导致推导档位变化时同样记录；同一端点有多个地址
// 时取暴露级别最高的一档，避免把同一端口拆成多条噪声历史。
func softwareExposureChanges(
	at time.Time,
	old, current models.SoftwareObservedItem,
) []models.SoftwareChangeEntry {
	oldPorts := map[string]models.SoftwareExposure{}
	for _, port := range old.ListeningPorts {
		endpoint := softwarePortEndpoint(port)
		if existing, ok := oldPorts[endpoint]; !ok || softwareExposureRank(port.Exposure) > softwareExposureRank(existing) {
			oldPorts[endpoint] = port.Exposure
		}
	}
	entries := make([]models.SoftwareChangeEntry, 0)
	currentEndpoints := map[string]bool{}
	for _, port := range current.ListeningPorts {
		endpoint := softwarePortEndpoint(port)
		if currentEndpoints[endpoint] {
			continue
		}
		currentEndpoints[endpoint] = true
		oldExposure, existed := oldPorts[endpoint]
		if !existed || oldExposure == softwareMaxExposure(current.ListeningPorts, port.Port, port.Protocol) {
			continue
		}
		entries = append(entries, softwareChangeEntry(
			at, models.SoftwareChangeExposureChanged, current,
			string(oldExposure), string(softwareMaxExposure(current.ListeningPorts, port.Port, port.Protocol)),
		))
	}
	return entries
}

func softwarePortEndpoint(port models.SoftwareListeningPort) string {
	return fmt.Sprintf("%d\x1f%s", port.Port, port.Protocol)
}

func softwareMaxExposure(
	ports []models.SoftwareListeningPort,
	portNumber int,
	protocol string,
) models.SoftwareExposure {
	var best models.SoftwareExposure
	for _, port := range ports {
		if port.Port != portNumber || port.Protocol != protocol {
			continue
		}
		if best == "" || softwareExposureRank(port.Exposure) > softwareExposureRank(best) {
			best = port.Exposure
		}
	}
	return best
}

func softwareExposureRank(exposure models.SoftwareExposure) int {
	switch exposure {
	case models.SoftwareExposureLocal:
		return 1
	case models.SoftwareExposureSpecificAddress:
		return 2
	case models.SoftwareExposureAllAddresses:
		return 3
	case models.SoftwareExposureLAN:
		return 4
	case models.SoftwareExposureVirtualLAN:
		return 5
	case models.SoftwareExposurePublic:
		return 6
	default:
		return 0
	}
}

// softwareObservedItemsForDeviceTx 在一个事务里读回一台设备的全部实测条
// 目与监听端口。替换实测条目前先用它取上一次结果做变化比对。
func softwareObservedItemsForDeviceTx(q dbtx, deviceID string) ([]models.SoftwareObservedItem, error) {
	rows, err := q.Query(`
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
		item.CollectedAt = time.Unix(collectedAt, 0)
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
		portRows, err := q.Query(`
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
