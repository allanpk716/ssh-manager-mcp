package store

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"ssh-manager-mcp/internal/models"
)

// SetSoftwareCollectionCollector 登记一台设备由哪个客户端实例采集，并可选
// 指定采集用服务器条目。collectorRef 是设备码的名字；serverRef 可用服务
// 器条目名或标识符，为空时沿用采集服务的默认选择规则。登记前校验设备码
// 有效，且采集用服务器条目既在这台设备上，也在该设备码绑定 profile 的授
// 权范围内；登记与审计在同一个事务里提交。
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
	var tokenID, profileID string
	err = tx.QueryRow(`
		SELECT id,COALESCE(profile_id,'') FROM cache_tokens
		WHERE name=? AND status='active'`, collectorRef,
	).Scan(&tokenID, &profileID)
	if err == sql.ErrNoRows {
		return fmt.Errorf("客户端实例 %q 不是有效的活动设备码", collectorRef)
	}
	if err != nil {
		return err
	}
	if profileID == "" {
		return fmt.Errorf("客户端实例 %q 的设备码没有绑定 profile，请先在权威端绑定", collectorRef)
	}
	servers, err := serversForDeviceTx(tx, device.ID)
	if err != nil {
		return err
	}
	if len(servers) == 0 {
		return fmt.Errorf("设备 %s 没有服务器条目，无法登记采集执行者", device.Name)
	}
	sudoEnabled, err := softwareCollectSudoEnabledTx(tx)
	if err != nil {
		return err
	}
	chosen, err := chooseSoftwareCollectServer(servers, sudoEnabled, serverRef)
	if err != nil {
		return err
	}
	var granted int
	if err := tx.QueryRow(`
		SELECT COUNT(*) FROM profile_servers WHERE profile_id=? AND server_id=?`,
		profileID, chosen.ID,
	).Scan(&granted); err != nil {
		return err
	}
	if granted == 0 {
		return fmt.Errorf(
			"客户端实例 %q 绑定的 profile 不包含采集用服务器条目 %s，不能把它登记为这台设备的采集执行者",
			collectorRef, chosen.Name,
		)
	}
	at := s.nowTime()
	if _, err := tx.Exec(`
		INSERT INTO software_collection_status(device_id,collector,preferred_server_id)
		VALUES(?,?,?)
		ON CONFLICT(device_id) DO UPDATE SET
			collector=excluded.collector,
			preferred_server_id=excluded.preferred_server_id`,
		device.ID, collectorRef, chosen.ID,
	); err != nil {
		return err
	}
	if err := writeSoftwareAuditTx(tx, at, "software-collect-collector", map[string]any{
		"device": device.Name, "collector": collectorRef, "server": chosen.Name,
	}, source); err != nil {
		return err
	}
	return tx.Commit()
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
func (s *Store) SoftwareCollectionAssignments(collector string, now time.Time) ([]models.SoftwareCollectionAssignment, error) {
	statuses, err := s.SoftwareCollectionStatusesForCollector(collector)
	if err != nil {
		return nil, err
	}
	out := make([]models.SoftwareCollectionAssignment, 0, len(statuses))
	for _, status := range statuses {
		servers, err := s.FullServersForDevice(status.DeviceName)
		if err != nil {
			return nil, err
		}
		sudoEnabled, err := s.SoftwareCollectSudoEnabled()
		if err != nil {
			return nil, err
		}
		chosen, err := chooseSoftwareCollectServer(servers, sudoEnabled, status.PreferredServerID)
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
