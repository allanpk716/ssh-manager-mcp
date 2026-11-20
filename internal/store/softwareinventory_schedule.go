package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	"ssh-manager-mcp/internal/models"
)

const softwareCollectionDailyTimeSettingKey = "softwareinventory.collection.daily_time"

// SoftwareCollectionDailyTime 读取全局每天采集时间。设置缺失时返回默认
// 01:00；已保存但格式非法的值会让调度与客户端分派同时失败，避免悄悄换
// 回默认时间。
func (s *Store) SoftwareCollectionDailyTime() (string, error) {
	value, stored, err := s.GetSetting(softwareCollectionDailyTimeSettingKey)
	if err != nil {
		return "", err
	}
	if !stored {
		return models.DefaultSoftwareCollectionDailyTime, nil
	}
	normalized := strings.TrimSpace(value)
	if _, _, err := models.ParseSoftwareCollectionDailyTime(normalized); err != nil {
		return "", err
	}
	return normalized, nil
}

// SetSoftwareCollectionDailyTime 校验并持久保存全局每天采集时间，同时写
// 一条软件清单审计。空值不允许恢复默认；后续如需恢复默认，应提供明确的
// 删除入口。
func (s *Store) SetSoftwareCollectionDailyTime(value string, source WriteSource) error {
	if s.readOnly {
		return ErrReadOnly
	}
	normalized := strings.TrimSpace(value)
	if _, _, err := models.ParseSoftwareCollectionDailyTime(normalized); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var oldValue string
	switch err := tx.QueryRow(
		`SELECT value FROM settings WHERE key=?`, softwareCollectionDailyTimeSettingKey,
	).Scan(&oldValue); {
	case err == nil:
	case errors.Is(err, sql.ErrNoRows):
		oldValue = models.DefaultSoftwareCollectionDailyTime
	default:
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO settings(key,value,updated_at) VALUES(?,?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`,
		softwareCollectionDailyTimeSettingKey, normalized, s.nowTime().Unix(),
	); err != nil {
		return err
	}
	if err := writeSoftwareAuditTx(tx, s.nowTime(), "software-collect-time", map[string]any{
		"old": strings.TrimSpace(oldValue), "new": normalized,
	}, source); err != nil {
		return err
	}
	return tx.Commit()
}

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
