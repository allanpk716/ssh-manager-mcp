package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// SnapshotSoftwareObservedItem 是全量备份里的实测软件条目行。
type SnapshotSoftwareObservedItem struct {
	ID               string `json:"id"`
	DeviceID         string `json:"device_id"`
	StableKey        string `json:"stable_key"`
	Category         string `json:"category"`
	Name             string `json:"name"`
	Version          string `json:"version"`
	PackageURL       string `json:"purl,omitempty"`
	BelongsTo        string `json:"belongs_to,omitempty"`
	Architecture     string `json:"architecture,omitempty"`
	ContainerTag     string `json:"container_tag,omitempty"`
	ContainerDigest  string `json:"container_digest,omitempty"`
	ContainerRunning bool   `json:"container_running,omitempty"`
	ExecutablePath   string `json:"executable_path,omitempty"`
	ProcessName      string `json:"process_name,omitempty"`
	ProcessID        int    `json:"process_id,omitempty"`
	CollectedAt      int64  `json:"collected_at"`
}

// SnapshotSoftwareObservedPort 是全量备份里的实测监听端口行。
type SnapshotSoftwareObservedPort struct {
	ID       string `json:"id"`
	ItemID   string `json:"item_id"`
	DeviceID string `json:"device_id"`
	Address  string `json:"address"`
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
	Exposure string `json:"exposure"`
}

// SnapshotSoftwareCollectionStatus 是全量备份里的采集状态行；清单过期不
// 落库，恢复后按读取时刻重新计算。
type SnapshotSoftwareCollectionStatus struct {
	DeviceID          string `json:"device_id"`
	Collector         string `json:"collector"`
	PreferredServerID string `json:"preferred_server_id,omitempty"`
	LastServerID      string `json:"last_server_id,omitempty"`
	LastSuccessAt     int64  `json:"last_success_at"`
	LastFailureAt     int64  `json:"last_failure_at"`
	LastFailureReason string `json:"last_failure_reason,omitempty"`
	LastIncomplete    bool   `json:"last_incomplete,omitempty"`
	CommandSetVersion string `json:"command_set_version,omitempty"`
	SystemType        string `json:"system_type,omitempty"`
	PendingImmediate  bool   `json:"pending_immediate,omitempty"`
}

// SnapshotSoftwareCollectSudoSetting 是全量备份里的 sudo 采集开关行。
type SnapshotSoftwareCollectSudoSetting struct {
	ServerID    string `json:"server_id"`
	SudoEnabled bool   `json:"sudo_enabled"`
	UpdatedAt   int64  `json:"updated_at"`
}

// SnapshotSoftwareChangeHistory 是全量备份里的变化历史行。
type SnapshotSoftwareChangeHistory struct {
	ID         string `json:"id"`
	DeviceID   string `json:"device_id"`
	OccurredAt int64  `json:"occurred_at"`
	Kind       string `json:"kind"`
	Category   string `json:"category"`
	Name       string `json:"name"`
	StableKey  string `json:"stable_key"`
	OldValue   string `json:"old_value,omitempty"`
	NewValue   string `json:"new_value,omitempty"`
}

