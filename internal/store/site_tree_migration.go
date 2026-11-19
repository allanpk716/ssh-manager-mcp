package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// sitesParentNameIndex 是「同一上级下地点名字唯一」的数据库唯一索引。
// 旧结构的 sites.name_lower 是全表唯一；迁移重建表后撤销它，改用这个
// 以 COALESCE(parent_id,”) 为键的表达式索引，让根地点与每个上级地点
// 下的空上级归到同一个比较键。
const sitesParentNameIndex = "sites_parent_name_lower"

// migrateSiteTreeAndDeviceSites 是票 #295 的独立迁移步骤：地点表加可空
// 上级并把全表唯一的名字约束放宽为同一上级下唯一；机器表加可空地点，
// 并把旧数据里机器的地点回填为它第一个物理局域网覆盖的地点。
//
// 「第一个」按网络名、成员地址排序取值，与设备列表的成员展示顺序一
// 致；有多个物理局域网的机器在迁移审计里记录全部候选网络。整个迁移
// 在一个事务里提交，重复打开时不改库。
func migrateSiteTreeAndDeviceSites(db *sql.DB) error {
	sitesExists, err := tableExists(db, "sites")
	if err != nil || !sitesExists {
		return err
	}
	devicesExists, err := tableExists(db, "devices")
	if err != nil {
		return err
	}
	hasParentColumn, err := columnPresent(db, "sites", "parent_id")
	if err != nil {
		return err
	}
	hasDeviceSiteColumn := false
	if devicesExists {
		if hasDeviceSiteColumn, err = columnPresent(db, "devices", "site_id"); err != nil {
			return err
		}
	}
	indexPresent, err := indexExists(db, sitesParentNameIndex)
	if err != nil {
		return err
	}
	if hasParentColumn && hasDeviceSiteColumn && indexPresent {
		return nil
	}

	// 重建 sites 需要 DROP TABLE；与其它受守卫表重建一样，必须在固定
	// 的单个连接上先关闭外部键，再在同一事务里完成全部结构变化与数据
	// 回填。
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	defer func() { _, _ = conn.ExecContext(ctx, `PRAGMA foreign_keys=ON`) }()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if !hasParentColumn {
		statements := []string{
			`CREATE TABLE sites_new (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  name_lower TEXT NOT NULL,
  parent_id TEXT,
  note TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
)`,
			`INSERT INTO sites_new(id,name,name_lower,parent_id,note,created_at,updated_at)
SELECT id,name,name_lower,NULL,note,created_at,updated_at FROM sites`,
			`DROP TABLE sites`,
			`ALTER TABLE sites_new RENAME TO sites`,
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("migrate site tree: %w", err)
			}
		}
		// DROP TABLE sites 会带走挂在旧表上的快照修订号触发器；在同一
		// 个事务里重放触发器结构，避免重建后的第一次写入漏推进修订号。
		// 它是幂化的，未受影响的触发器保持原样。
		if _, err := tx.ExecContext(ctx, snapshotRevisionSchema()); err != nil {
			return fmt.Errorf("restore snapshot revision triggers after site tree migration: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(
		`CREATE UNIQUE INDEX IF NOT EXISTS %s ON sites(COALESCE(parent_id,''),name_lower)`,
		sitesParentNameIndex,
	)); err != nil {
		return fmt.Errorf("create site parent-name index: %w", err)
	}

	if devicesExists && !hasDeviceSiteColumn {
		if _, err := tx.ExecContext(ctx,
			`ALTER TABLE devices ADD COLUMN site_id TEXT REFERENCES sites(id)`); err != nil {
			return fmt.Errorf("add device site column: %w", err)
		}
		if err := backfillDeviceSitesTx(tx, time.Now()); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

// backfillDeviceSitesTx 给还没有地点的机器回填它第一个物理局域网覆盖的
// 地点，并写迁移审计。
func backfillDeviceSitesTx(tx *sql.Tx, at time.Time) error {
	if _, err := tx.Exec(`
		UPDATE devices SET site_id = (
			SELECT network.site_id
			FROM network_memberships member
			JOIN networks network ON network.id=member.network_id
			WHERE member.device_id=devices.id
			  AND network.type='physical'
			  AND network.site_id IS NOT NULL
			ORDER BY network.name,member.address,network.id
			LIMIT 1)
		WHERE site_id IS NULL`); err != nil {
		return fmt.Errorf("backfill device sites: %w", err)
	}

	rows, err := tx.Query(`
		SELECT device.name,site.name,
		       (SELECT COUNT(*)
		        FROM network_memberships member
		        JOIN networks network ON network.id=member.network_id
		        WHERE member.device_id=device.id AND network.type='physical')
		FROM devices device
		JOIN sites site ON site.id=device.site_id
		ORDER BY device.name,device.id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	devices := make([]map[string]any, 0)
	for rows.Next() {
		var deviceName, siteName string
		var physicalCount int
		if err := rows.Scan(&deviceName, &siteName, &physicalCount); err != nil {
			return err
		}
		entry := map[string]any{
			"name": deviceName,
			"site": siteName,
		}
		if physicalCount > 1 {
			networkNames, err := physicalNetworkNamesForDeviceTx(tx, deviceName)
			if err != nil {
				return err
			}
			entry["physical-networks"] = networkNames
		}
		devices = append(devices, entry)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(devices) == 0 {
		return nil
	}
	return writeTopologyAuditTx(tx, at, "device.site-migrate", map[string]any{
		"devices": devices,
		"reason":  "机器地点从第一个物理局域网覆盖的地点回填",
	})
}

// physicalNetworkNamesForDeviceTx 返回一台机器的全部物理局域网名字，迁
// 移审计用它记录「取第一个」时的候选网络。
func physicalNetworkNamesForDeviceTx(tx *sql.Tx, deviceName string) ([]string, error) {
	rows, err := tx.Query(`
		SELECT network.name
		FROM network_memberships member
		JOIN networks network ON network.id=member.network_id
		JOIN devices device ON device.id=member.device_id
		WHERE device.name_lower=? AND network.type='physical'
		ORDER BY network.name,member.address,network.id`,
		deviceName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := make([]string, 0)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// indexExists 报告数据库里是否存在指定名字的索引。
func indexExists(db *sql.DB, name string) (bool, error) {
	var exists int
	err := db.QueryRow(`SELECT 1 FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
