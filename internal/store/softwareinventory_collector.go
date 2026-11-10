package store

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"ssh-manager-mcp/internal/models"
)

// SetSoftwareCollectionCollector 登记一台设备的采集执行者，并可选指定采集
// 用服务器条目。collectorRef 是设备码的名字，空值表示权威端或单机；实际
// 存储使用设备码编号。serverRef 可用服务器条目名或标识符，为空表示每次
// 采集时再按默认规则选择。客户端实例登记前校验设备码有效、已绑定
// profile，且显式指定的采集用服务器条目既在这台设备上，也在该设备码绑
// 定 profile 的授权范围内；登记与审计在同一个事务里提交。
func (s *Store) SetSoftwareCollectionCollector(
	deviceRef, collectorRef, serverRef string,
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
	device, err := deviceByNameTx(tx, deviceRef)
	if err != nil {
		return err
	}
	collectorRef = strings.TrimSpace(collectorRef)
	collector := models.AuthorityCollector()
	var profileID string
	if collectorRef != "" {
		err = tx.QueryRow(`
			SELECT id,COALESCE(profile_id,'') FROM cache_tokens
			WHERE name=? AND status='active'`, collectorRef,
		).Scan(&collector.CacheTokenID, &profileID)
		if err == sql.ErrNoRows {
			return fmt.Errorf("客户端实例 %q 不是有效的活动设备码", collectorRef)
		}
		if err != nil {
			return err
		}
		if profileID == "" {
			return fmt.Errorf("客户端实例 %q 的设备码没有绑定 profile，请先在权威端绑定", collectorRef)
		}
	}
	servers, err := serversForDeviceTx(tx, device.ID)
	if err != nil {
		return err
	}
	sudoEnabled, err := softwareCollectSudoEnabledTx(tx)
	if err != nil {
		return err
	}
	chosen := (*models.Server)(nil)
	if serverRef != "" {
		chosen, err = chooseSoftwareCollectServer(servers, sudoEnabled, serverRef)
		if err != nil {
			return err
		}
	}
	if collector.IsClient() {
		var granted int
		if chosen == nil {
			err = tx.QueryRow(`
				SELECT COUNT(*)
				FROM profile_servers grant
				JOIN servers server ON server.id=grant.server_id
				WHERE grant.profile_id=? AND server.device_id=?`,
				profileID, device.ID,
			).Scan(&granted)
		} else {
			err = tx.QueryRow(`
				SELECT COUNT(*) FROM profile_servers WHERE profile_id=? AND server_id=?`,
				profileID, chosen.ID,
			).Scan(&granted)
		}
		if err != nil {
			return err
		}
		if granted == 0 {
			serverName := preferredServerName(chosen)
			if serverName == "" {
				return fmt.Errorf(
					"客户端实例 %q 绑定的 profile 不包含这台设备的任何服务器条目，不能把它登记为这台设备的采集执行者",
					collectorRef,
				)
			}
			return fmt.Errorf(
				"客户端实例 %q 绑定的 profile 不包含采集用服务器条目 %s，不能把它登记为这台设备的采集执行者",
				collectorRef, serverName,
			)
		}
	}
	at := s.nowTime()
	if _, err := tx.Exec(`
		INSERT INTO software_collection_status(device_id,collector,preferred_server_id)
		VALUES(?,?,?)
		ON CONFLICT(device_id) DO UPDATE SET
			collector=excluded.collector,
			preferred_server_id=excluded.preferred_server_id`,
		device.ID, collector.CacheTokenID, preferredServerID(chosen),
	); err != nil {
		return err
	}
	if err := writeSoftwareAuditTx(tx, at, "software-collect-collector", map[string]any{
		"device": device.Name, "collector": collectorRef,
		"collector_id": collector.CacheTokenID, "server": preferredServerName(chosen),
	}, source); err != nil {
		return err
	}
	return tx.Commit()
}

func preferredServerID(server *models.Server) string {
	if server == nil {
		return ""
	}
	return server.ID
}

func preferredServerName(server *models.Server) string {
	if server == nil {
		return ""
	}
	return server.Name
}

func serversForDeviceTx(tx *sql.Tx, deviceID string) ([]*models.Server, error) {
	rows, err := tx.Query(`
		SELECT id,name,host,port,user,COALESCE(device_id,''),auth_method,credential_id,sudo_credential_id,shell_type,tags,description,location,hardware,services,role,caveats,expose_host,created_at,updated_at,revision
		FROM servers WHERE device_id=?`, deviceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*models.Server, 0)
	for rows.Next() {
		server, err := scanServer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, server)
	}
	return out, rows.Err()
}

