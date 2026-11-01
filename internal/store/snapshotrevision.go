package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// snapshotRevisionTables 列出会影响导出快照内容的全部配置表。写入这些表
// 的每一条插入、更新、删除都让全库快照修订号加一;审计表、设备码表、
// 配对表、隧道表和设置表不在其中,因为它们不出现在任何 profile 的导出
// 快照里。新配置表加入 schema 时,应同时补进这张清单。
var snapshotRevisionTables = []string{
	"credentials",
	"servers",
	"profiles",
	"profile_servers",
	"projects",
	"host_keys",
	"policy_rules",
	"policy_disabled_defaults",
}

// snapshotRevisionSchema 生成快照修订号的表结构、初始行和触发器。触发器
// 保证修订号与配置写入在同一个数据库事务里提交:调用方写入配置行,触发
// 器在同一个事务内加一,事务回滚时一并回滚。初始行由 INSERT OR IGNORE
// 提供,因此旧库第一次打开时也会迁移到修订号 1,之后重复打开是无操作。
// 一次业务写入如果落了多张配置表的行(例如「带凭据新增服务器」),修订号
// 会加多于一;消费方只比较修订号是否相同,不依赖每次恰好加一。
func snapshotRevisionSchema() string {
	var b strings.Builder
	b.WriteString(`
CREATE TABLE IF NOT EXISTS snapshot_revision (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  revision INTEGER NOT NULL
);
INSERT OR IGNORE INTO snapshot_revision(id, revision) VALUES (1, 1);
`)
	for _, table := range snapshotRevisionTables {
		for _, event := range []string{"insert", "update", "delete"} {
			fmt.Fprintf(&b, "CREATE TRIGGER IF NOT EXISTS snapshot_revision_%s_%s\nAFTER %s ON %s BEGIN\n  UPDATE snapshot_revision SET revision = revision + 1;\nEND;\n",
				table, event, strings.ToUpper(event), table)
		}
	}
	return b.String()
}

// SnapshotRevision 返回全库统一、持久、单调递增的快照修订号。单机库与
// 权威端库共用这一套存储,客户端比较修订号即可判断快照是否需要重新
// 拉取。
func (s *Store) SnapshotRevision() (int64, error) {
	var rev int64
	err := s.db.QueryRow(`SELECT revision FROM snapshot_revision WHERE id=1`).Scan(&rev)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("snapshot revision row missing (schema initialization failed)")
	}
	return rev, err
}

// setSnapshotRevisionTx 在导入快照的事务里把修订号恢复为快照携带的值。
// 触发器在导入各配置表时已经加过一,这里显式覆盖成快照原值,保证恢复
// 是无损的:导入后的库继续从原修订号往前走。
func setSnapshotRevisionTx(tx *sql.Tx, revision int64) error {
	_, err := tx.Exec(
		`INSERT INTO snapshot_revision(id, revision) VALUES (1, ?)
		 ON CONFLICT(id) DO UPDATE SET revision=excluded.revision`, revision)
	return err
}
