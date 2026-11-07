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
// 快照里。布局坐标表是拓扑表的明确例外:拓扑图拖动会频繁改坐标,坐标变
// 化不产生变更提示,也不触发重新拉取,只随下一次因其他原因发生的全量拉
// 取更新。其余新配置表加入 schema 时,应同时补进这张清单。
var snapshotRevisionTables = []string{
	"credentials",
	"servers",
	"profiles",
	"profile_servers",
	"projects",
	"entry_host_keys",
	"sites",
	"networks",
	"devices",
	"network_memberships",
	"policy_rules",
	"policy_disabled_defaults",
	"secrets",
	"secret_server_allow",
}

// snapshotRevisionRetiredTriggers 列出旧版本为不再导出的表建立的修订号
// 触发器。打开旧库时删除它们，避免遗留触发器把无关写入误判成快照变化。
var snapshotRevisionRetiredTriggers = []string{
	"snapshot_revision_host_keys_insert",
	"snapshot_revision_host_keys_update",
	"snapshot_revision_host_keys_delete",
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
		events := []string{"insert", "update", "delete"}
		if table == "servers" {
			events = []string{"insert", "delete"}
		}
		for _, event := range events {
			fmt.Fprintf(&b, "CREATE TRIGGER IF NOT EXISTS snapshot_revision_%s_%s\nAFTER %s ON %s BEGIN\n  UPDATE snapshot_revision SET revision = revision + 1;\nEND;\n",
				table, event, strings.ToUpper(event), table)
		}
	}
	b.WriteString(snapshotRevisionServerUpdateTrigger())
	// 改绑设备码会改变这个设备码下一次拉到的 profile 快照,但普通设备码
	// 维护(记录最后拉取时间、客户端版本、吊销)不改变任何快照内容。因此
	// 这里只对 profile 绑定列建更新触发器,并且只在绑定真正变化时加一。
	b.WriteString(`
CREATE TRIGGER IF NOT EXISTS snapshot_revision_cache_tokens_profile
AFTER UPDATE OF profile_id ON cache_tokens
FOR EACH ROW WHEN OLD.profile_id IS NOT NEW.profile_id
BEGIN
  UPDATE snapshot_revision SET revision = revision + 1;
END;
`)
	return b.String()
}

// snapshotRevisionServerUpdateTrigger 生成服务器条目表的快照修订号更新触
// 发器。结构初始化与旧库迁移共用这一份结构化查询语言，保证新库和升级库
// 得到相同定义。
func snapshotRevisionServerUpdateTrigger() string {
	// 服务器条目的所属设备随客户端快照下发，变化必须让客户端重新拉取。
	return `
CREATE TRIGGER IF NOT EXISTS snapshot_revision_servers_update
AFTER UPDATE ON servers
FOR EACH ROW WHEN
  OLD.id IS NOT NEW.id OR OLD.name IS NOT NEW.name OR OLD.host IS NOT NEW.host OR OLD.port IS NOT NEW.port OR
  OLD.user IS NOT NEW.user OR OLD.auth_method IS NOT NEW.auth_method OR
  OLD.credential_id IS NOT NEW.credential_id OR OLD.sudo_credential_id IS NOT NEW.sudo_credential_id OR
  OLD.device_id IS NOT NEW.device_id OR
  OLD.shell_type IS NOT NEW.shell_type OR OLD.tags IS NOT NEW.tags OR
  OLD.description IS NOT NEW.description OR OLD.location IS NOT NEW.location OR
  OLD.hardware IS NOT NEW.hardware OR OLD.services IS NOT NEW.services OR
  OLD.role IS NOT NEW.role OR OLD.caveats IS NOT NEW.caveats OR
  OLD.expose_host IS NOT NEW.expose_host OR OLD.created_at IS NOT NEW.created_at OR
  OLD.updated_at IS NOT NEW.updated_at OR OLD.revision IS NOT NEW.revision
BEGIN
  UPDATE snapshot_revision SET revision = revision + 1;
END;
`
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

// setSnapshotRevisionTx 在导入快照的事务里把修订号推进到导入前当前值与
// 快照携带值中较大者的后一个值。修订号必须全库单调递增:如果恢复成快照
// 携带值,目标库此前更高的修订号会倒退,轮询方会把不同内容误判成未变化。
func setSnapshotRevisionTx(tx *sql.Tx, revision, currentBeforeImport int64) error {
	next := currentBeforeImport
	if revision > next {
		next = revision
	}
	next++
	_, err := tx.Exec(
		`INSERT INTO snapshot_revision(id, revision) VALUES (1, ?)
		 ON CONFLICT(id) DO UPDATE SET revision=excluded.revision`, next)
	return err
}