// snapshotSoftwareCollectionBackup 读取全量备份需要的实测与采集各表。结
// 果按标识符稳定排序，同一数据库两次导出的字段顺序一致；profile 导出不
// 调用这个函数。
func (s *Store) snapshotSoftwareCollectionBackup() (
	[]SnapshotSoftwareObservedItem,
	[]SnapshotSoftwareObservedPort,
	[]SnapshotSoftwareCollectionStatus,
	[]SnapshotSoftwareCollectSudoSetting,
	[]SnapshotSoftwareChangeHistory,
	error,
) {
	items := make([]SnapshotSoftwareObservedItem, 0)
	itemRows, err := s.db.Query(`
		SELECT id,device_id,stable_key,category,name,version,package_url,belongs_to,
		       architecture,container_tag,container_digest,container_running,executable_path,
		       process_name,process_id,collected_at
		FROM software_observed_items ORDER BY id`)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	for itemRows.Next() {
		var row SnapshotSoftwareObservedItem
		if err := itemRows.Scan(
			&row.ID, &row.DeviceID, &row.StableKey, &row.Category, &row.Name, &row.Version,
			&row.PackageURL, &row.BelongsTo, &row.Architecture, &row.ContainerTag,
			&row.ContainerDigest, &row.ContainerRunning, &row.ExecutablePath,
			&row.ProcessName, &row.ProcessID, &row.CollectedAt,
		); err != nil {
			itemRows.Close()
			return nil, nil, nil, nil, nil, err
		}
		items = append(items, row)
	}
	if err := itemRows.Err(); err != nil {
		itemRows.Close()
		return nil, nil, nil, nil, nil, err
	}
	if err := itemRows.Close(); err != nil {
		return nil, nil, nil, nil, nil, err
	}

	ports := make([]SnapshotSoftwareObservedPort, 0)
	portRows, err := s.db.Query(`
		SELECT id,item_id,device_id,address,port,protocol,exposure
		FROM software_observed_ports ORDER BY item_id,id`)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	for portRows.Next() {
		var row SnapshotSoftwareObservedPort
		if err := portRows.Scan(
			&row.ID, &row.ItemID, &row.DeviceID, &row.Address, &row.Port,
			&row.Protocol, &row.Exposure,
		); err != nil {
			portRows.Close()
			return nil, nil, nil, nil, nil, err
		}
		ports = append(ports, row)
	}
	if err := portRows.Err(); err != nil {
		portRows.Close()
		return nil, nil, nil, nil, nil, err
	}
	if err := portRows.Close(); err != nil {
		return nil, nil, nil, nil, nil, err
	}

	statuses := make([]SnapshotSoftwareCollectionStatus, 0)
	statusRows, err := s.db.Query(`
		SELECT device_id,collector,COALESCE(preferred_server_id,''),COALESCE(last_server_id,''),
		       last_success_at,last_failure_at,last_failure_reason,last_incomplete,
		       command_set_version,system_type,pending_immediate
		FROM software_collection_status ORDER BY device_id`)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	for statusRows.Next() {
		var row SnapshotSoftwareCollectionStatus
		if err := statusRows.Scan(
			&row.DeviceID, &row.Collector, &row.PreferredServerID, &row.LastServerID,
			&row.LastSuccessAt, &row.LastFailureAt, &row.LastFailureReason, &row.LastIncomplete,
			&row.CommandSetVersion, &row.SystemType, &row.PendingImmediate,
		); err != nil {
			statusRows.Close()
			return nil, nil, nil, nil, nil, err
		}
		statuses = append(statuses, row)
	}
	if err := statusRows.Err(); err != nil {
		statusRows.Close()
		return nil, nil, nil, nil, nil, err
	}
	if err := statusRows.Close(); err != nil {
		return nil, nil, nil, nil, nil, err
	}

	settings := make([]SnapshotSoftwareCollectSudoSetting, 0)
	settingRows, err := s.db.Query(`
		SELECT server_id,sudo_enabled,updated_at
		FROM software_collection_settings ORDER BY server_id`)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	for settingRows.Next() {
		var row SnapshotSoftwareCollectSudoSetting
		if err := settingRows.Scan(&row.ServerID, &row.SudoEnabled, &row.UpdatedAt); err != nil {
			settingRows.Close()
			return nil, nil, nil, nil, nil, err
		}
		settings = append(settings, row)
	}
	if err := settingRows.Err(); err != nil {
		settingRows.Close()
		return nil, nil, nil, nil, nil, err
	}
	if err := settingRows.Close(); err != nil {
		return nil, nil, nil, nil, nil, err
	}

	history := make([]SnapshotSoftwareChangeHistory, 0)
	historyRows, err := s.db.Query(`
		SELECT id,device_id,occurred_at,kind,category,name,stable_key,old_value,new_value
		FROM software_change_history ORDER BY device_id,occurred_at,id`)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	for historyRows.Next() {
		var row SnapshotSoftwareChangeHistory
		if err := historyRows.Scan(
			&row.ID, &row.DeviceID, &row.OccurredAt, &row.Kind, &row.Category,
			&row.Name, &row.StableKey, &row.OldValue, &row.NewValue,
		); err != nil {
			historyRows.Close()
			return nil, nil, nil, nil, nil, err
		}
		history = append(history, row)
	}
	if err := historyRows.Err(); err != nil {
		historyRows.Close()
		return nil, nil, nil, nil, nil, err
	}
	if err := historyRows.Close(); err != nil {
		return nil, nil, nil, nil, nil, err
	}
	return items, ports, statuses, settings, history, nil
}

