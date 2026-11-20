package store

import (
	"fmt"
	"strings"

	"ssh-manager-mcp/internal/models"
)

// EditDeviceSitesWithSource 在一次事务中把多台设备的地点改为同一个目
// 标。siteRef 用完整地点路径或全库唯一的裸名字指称目标地点，空字符串表
// 示清空地点。设备名按传入顺序去重；任一台写入失败时整批回滚，地点与
// 审计都不会出现部分提交。每台设备各写一条现有 device.edit 审计。
func (s *Store) EditDeviceSitesWithSource(
	devices []string, siteRef string, source WriteSource,
) ([]*models.Device, error) {
	if s.readOnly {
		return nil, ErrReadOnly
	}
	if len(devices) == 0 {
		return nil, fmt.Errorf("至少要选择一台设备才能批量设置地点")
	}
	seen := make(map[string]bool, len(devices))
	names := make([]string, 0, len(devices))
	for _, device := range devices {
		name := strings.TrimSpace(device)
		if name == "" {
			return nil, fmt.Errorf("设备名字不能为空，无法批量设置地点")
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	edited := make([]*models.Device, 0, len(names))
	for _, name := range names {
		device, err := s.editDeviceTx(tx, name, DeviceChanges{Site: &siteRef}, source)
		if err != nil {
			return nil, err
		}
		edited = append(edited, device)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return edited, nil
}
