package store

import (
	"database/sql"
	"strings"
)

// migrateServerLocationIntoDescription 取消服务器元数据的 location 列
// （票 #296）：非空文本先按「原位置：<文本>」追加到说明末尾，再删除该
// 列。列不存在即已完成，天然幂等；数据修正与删列在同一个事务里提交。
// 追加后的说明可能超过常规写入的 4096 字节上限，迁移以保留原文优先，
// 不做截断。
func migrateServerLocationIntoDescription(db *sql.DB) error {
	exists, err := tableExists(db, "servers")
	if err != nil || !exists {
		return err
	}
	present, err := columnPresent(db, "servers", "location")
	if err != nil || !present {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
		UPDATE servers
		SET description = CASE
			WHEN COALESCE(description,'') = '' THEN '原位置：' || TRIM(COALESCE(location,''))
			ELSE COALESCE(description,'') || char(10) || '原位置：' || TRIM(COALESCE(location,''))
		END
		WHERE TRIM(COALESCE(location,'')) <> ''`); err != nil {
		return err
	}
	// 触发器迁移已在 migrate() 里把修订号触发器换成不含 location 的最新
	// 定义，因此这里可以直接删列。
	if _, err := tx.Exec(`ALTER TABLE servers DROP COLUMN location`); err != nil {
		return err
	}
	return tx.Commit()
}

// mergeLegacyServerLocation 把旧快照携带的服务器位置并入说明，规则与
// 存储迁移一致；用于导入旧版本导出的快照，新导出不再携带该字段。
func mergeLegacyServerLocation(description, location string) string {
	location = strings.TrimSpace(location)
	if location == "" {
		return description
	}
	suffix := "原位置：" + location
	if description == "" {
		return suffix
	}
	if strings.HasSuffix(description, suffix) {
		return description
	}
	return description + "\n" + suffix
}