// importSoftwareCollectionBackupTx 在 owner 全量恢复事务里写回实测与采集
// 各表。客户端缓存导入不调用这个函数。
func importSoftwareCollectionBackupTx(tx *sql.Tx, snap *Snapshot) error {
	for _, row := range snap.SoftwareObservedItems {
		if _, err := tx.Exec(`
			INSERT INTO software_observed_items(
				id,device_id,stable_key,category,name,name_lower,version,package_url,belongs_to,
				architecture,container_tag,container_digest,container_running,executable_path,
				process_name,process_id,collected_at
			) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			row.ID, row.DeviceID, row.StableKey, row.Category, row.Name,
			strings.ToLower(row.Name), row.Version, row.PackageURL, row.BelongsTo,
			row.Architecture, row.ContainerTag, row.ContainerDigest, row.ContainerRunning,
			row.ExecutablePath, row.ProcessName, row.ProcessID, row.CollectedAt,
		); err != nil {
			return fmt.Errorf("insert software observed item %s: %w", row.ID, err)
		}
	}
	for _, row := range snap.SoftwareObservedPorts {
		if _, err := tx.Exec(`
			INSERT INTO software_observed_ports(
				id,item_id,device_id,address,port,protocol,exposure
			) VALUES(?,?,?,?,?,?,?)`,
			row.ID, row.ItemID, row.DeviceID, row.Address, row.Port, row.Protocol, row.Exposure,
		); err != nil {
			return fmt.Errorf("insert software observed port %s: %w", row.ID, err)
		}
	}
	for _, row := range snap.SoftwareCollectionStatuses {
		if _, err := tx.Exec(`
			INSERT INTO software_collection_status(
				device_id,collector,preferred_server_id,last_server_id,last_success_at,
				last_failure_at,last_failure_reason,last_incomplete,command_set_version,
				system_type,pending_immediate
			) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			row.DeviceID, row.Collector, row.PreferredServerID, row.LastServerID,
			row.LastSuccessAt, row.LastFailureAt, row.LastFailureReason, row.LastIncomplete,
			row.CommandSetVersion, row.SystemType, row.PendingImmediate,
		); err != nil {
			return fmt.Errorf("insert software collection status %s: %w", row.DeviceID, err)
		}
	}
	for _, row := range snap.SoftwareCollectSudoSettings {
		if _, err := tx.Exec(`
			INSERT INTO software_collection_settings(server_id,sudo_enabled,updated_at)
			VALUES(?,?,?)`, row.ServerID, row.SudoEnabled, row.UpdatedAt,
		); err != nil {
			return fmt.Errorf("insert software collect sudo setting %s: %w", row.ServerID, err)
		}
	}
	for _, row := range snap.SoftwareChangeHistory {
		if _, err := tx.Exec(`
			INSERT INTO software_change_history(
				id,device_id,occurred_at,kind,category,name,stable_key,old_value,new_value
			) VALUES(?,?,?,?,?,?,?,?,?)`,
			row.ID, row.DeviceID, row.OccurredAt, row.Kind, row.Category,
			row.Name, row.StableKey, row.OldValue, row.NewValue,
		); err != nil {
			return fmt.Errorf("insert software change history %s: %w", row.ID, err)
		}
	}
	return nil
}
