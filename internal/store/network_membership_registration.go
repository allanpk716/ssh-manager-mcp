package store

import (
	"database/sql"
	"fmt"
)

// migrateNetworkMembershipRegistrationOrder 给旧库补网络成员登记序号，并按
// 创建时间、行标识符的顺序为存量成员赋值。赋值只是结构升级，不代表拓扑
// 内容变化；临时移除更新触发器，避免它把快照修订号推进。
func migrateNetworkMembershipRegistrationOrder(db *sql.DB) error {
	exists, err := tableExists(db, "network_memberships")
	if err != nil || !exists {
		return err
	}
	if err := addColumnIfMissing(
		db, "network_memberships", "registration_order", "INTEGER NOT NULL DEFAULT 0",
	); err != nil {
		return err
	}
	var missing bool
	if err := db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM network_memberships WHERE registration_order<=0)`,
	).Scan(&missing); err != nil {
		return err
	}
	if !missing {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DROP TRIGGER IF EXISTS snapshot_revision_network_memberships_update`); err != nil {
		return fmt.Errorf("remove network membership revision trigger for migration: %w", err)
	}
	if _, err := tx.Exec(`
		WITH missing AS (
			SELECT rowid AS missing_rowid,
			       ROW_NUMBER() OVER (ORDER BY created_at,rowid) +
			       (SELECT COALESCE(MAX(registration_order),0) FROM network_memberships) AS next_order
			FROM network_memberships
			WHERE registration_order<=0
		)
		UPDATE network_memberships
		SET registration_order=(
			SELECT next_order FROM missing WHERE missing.missing_rowid=network_memberships.rowid
		)
		WHERE registration_order<=0`); err != nil {
		return fmt.Errorf("assign legacy network membership registration order: %w", err)
	}
	return tx.Commit()
}
