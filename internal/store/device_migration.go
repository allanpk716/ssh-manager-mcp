package store

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"ssh-manager-mcp/internal/models"
)

// serverDeviceMigrationSetting 是「现有服务器条目已生成设备」的持久标记。
// 它放在普通设置表里，只表示迁移是否执行过，不保存任何拓扑事实。
const serverDeviceMigrationSetting = "topology.server-device-migration"

// migrateServerDevices 在打开 vault 时执行一次性设备生成。标记、设备、
// 服务器条目归属与迁移审计在同一个事务里提交；重复打开时直接返回。
func migrateServerDevices(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := migrateServerDevicesTx(tx, time.Now(), false); err != nil {
		return err
	}
	return tx.Commit()
}

// migrateServerDevicesTx 是设备生成主体，运行在调用方事务里。force 只给
// 导入不含拓扑数据的旧快照使用：目标库虽然是新库且已有完成标记，这次
// 恢复仍必须按旧数据执行同一套规则。
func migrateServerDevicesTx(tx *sql.Tx, at time.Time, force bool) error {
	if !force {
		done, err := serverDeviceMigrationDoneTx(tx)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}

	rows, err := tx.Query(`
		SELECT server.id,server.name,server.host,
		       pin.key_blob,pin.pin_format
		FROM servers server
		LEFT JOIN entry_host_keys pin ON pin.server_id=server.id
		WHERE server.device_id IS NULL
		ORDER BY lower(server.host),server.host,server.name,server.id`)
	if err != nil {
		return err
	}
	type migrationServer struct {
		id   string
		name string
		host string
	}
	type migrationGroup struct {
		host      string
		hasAnchor bool
		servers   []migrationServer
	}
	type migrationGroupKey struct {
		hostLower   string
		fingerprint string
		serverID    string
		noAnchor    bool
	}
	groups := make(map[migrationGroupKey]*migrationGroup)
	for rows.Next() {
		var item migrationServer
		var keyBlob []byte
		var pinFormat sql.NullString
		if err := rows.Scan(&item.id, &item.name, &item.host, &keyBlob, &pinFormat); err != nil {
			rows.Close()
			return err
		}
		var groupKey migrationGroupKey
		group := &migrationGroup{host: item.host}
		if keyBlob == nil {
			// 没有锚时缺少可靠的同一机器判据，每个条目独立成组，宁可少合
			// 并，也不把两台机器错合并。
			groupKey = migrationGroupKey{serverID: item.id, noAnchor: true}
		} else {
			identity := pinKeyIdentity(keyBlob, pinFormat.String, fmt.Sprintf("raw:%x", keyBlob))
			group.hasAnchor = true
			groupKey = migrationGroupKey{hostLower: strings.ToLower(item.host), fingerprint: identity}
		}
		if existing := groups[groupKey]; existing != nil {
			existing.servers = append(existing.servers, item)
			continue
		}
		group.servers = append(group.servers, item)
		groups[groupKey] = group
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	ordered := make([]*migrationGroup, 0, len(groups))
	for _, group := range groups {
		ordered = append(ordered, group)
	}
	sort.Slice(ordered, func(left, right int) bool {
		first, second := ordered[left], ordered[right]
		leftHost, rightHost := strings.ToLower(first.host), strings.ToLower(second.host)
		if leftHost != rightHost {
			return leftHost < rightHost
		}
		if first.host != second.host {
			return first.host < second.host
		}
		if first.hasAnchor != second.hasAnchor {
			return first.hasAnchor
		}
		if len(first.servers) == 0 || len(second.servers) == 0 {
			return len(first.servers) < len(second.servers)
		}
		return first.servers[0].name < second.servers[0].name
	})

	auditDevices := make([]map[string]any, 0, len(ordered))
	for _, group := range ordered {
		base := group.host
		if len(group.servers) == 1 {
			base = group.servers[0].name
		}
		name, err := availableDeviceNameTx(tx, base)
		if err != nil {
			return err
		}
		deviceID := newID()
		device := &models.Device{ID: deviceID, Name: name}
		device.CreatedAt, device.UpdatedAt = time.Unix(now(), 0), time.Unix(now(), 0)
		if err := insertDeviceTx(tx, device); err != nil {
			return err
		}
		serverNames := make([]string, 0, len(group.servers))
		for _, server := range group.servers {
			if _, err := tx.Exec(`UPDATE servers SET device_id=? WHERE id=?`, deviceID, server.id); err != nil {
				return err
			}
			serverNames = append(serverNames, server.name)
		}
		auditDevices = append(auditDevices, map[string]any{
			"name": device.Name, "servers": serverNames,
		})
	}
	if len(auditDevices) > 0 {
		if err := writeTopologyAuditTx(tx, at, "device.migrate", map[string]any{
			"devices": auditDevices,
		}); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(
		`INSERT INTO settings(key,value,updated_at) VALUES(?,?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`,
		serverDeviceMigrationSetting, "done", at.Unix(),
	); err != nil {
		return err
	}
	return nil
}

// serverDeviceMigrationDoneTx 读取一次性设备生成标记。
func serverDeviceMigrationDoneTx(tx *sql.Tx) (bool, error) {
	var exists int
	err := tx.QueryRow(`SELECT 1 FROM settings WHERE key=?`, serverDeviceMigrationSetting).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