func softwareCollectSudoEnabledTx(tx *sql.Tx) (map[string]bool, error) {
	rows, err := tx.Query(`SELECT server_id,sudo_enabled FROM software_collection_settings`)
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

func chooseSoftwareCollectServer(
	servers []*models.Server,
	sudoEnabled map[string]bool,
	requested string,
) (*models.Server, error) {
	sorted := make([]*models.Server, len(servers))
	copy(sorted, servers)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	if strings.TrimSpace(requested) == "" {
		for _, server := range sorted {
			if sudoEnabled[server.ID] {
				return server, nil
			}
		}
		if len(sorted) == 0 {
			return nil, fmt.Errorf("设备没有服务器条目")
		}
		return sorted[0], nil
	}
	request := strings.TrimSpace(requested)
	for _, server := range sorted {
		if server.Name == request || server.ID == request {
			return server, nil
		}
	}
	return nil, fmt.Errorf("服务器条目 %q 不在这台设备上", request)
}

// SoftwareCollectionAssignments 列出一个客户端实例名下需要执行的采集分派。
// preferredServerID 为空属于旧数据，读取时按默认规则补齐为第一个条目。
func (s *Store) SoftwareCollectionAssignments(collector models.Collector, now time.Time) ([]models.SoftwareCollectionAssignment, error) {
	statuses, err := s.SoftwareCollectionStatusesForCollector(collector)
	if err != nil {
		return nil, err
	}
	out := make([]models.SoftwareCollectionAssignment, 0, len(statuses))
	var profileID string
	if err := s.db.QueryRow(
		`SELECT COALESCE(profile_id,'') FROM cache_tokens WHERE id=?`,
		collector.CacheTokenID,
	).Scan(&profileID); err != nil {
		return nil, err
	}
	granted := map[string]bool{}
	if profileID != "" {
		rows, err := s.db.Query(
			`SELECT server_id FROM profile_servers WHERE profile_id=?`, profileID,
		)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var serverID string
			if err := rows.Scan(&serverID); err != nil {
				rows.Close()
				return nil, err
			}
			granted[serverID] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	for _, status := range statuses {
		servers, err := s.FullServersForDevice(status.DeviceName)
		if err != nil {
			return nil, err
		}
		visibleServers := make([]*models.Server, 0, len(servers))
		for _, server := range servers {
			if granted[server.ID] {
				visibleServers = append(visibleServers, server)
			}
		}
		sudoEnabled, err := s.SoftwareCollectSudoEnabled()
		if err != nil {
			return nil, err
		}
		chosen, err := chooseSoftwareCollectServer(visibleServers, sudoEnabled, status.PreferredServerID)
		if err != nil {
			return nil, fmt.Errorf("设备 %s：%w", status.DeviceName, err)
		}
		out = append(out, models.SoftwareCollectionAssignment{
			DeviceID: status.DeviceID, DeviceName: status.DeviceName,
			ServerID: chosen.ID, ServerName: chosen.Name,
			Sudo: sudoEnabled[chosen.ID], PendingImmediate: status.PendingImmediate,
			LastSuccessAt: status.LastSuccessAt,
			Due: status.PendingImmediate || status.LastSuccessAt.IsZero() ||
				now.Sub(status.LastSuccessAt) >= 24*time.Hour,
		})
	}
	return out, nil
}

// RecordSoftwareCollectionFailureFromCollector 记录客户端实例上交的一次
// 采集失败。条目保留不动，失败时间与原因写进权威端采集状态，并写一行采
// 集审计；不改变这台设备登记的采集执行者。
func (s *Store) RecordSoftwareCollectionFailureFromCollector(
	deviceName string,
	collector models.Collector,
	reason, serverID string,
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
	if err := writeSoftwareCollectionStatusTx(
		tx, device.ID, serverID, collector, nil, softwareCollectionFailure{At: at, Reason: reason},
	); err != nil {
		return err
	}
	if err := writeSoftwareAuditTx(tx, at, "software-collect", map[string]any{
		"device": device.Name, "server": serverName, "collector": collector.CacheTokenID,
		"sudo": sudo, "result": "failed", "reason": reason,
	}, source); err != nil {
		return err
	}
	return tx.Commit()
}
