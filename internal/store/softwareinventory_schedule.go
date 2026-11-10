package store

import (
	"time"

	"ssh-manager-mcp/internal/models"
)

// SoftwareCollectionStatusesForCollector 列出一台采集执行者名下的全部受管
// 设备的采集状态，按设备名排序。只返回挂了服务器条目的设备（采集必须经
// 过某个服务器条目的登录链路）；从未采集的设备返回零值状态。调度服务与
// 「采集分派给本实例的设备」子命令用它计算到期设备。
func (s *Store) SoftwareCollectionStatusesForCollector(collector models.Collector) ([]models.SoftwareCollectionStatus, error) {
	rows, err := s.db.Query(`
		SELECT devices.id,devices.name,COALESCE(status.collector,''),COALESCE(token.name,''),
		       EXISTS(SELECT 1 FROM cache_tokens revoked
		              WHERE revoked.id=status.collector AND revoked.status='revoked'),
		       COALESCE(status.preferred_server_id,''),COALESCE(status.last_server_id,''),
		       COALESCE(last_server.name,''),
		       COALESCE(status.last_success_at,0),COALESCE(status.last_failure_at,0),
		       COALESCE(status.last_failure_reason,''),COALESCE(status.last_incomplete,0),
		       COALESCE(status.command_set_version,''),COALESCE(status.system_type,''),
		       COALESCE(status.pending_immediate,0)
		FROM devices
		LEFT JOIN software_collection_status status ON status.device_id=devices.id
			LEFT JOIN cache_tokens token ON token.id=status.collector
		LEFT JOIN servers last_server ON last_server.id=status.last_server_id
			WHERE COALESCE(status.collector,'')=? AND EXISTS (
			SELECT 1 FROM servers WHERE servers.device_id=devices.id)
			ORDER BY devices.name,devices.id`, collector.CacheTokenID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]models.SoftwareCollectionStatus, 0)
	for rows.Next() {
		var status models.SoftwareCollectionStatus
		var successAt, failureAt int64
		if err := rows.Scan(
			&status.DeviceID, &status.DeviceName, &status.Collector.CacheTokenID,
			&status.CollectorName, &status.CollectorRevoked,
			&status.PreferredServerID, &status.LastServerID, &status.LastServerName,
			&successAt, &failureAt, &status.LastFailureReason, &status.LastIncomplete,
			&status.CommandSetVersion, &status.SystemType, &status.PendingImmediate,
		); err != nil {
			return nil, err
		}
		status.LastSuccessAt, status.LastFailureAt = time.Unix(successAt, 0), time.Unix(failureAt, 0)
		out = append(out, status)
	}
	return out, rows.Err()
}

// RequestSoftwareImmediateCollection 给一台设备登记待执行的「立即采集」请
// 求：采集执行者在本实例上时命令会同步执行；由客户端实例采集的设备等它
// 下一轮分派时优先执行。请求与审计在同一个事务里提交。
func (s *Store) RequestSoftwareImmediateCollection(deviceName string, source WriteSource) error {
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
	if _, err := tx.Exec(`
		INSERT INTO software_collection_status(device_id,collector,pending_immediate)
		VALUES(?,'',1)
		ON CONFLICT(device_id) DO UPDATE SET pending_immediate=1`, device.ID); err != nil {
		return err
	}
	if err := writeSoftwareAuditTx(tx, s.nowTime(), "software-collect-request", map[string]any{
		"device": device.Name,
	}, source); err != nil {
		return err
	}
	return tx.Commit()
}
