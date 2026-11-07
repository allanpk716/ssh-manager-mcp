package store

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/policy"
)

func TestSnapshotRevisionServerUpdateTriggerCoversAllExportedColumns(t *testing.T) {
	st := newTestStore(t)
	columns, err := st.db.Query(`PRAGMA table_info(servers)`)
	if err != nil {
		t.Fatal(err)
	}
	defer columns.Close()
	var checked []string
	for columns.Next() {
		var columnIndex int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue sql.NullString
		if err := columns.Scan(&columnIndex, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		checked = append(checked, name)
	}
	if err := columns.Err(); err != nil {
		t.Fatal(err)
	}
	if len(checked) == 0 {
		t.Fatal("servers table has no columns to check")
	}

	var triggerSQL string
	if err := st.db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='trigger' AND name=?`,
		"snapshot_revision_servers_update",
	).Scan(&triggerSQL); err != nil {
		t.Fatal(err)
	}
	for _, column := range checked {
		comparison := regexp.MustCompile(
			`OLD\.` + regexp.QuoteMeta(column) + `\s+IS NOT NEW\.` + regexp.QuoteMeta(column),
		)
		if !comparison.MatchString(triggerSQL) {
			t.Fatalf("snapshot revision update trigger must compare exported servers column %s", column)
		}
	}
}

func triggerExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var exists bool
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='trigger' AND name=?)`, name).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	return exists
}

func triggerDefinition(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var definition string
	err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='trigger' AND name=?`,
		name,
	).Scan(&definition)
	if err != nil {
		t.Fatal(err)
	}
	return definition
}

// TestSnapshotRevisionServerDeviceTriggerUpgradeHasNoGap 构造升级前形态的
// vault：服务器条目表还没有所属设备引用列，修订号更新触发器还是「任何
// 更新都加一」的旧定义。升级后的第一个 Store 实例必须立即得到新触发器，
// 不能等进程重启后的下一次打开。
func TestSnapshotRevisionServerDeviceTriggerUpgradeHasNoGap(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "store.db")
	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	oldShape := `
CREATE TABLE servers (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  host TEXT NOT NULL,
  port INTEGER NOT NULL,
  user TEXT NOT NULL,
  auth_method TEXT NOT NULL,
  credential_id TEXT,
  sudo_credential_id TEXT,
  shell_type TEXT NOT NULL DEFAULT 'posix',
  tags TEXT,
  description TEXT NOT NULL DEFAULT '',
  location TEXT NOT NULL DEFAULT '',
  hardware TEXT NOT NULL DEFAULT '',
  services TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL DEFAULT '',
  caveats TEXT NOT NULL DEFAULT '',
  expose_host INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  revision INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE snapshot_revision (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  revision INTEGER NOT NULL
);
INSERT INTO snapshot_revision(id, revision) VALUES (1, 41);
INSERT INTO servers(
  id,name,host,port,user,auth_method,tags,created_at,updated_at
) VALUES (
  'srv-old','old','192.0.2.10',22,'u','password','',1700000000,1700000000
);
CREATE TRIGGER snapshot_revision_servers_update
AFTER UPDATE ON servers BEGIN
  UPDATE snapshot_revision SET revision = revision + 1;
END;
`
	if _, err := legacy.Exec(oldShape); err != nil {
		legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	mk := make([]byte, 32)
	randRead(t, mk)
	migrated, err := Open(dbPath, mk)
	if err != nil {
		t.Fatalf("升级打开旧库失败:%v", err)
	}
	migrationBaseline, err := migrated.SnapshotRevision()
	if err != nil {
		migrated.Close()
		t.Fatal(err)
	}
	srv, err := migrated.GetServer("srv-old")
	if err != nil {
		migrated.Close()
		t.Fatal(err)
	}
	srv.Host = "198.51.100.20"
	if err := migrated.UpdateServer(srv); err != nil {
		migrated.Close()
		t.Fatal(err)
	}
	afterConnectionChange, err := migrated.SnapshotRevision()
	if err != nil {
		migrated.Close()
		t.Fatal(err)
	}
	if afterConnectionChange != migrationBaseline+1 {
		migrated.Close()
		t.Fatalf(
			"升级后第一次连接字段更新的快照修订号 = %d,应为 %d",
			afterConnectionChange, migrationBaseline+1,
		)
	}
	if _, err := migrated.db.Exec(
		`UPDATE servers SET device_id=NULL WHERE id='srv-old'`,
	); err != nil {
		migrated.Close()
		t.Fatal(err)
	}
	afterDeviceReferenceChange, err := migrated.SnapshotRevision()
	if err != nil {
		migrated.Close()
		t.Fatal(err)
	}
	if afterDeviceReferenceChange != afterConnectionChange+1 {
		migrated.Close()
		t.Fatalf(
			"只改所属设备引用后的快照修订号 = %d,应为 %d",
			afterDeviceReferenceChange, afterConnectionChange+1,
		)
	}
	triggerBeforeReopen := triggerDefinition(
		t, migrated.db, "snapshot_revision_servers_update",
	)

	// SQLite 的 data_version 是数据库改动计数。这里用另一个连接观察，如果
	// 重复打开还要改库，这个计数会增加。
	observer, err := sql.Open("sqlite", dbPath)
	if err != nil {
		migrated.Close()
		t.Fatal(err)
	}
	var dataVersionBeforeReopen int
	if err := observer.QueryRow(`PRAGMA data_version`).Scan(&dataVersionBeforeReopen); err != nil {
		migrated.Close()
		observer.Close()
		t.Fatal(err)
	}
	if err := migrated.Close(); err != nil {
		observer.Close()
		t.Fatal(err)
	}
	reopened, err := Open(dbPath, mk)
	if err != nil {
		observer.Close()
		t.Fatalf("重复打开升级后的库失败:%v", err)
	}
	var dataVersionAfterReopen int
	if err := observer.QueryRow(`PRAGMA data_version`).Scan(&dataVersionAfterReopen); err != nil {
		reopened.Close()
		observer.Close()
		t.Fatal(err)
	}
	if dataVersionAfterReopen != dataVersionBeforeReopen {
		reopened.Close()
		observer.Close()
		t.Fatalf(
			"重复打开的数据改动计数从 %d 变成 %d,应保持不变",
			dataVersionBeforeReopen, dataVersionAfterReopen,
		)
	}
	if definition := triggerDefinition(
		t, reopened.db, "snapshot_revision_servers_update",
	); definition != triggerBeforeReopen {
		reopened.Close()
		observer.Close()
		t.Fatalf(
			"重复打开后修订号更新触发器定义改变:\n第一次打开:\n%s\n重复打开:\n%s",
			triggerBeforeReopen, definition,
		)
	}
	if err := observer.Close(); err != nil {
		reopened.Close()
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestSnapshotRevisionServerDeviceTriggerReplacesConditionalLegacy 构造票
// #130 之前的升级形态：所属设备列已经存在，但条件触发器刻意排除它。升级
// 打开必须在同一事务里替换触发器，让第一个所属设备变更立即推进修订号；
// 新库与升级库的触发器定义也必须完全相同。
func TestSnapshotRevisionServerDeviceTriggerReplacesConditionalLegacy(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "store.db")
	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	oldShape := `
CREATE TABLE servers (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  host TEXT NOT NULL,
  port INTEGER NOT NULL,
  user TEXT NOT NULL,
  auth_method TEXT NOT NULL,
  credential_id TEXT,
  sudo_credential_id TEXT,
  shell_type TEXT NOT NULL DEFAULT 'posix',
  tags TEXT,
  description TEXT NOT NULL DEFAULT '',
  location TEXT NOT NULL DEFAULT '',
  hardware TEXT NOT NULL DEFAULT '',
  services TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL DEFAULT '',
  caveats TEXT NOT NULL DEFAULT '',
  expose_host INTEGER NOT NULL DEFAULT 0,
  device_id TEXT,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  revision INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE snapshot_revision (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  revision INTEGER NOT NULL
);
INSERT INTO snapshot_revision(id, revision) VALUES (1, 41);
INSERT INTO servers(
  id,name,host,port,user,auth_method,tags,device_id,created_at,updated_at
) VALUES (
  'srv-old','old','192.0.2.10',22,'u','password','','device-old',1700000000,1700000000
);
CREATE TRIGGER snapshot_revision_servers_update
AFTER UPDATE ON servers
FOR EACH ROW WHEN
  OLD.id IS NOT NEW.id OR OLD.name IS NOT NEW.name OR OLD.host IS NOT NEW.host OR OLD.port IS NOT NEW.port OR
  OLD.user IS NOT NEW.user OR OLD.auth_method IS NOT NEW.auth_method OR
  OLD.credential_id IS NOT NEW.credential_id OR OLD.sudo_credential_id IS NOT NEW.sudo_credential_id OR
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
	if _, err := legacy.Exec(oldShape); err != nil {
		legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	mk := make([]byte, 32)
	randRead(t, mk)
	migrated, err := Open(dbPath, mk)
	if err != nil {
		t.Fatalf("升级打开旧库失败:%v", err)
	}
	if _, err := migrated.db.Exec(
		`UPDATE servers SET device_id='device-new' WHERE id='srv-old'`,
	); err != nil {
		migrated.Close()
		t.Fatal(err)
	}
	afterDeviceReferenceChange, err := migrated.SnapshotRevision()
	if err != nil {
		migrated.Close()
		t.Fatal(err)
	}
	if afterDeviceReferenceChange != 42 {
		migrated.Close()
		t.Fatalf("升级后第一次所属设备变更的快照修订号 = %d,应为 42", afterDeviceReferenceChange)
	}

	fresh := newTestStore(t)
	wantDefinition := triggerDefinition(t, fresh.db, "snapshot_revision_servers_update")
	if definition := triggerDefinition(
		t, migrated.db, "snapshot_revision_servers_update",
	); definition != wantDefinition {
		migrated.Close()
		t.Fatalf(
			"升级库的修订号更新触发器定义:\n%s\n应与新库一致:\n%s",
			definition, wantDefinition,
		)
	}
	if err := migrated.Close(); err != nil {
		t.Fatal(err)
	}

	observer, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	var dataVersionBeforeReopen int
	if err := observer.QueryRow(`PRAGMA data_version`).Scan(&dataVersionBeforeReopen); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dbPath, mk)
	if err != nil {
		t.Fatalf("重复打开升级后的库失败:%v", err)
	}
	defer reopened.Close()
	var dataVersionAfterReopen int
	if err := observer.QueryRow(`PRAGMA data_version`).Scan(&dataVersionAfterReopen); err != nil {
		t.Fatal(err)
	}
	if dataVersionAfterReopen != dataVersionBeforeReopen {
		t.Fatalf(
			"重复打开的数据改动计数从 %d 变成 %d,应保持不变",
			dataVersionBeforeReopen, dataVersionAfterReopen,
		)
	}
}

// snapshotSeed 是快照修订号测试的基础库:一台已授权的服务器、一个
// profile、一个项目。各操作表条目可以用 setup 先补充自己的前置状态,
// 让 op 只包含被测的那一次写入。
type snapshotSeed struct {
	serverID        string
	profileID       string
	rebindProfileID string
	projectID       string
}

func seedSnapshotStore(t *testing.T) (*Store, *snapshotSeed) {
	t.Helper()
	s := newTestStore(t)
	cid, err := s.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	if err != nil {
		t.Fatal(err)
	}
	serverID, err := s.AddServer(&models.Server{
		Name: "gpu", Host: "192.0.2.10", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid,
	})
	if err != nil {
		t.Fatal(err)
	}
	profileID, err := s.AddProfile("team-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.GrantServers(profileID, []string{serverID}); err != nil {
		t.Fatal(err)
	}
	projectID, _, err := s.AddProject("proj-x", profileID)
	if err != nil {
		t.Fatal(err)
	}
	return s, &snapshotSeed{serverID: serverID, profileID: profileID, projectID: projectID}
}

// snapshotContent 返回当前全库导出内容(把修订号本身清零,内容比较只看
// 配置数据)以及存储层修订号。
func snapshotContent(t *testing.T, s *Store) (int64, string) {
	t.Helper()
	snap, err := s.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	rev := snap.SnapshotRevision
	snap.SnapshotRevision = 0
	blob, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	return rev, string(blob)
}

// TestSnapshotRevision_MigrationStartsAtOne: 新库的修订号从 1 开始,并且跨
// 开关持久。
func TestSnapshotRevision_MigrationStartsAtOne(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "store.db")
	mk := make([]byte, 32)
	randRead(t, mk)
	s, err := Open(dbPath, mk)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dbPath, mk)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	rev, err := s2.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if rev != 1 {
		t.Fatalf("新库快照修订号 = %d,应为 1", rev)
	}
}

// TestSnapshotRevision_LegacyDatabaseMigratesToOne: 旧结构库没有修订号表,
// 第一次打开时迁移为 1,之后的配置写入照常递增。
func TestSnapshotRevision_LegacyDatabaseMigratesToOne(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "store.db")
	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(oldShapeServersProjects); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	mk := make([]byte, 32)
	randRead(t, mk)
	migrated, err := Open(dbPath, mk)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	rev, err := migrated.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if rev != 1 {
		t.Fatalf("迁移后的快照修订号 = %d,应为 1", rev)
	}
	if _, err := migrated.AddServer(&models.Server{Name: "gpu", Host: "192.0.2.10", Port: 22, User: "u", AuthMethod: models.AuthPassword}); err != nil {
		t.Fatal(err)
	}
	after, err := migrated.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if after <= rev {
		t.Fatalf("迁移后写入的快照修订号 = %d,应大于 %d", after, rev)
	}
}

// TestSnapshotRevision_OldHostKeyTriggerUpgrade: 旧库已经带着指向
// host_keys 的修订号触发器。升级打开时必须迁移锚、删除过期触发器，并让
// 按条目锚的写入继续推进修订号；重复打开也不能因触发器迁移失败。
func TestSnapshotRevision_OldHostKeyTriggerUpgrade(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "store.db")
	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	oldShape := `
CREATE TABLE servers (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  host TEXT NOT NULL,
  port INTEGER NOT NULL,
  user TEXT NOT NULL,
  auth_method TEXT NOT NULL,
  credential_id TEXT,
  sudo_credential_id TEXT,
  tags TEXT,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE host_keys (
  host_port TEXT PRIMARY KEY,
  key_blob BLOB NOT NULL,
  created_at INTEGER NOT NULL,
  pin_format TEXT NOT NULL,
  pin_source TEXT NOT NULL,
  pin_device TEXT NOT NULL
);
CREATE TABLE snapshot_revision (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  revision INTEGER NOT NULL
);
INSERT INTO snapshot_revision(id, revision) VALUES (1, 41);
INSERT INTO servers(id,name,host,port,user,auth_method,tags,created_at,updated_at)
VALUES ('srv-old','old','192.0.2.10',22,'u','password','',1700000000,1700000000);
INSERT INTO host_keys(host_port,key_blob,created_at,pin_format,pin_source,pin_device)
VALUES ('192.0.2.10:22',X'0102',1700000000,'blob','tofu','');
`
	for _, event := range []string{"insert", "update", "delete"} {
		oldShape += "CREATE TRIGGER snapshot_revision_host_keys_" + event +
			" AFTER " + strings.ToUpper(event) + " ON host_keys BEGIN" +
			" UPDATE snapshot_revision SET revision = revision + 1; END;\n"
	}
	if _, err := legacy.Exec(oldShape); err != nil {
		legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	mk := make([]byte, 32)
	randRead(t, mk)
	s, err := Open(dbPath, mk)
	if err != nil {
		t.Fatalf("升级打开旧库失败:%v", err)
	}
	rev, err := s.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if rev <= 41 {
		t.Fatalf("锚迁移后的快照修订号 = %d,应大于 41", rev)
	}
	for _, trigger := range snapshotRevisionRetiredTriggers {
		if triggerExists(t, s.db, trigger) {
			t.Fatalf("升级后仍存在过期触发器 %s", trigger)
		}
	}
	srv, err := s.GetServer("srv-old")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClearEntryPin("srv-old", func(old PinMeta) AuditRow {
		return AuditRow{Action: "pin-delete"}
	}); err != nil {
		t.Fatal(err)
	}
	rev, err = s.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveEntryPin(srv, []byte("new-anchor")); err != nil {
		t.Fatal(err)
	}
	afterWrite, err := s.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if afterWrite != rev+1 {
		t.Fatalf("写入按条目锚后的快照修订号 = %d,应为 %d", afterWrite, rev+1)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dbPath, mk)
	if err != nil {
		t.Fatalf("重复打开升级后的库失败:%v", err)
	}
	defer reopened.Close()
	reopenedRev, err := reopened.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if reopenedRev != afterWrite {
		t.Fatalf("重复打开后的快照修订号 = %d,应保持 %d", reopenedRev, afterWrite)
	}
}

// TestSnapshotRevision_HostOnlyHostKeysUpgrade: 更早的旧结构按 host 列存放
// 锚，打开时整张 host_keys 表会被删除重建。即使库里已有旧修订号触发器，
// 升级、重复打开和后续按条目写锚也都必须成功。
func TestSnapshotRevision_HostOnlyHostKeysUpgrade(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "store.db")
	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	oldShape := `
CREATE TABLE servers (
  id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, host TEXT NOT NULL,
  port INTEGER NOT NULL, user TEXT NOT NULL, auth_method TEXT NOT NULL,
  credential_id TEXT, sudo_credential_id TEXT, tags TEXT,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE TABLE host_keys (
  host TEXT PRIMARY KEY, key_blob BLOB NOT NULL, created_at INTEGER NOT NULL
);
CREATE TABLE snapshot_revision (
  id INTEGER PRIMARY KEY CHECK (id = 1), revision INTEGER NOT NULL
);
INSERT INTO snapshot_revision(id, revision) VALUES (1, 7);
INSERT INTO servers(id,name,host,port,user,auth_method,tags,created_at,updated_at)
VALUES ('srv-old','old','192.0.2.10',22,'u','password','',1700000000,1700000000);
CREATE TRIGGER snapshot_revision_host_keys_insert
AFTER INSERT ON host_keys BEGIN
  UPDATE snapshot_revision SET revision = revision + 1;
END;
`
	if _, err := legacy.Exec(oldShape); err != nil {
		legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	mk := make([]byte, 32)
	randRead(t, mk)
	s, err := Open(dbPath, mk)
	if err != nil {
		t.Fatalf("升级打开按主机列存锚的旧库失败:%v", err)
	}
	rev, err := s.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if rev <= 7 {
		t.Fatalf("删除旧锚表并生成设备后的快照修订号 = %d,应大于 7", rev)
	}
	for _, trigger := range snapshotRevisionRetiredTriggers {
		if triggerExists(t, s.db, trigger) {
			t.Fatalf("删除旧锚表后仍存在过期触发器 %s", trigger)
		}
	}
	srv, err := s.GetServer("srv-old")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClearEntryPin("srv-old", func(old PinMeta) AuditRow {
		return AuditRow{Action: "pin-delete"}
	}); err != nil {
		t.Fatal(err)
	}
	rev, err = s.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveEntryPin(srv, []byte("new-anchor")); err != nil {
		t.Fatal(err)
	}
	afterWrite, err := s.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if afterWrite != rev+1 {
		t.Fatalf("写入按条目锚后的快照修订号 = %d,应为 %d", afterWrite, rev+1)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dbPath, mk)
	if err != nil {
		t.Fatalf("重复打开升级后的库失败:%v", err)
	}
	defer reopened.Close()
	reopenedRev, err := reopened.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if reopenedRev != afterWrite {
		t.Fatalf("重复打开后的快照修订号 = %d,应保持 %d", reopenedRev, afterWrite)
	}
}

// TestSnapshotRevision_AdvancesOnEverySnapshotWrite: 存储层性质测试——只要
// 导出快照内容变了,修订号就增加。新写入路径出现时,在这张表里加一行。
func TestSnapshotRevision_AdvancesOnEverySnapshotWrite(t *testing.T) {
	defaultRuleID := policy.DefaultRuleIDs()[0]
	newServer := func(name, host string) *models.Server {
		return &models.Server{Name: name, Host: host, Port: 22, User: "u", AuthMethod: models.AuthPassword}
	}
	cases := []struct {
		name string
		// sameWholeSnapshot marks operations that change which profile a device
		// code can see, rather than the whole-vault export itself.
		sameWholeSnapshot bool
		setup             func(t *testing.T, s *Store, seed *snapshotSeed)
		op                func(t *testing.T, s *Store, seed *snapshotSeed) error
	}{
		{
			name: "新增服务器",
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				_, err := s.AddServer(newServer("extra", "192.0.2.11"))
				return err
			},
		},
		{
			name: "新增站点",
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				_, err := s.AddSite("办公室", "新增站点")
				return err
			},
		},
		{
			name: "编辑站点",
			setup: func(t *testing.T, s *Store, seed *snapshotSeed) {
				if _, err := s.AddSite("办公室", "原备注"); err != nil {
					t.Fatal(err)
				}
			},
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				note := "新备注"
				_, err := s.EditSite("办公室", SiteChanges{Note: &note})
				return err
			},
		},
		{
			name: "删除站点",
			setup: func(t *testing.T, s *Store, seed *snapshotSeed) {
				if _, err := s.AddSite("办公室", ""); err != nil {
					t.Fatal(err)
				}
			},
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				return s.DeleteSite("办公室")
			},
		},
		{
			name: "新增网络",
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				_, err := s.AddNetwork(models.Network{Name: "虚拟网", Type: models.NetworkVirtual})
				return err
			},
		},
		{
			name: "编辑网络",
			setup: func(t *testing.T, s *Store, seed *snapshotSeed) {
				if _, err := s.AddNetwork(models.Network{
					Name: "虚拟网", Type: models.NetworkVirtual, NetworkRange: "192.0.2.0/24",
				}); err != nil {
					t.Fatal(err)
				}
			},
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				networkRange := "198.51.100.0/24"
				_, err := s.EditNetwork("虚拟网", NetworkChanges{NetworkRange: &networkRange})
				return err
			},
		},
		{
			name: "删除网络",
			setup: func(t *testing.T, s *Store, seed *snapshotSeed) {
				if _, err := s.AddNetwork(models.Network{Name: "虚拟网", Type: models.NetworkVirtual}); err != nil {
					t.Fatal(err)
				}
			},
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				_, err := s.DeleteNetwork("虚拟网")
				return err
			},
		},
		{
			name: "新增设备",
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				_, err := s.AddDevice("工作站", "新增设备", "")
				return err
			},
		},
		{
			name: "编辑设备",
			setup: func(t *testing.T, s *Store, seed *snapshotSeed) {
				if _, err := s.AddDevice("工作站", "原备注", ""); err != nil {
					t.Fatal(err)
				}
			},
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				note := "新备注"
				_, err := s.EditDevice("工作站", DeviceChanges{Note: &note})
				return err
			},
		},
		{
			name: "删除设备",
			setup: func(t *testing.T, s *Store, seed *snapshotSeed) {
				if _, err := s.AddDevice("工作站", "", ""); err != nil {
					t.Fatal(err)
				}
			},
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				preview, err := s.DeviceDeletePreview("工作站")
				if err != nil {
					return err
				}
				_, err = s.DeleteDeviceCascading("工作站", preview)
				return err
			},
		},
		{
			name: "关联客户端实例",
			setup: func(t *testing.T, s *Store, seed *snapshotSeed) {
				if _, _, err := s.AddCacheToken("laptop", seed.profileID); err != nil {
					t.Fatal(err)
				}
				if _, err := s.AddDevice("工作站", "", ""); err != nil {
					t.Fatal(err)
				}
			},
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				client := "laptop"
				_, err := s.EditDevice("工作站", DeviceChanges{Client: &client})
				return err
			},
		},
		{
			name: "解除客户端实例关联",
			setup: func(t *testing.T, s *Store, seed *snapshotSeed) {
				if _, _, err := s.AddCacheToken("laptop", seed.profileID); err != nil {
					t.Fatal(err)
				}
				if _, err := s.AddDevice("工作站", "", "laptop"); err != nil {
					t.Fatal(err)
				}
			},
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				client := ""
				_, err := s.EditDevice("工作站", DeviceChanges{Client: &client})
				return err
			},
		},
		{
			name: "新增网络成员",
			setup: func(t *testing.T, s *Store, seed *snapshotSeed) {
				if _, err := s.AddDevice("工作站", "", ""); err != nil {
					t.Fatal(err)
				}
				if _, err := s.AddNetwork(models.Network{Name: "虚拟网", Type: models.NetworkVirtual}); err != nil {
					t.Fatal(err)
				}
			},
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				_, err := s.AddNetworkMembership("工作站", "虚拟网", "192.0.2.20")
				return err
			},
		},
		{
			name: "编辑网络成员",
			setup: func(t *testing.T, s *Store, seed *snapshotSeed) {
				if _, err := s.AddDevice("工作站", "", ""); err != nil {
					t.Fatal(err)
				}
				if _, err := s.AddNetwork(models.Network{Name: "虚拟网", Type: models.NetworkVirtual}); err != nil {
					t.Fatal(err)
				}
				if _, err := s.AddNetworkMembership("工作站", "虚拟网", "192.0.2.20"); err != nil {
					t.Fatal(err)
				}
			},
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				_, err := s.EditNetworkMembership("工作站", "虚拟网", "198.51.100.20")
				return err
			},
		},
		{
			name: "删除网络成员",
			setup: func(t *testing.T, s *Store, seed *snapshotSeed) {
				if _, err := s.AddDevice("工作站", "", ""); err != nil {
					t.Fatal(err)
				}
				if _, err := s.AddNetwork(models.Network{Name: "虚拟网", Type: models.NetworkVirtual}); err != nil {
					t.Fatal(err)
				}
				if _, err := s.AddNetworkMembership("工作站", "虚拟网", "192.0.2.20"); err != nil {
					t.Fatal(err)
				}
			},
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				_, err := s.RemoveNetworkMembership("工作站", "虚拟网")
				return err
			},
		},
		{
			name: "移动服务器条目所属设备",
			setup: func(t *testing.T, s *Store, seed *snapshotSeed) {
				if _, err := s.AddDevice("机器A", "", ""); err != nil {
					t.Fatal(err)
				}
				if _, err := s.AddDevice("机器B", "", ""); err != nil {
					t.Fatal(err)
				}
				if _, err := s.MoveServerToDevice(seed.serverID, "机器A"); err != nil {
					t.Fatal(err)
				}
			},
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				_, err := s.MoveServerToDevice(seed.serverID, "机器B")
				return err
			},
		},
		{
			name: "更新服务器",
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				srv, err := s.GetServer(seed.serverID)
				if err != nil {
					return err
				}
				srv.Role = "edge gateway"
				return s.UpdateServer(srv)
			},
		},
		{
			name: "删除服务器",
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				return s.DeleteServer(seed.serverID)
			},
		},
		{
			name: "新增凭据",
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				_, err := s.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("new")})
				return err
			},
		},
		{
			name: "带凭据新增服务器",
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				_, err := s.AddServerWithCredentials(newServer("boxed", "192.0.2.12"),
					&models.Credential{Type: models.CredPassword, Secret: []byte("boxed")}, nil)
				return err
			},
		},
		{
			name: "清除服务器凭据",
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				return s.ClearServerCredential(seed.serverID)
			},
		},
		{
			name: "新增 profile",
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				_, err := s.AddProfile("team-b")
				return err
			},
		},
		{
			name:              "改绑设备码",
			sameWholeSnapshot: true,
			setup: func(t *testing.T, s *Store, seed *snapshotSeed) {
				profileID, err := s.AddProfile("team-b")
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := s.AddCacheToken("laptop", seed.profileID); err != nil {
					t.Fatal(err)
				}
				seed.rebindProfileID = profileID
			},
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				return s.BindCacheToken("laptop", seed.rebindProfileID)
			},
		},
		{
			name: "授权服务器",
			setup: func(t *testing.T, s *Store, seed *snapshotSeed) {
				id, err := s.AddServer(newServer("grant-me", "192.0.2.13"))
				if err != nil {
					t.Fatal(err)
				}
				seed.serverID = id
			},
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				return s.GrantServers(seed.profileID, []string{seed.serverID})
			},
		},
		{
			name: "撤销授权",
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				_, _, err := s.SyncServers(seed.profileID, nil)
				return err
			},
		},
		{
			name: "删除 profile",
			setup: func(t *testing.T, s *Store, seed *snapshotSeed) {
				id, err := s.AddProfile("team-b")
				if err != nil {
					t.Fatal(err)
				}
				seed.profileID = id
			},
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				return s.DeleteProfile(seed.profileID)
			},
		},
		{
			name: "新增项目",
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				_, _, err := s.AddProject("proj-y", seed.profileID)
				return err
			},
		},
		{
			name: "停用项目",
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				return s.SetProjectStatus(seed.projectID, models.ProjectDisabled)
			},
		},
		{
			name: "轮换项目令牌",
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				_, err := s.RotateProject(seed.projectID)
				return err
			},
		},
		{
			name: "删除项目",
			setup: func(t *testing.T, s *Store, seed *snapshotSeed) {
				if err := s.SetProjectStatus(seed.projectID, models.ProjectRevoked); err != nil {
					t.Fatal(err)
				}
			},
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				return s.DeleteProject(seed.projectID)
			},
		},
		{
			name: "新增命令策略规则",
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				return s.AddPolicyRule(policy.CustomRule{
					ID: "NO-EXAMPLE", Commands: []string{"example"}, Disposition: policy.DispositionBlock,
					Explanation: "示例规则",
				})
			},
		},
		{
			name: "新增机密项",
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				return s.AddSecret("DEPLOY_TOKEN", "部署令牌", []byte("revision-add-secret"))
			},
		},
		{
			name: "更换机密项值",
			setup: func(t *testing.T, s *Store, seed *snapshotSeed) {
				if err := s.AddSecret("DEPLOY_TOKEN", "部署令牌", []byte("revision-old-secret")); err != nil {
					t.Fatal(err)
				}
			},
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				return s.SetSecretValue("DEPLOY_TOKEN", []byte("revision-new-secret"))
			},
		},
		{
			name: "修改机密项备注",
			setup: func(t *testing.T, s *Store, seed *snapshotSeed) {
				if err := s.AddSecret("DEPLOY_TOKEN", "旧备注", []byte("revision-note-secret")); err != nil {
					t.Fatal(err)
				}
			},
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				return s.EditSecretNote("DEPLOY_TOKEN", "新备注")
			},
		},
		{
			name: "删除机密项",
			setup: func(t *testing.T, s *Store, seed *snapshotSeed) {
				if err := s.AddSecret("DEPLOY_TOKEN", "部署令牌", []byte("revision-delete-secret")); err != nil {
					t.Fatal(err)
				}
			},
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				return s.DeleteSecret("DEPLOY_TOKEN")
			},
		},
		{
			name: "增加机密项可写入服务器",
			setup: func(t *testing.T, s *Store, seed *snapshotSeed) {
				if err := s.AddSecret("DEPLOY_TOKEN", "部署令牌", []byte("revision-allow-secret")); err != nil {
					t.Fatal(err)
				}
			},
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				srv, err := s.GetServer(seed.serverID)
				if err != nil {
					return err
				}
				return s.AllowSecrets([]string{"DEPLOY_TOKEN"}, []string{srv.Name})
			},
		},
		{
			name: "减少机密项可写入服务器",
			setup: func(t *testing.T, s *Store, seed *snapshotSeed) {
				if err := s.AddSecret("DEPLOY_TOKEN", "部署令牌", []byte("revision-disallow-secret")); err != nil {
					t.Fatal(err)
				}
				srv, err := s.GetServer(seed.serverID)
				if err != nil {
					t.Fatal(err)
				}
				if err := s.AllowSecrets([]string{"DEPLOY_TOKEN"}, []string{srv.Name}); err != nil {
					t.Fatal(err)
				}
			},
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				srv, err := s.GetServer(seed.serverID)
				if err != nil {
					return err
				}
				return s.DisallowSecrets([]string{"DEPLOY_TOKEN"}, []string{srv.Name})
			},
		},
		{
			name: "删除命令策略规则",
			setup: func(t *testing.T, s *Store, seed *snapshotSeed) {
				if err := s.AddPolicyRule(policy.CustomRule{
					ID: "NO-EXAMPLE", Commands: []string{"example"}, Disposition: policy.DispositionBlock,
					Explanation: "示例规则",
				}); err != nil {
					t.Fatal(err)
				}
			},
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				_, err := s.DeletePolicyRule("NO-EXAMPLE")
				return err
			},
		},
		{
			name: "停用默认命令策略规则",
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				_, err := s.DisableDefaultRule(defaultRuleID)
				return err
			},
		},
		{
			name: "启用默认命令策略规则",
			setup: func(t *testing.T, s *Store, seed *snapshotSeed) {
				if _, err := s.DisableDefaultRule(defaultRuleID); err != nil {
					t.Fatal(err)
				}
			},
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				_, err := s.EnableDefaultRule(defaultRuleID)
				return err
			},
		},
		{
			name: "手动锚定主机密钥",
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				srv, err := s.GetServer(seed.serverID)
				if err != nil {
					return err
				}
				_, err = s.UpsertManualEntryPin(srv, "192.0.2.10:22", []byte("anchor-blob"), PinFormatBlob, false,
					func(old *PinMeta) AuditRow { return AuditRow{Action: "pin-manual"} })
				return err
			},
		},
		{
			name: "首次信任主机密钥",
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				srv, err := s.GetServer(seed.serverID)
				if err != nil {
					return err
				}
				return s.SaveEntryPin(srv, []byte("tofu-blob"))
			},
		},
		{
			name: "清除主机密钥锚",
			setup: func(t *testing.T, s *Store, seed *snapshotSeed) {
				srv, err := s.GetServer(seed.serverID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.UpsertManualEntryPin(srv, "192.0.2.10:22", []byte("anchor-blob"), PinFormatBlob, false,
					func(old *PinMeta) AuditRow { return AuditRow{Action: "pin-manual"} }); err != nil {
					t.Fatal(err)
				}
			},
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				_, err := s.ClearEntryPin(seed.serverID, func(old PinMeta) AuditRow { return AuditRow{Action: "pin-clear"} })
				return err
			},
		},
		{
			name: "客户端转发主机密钥锚",
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				srv, err := s.GetServer(seed.serverID)
				if err != nil {
					return err
				}
				_, err = s.InsertForwardedEntryPin(srv, "192.0.2.10:22", []byte("forwarded-blob"), "laptop",
					AuditRow{Action: "pin-forward", Status: "ok"})
				return err
			},
		},
		{
			name: "编辑服务器元数据",
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				role := "edge gateway"
				_, err := s.UpdateForwardedMetadata(seed.serverID, map[string]*string{"role": &role}, 0, "laptop")
				return err
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			s, seed := seedSnapshotStore(t)
			if tc.setup != nil {
				tc.setup(t, s, seed)
			}
			before, beforeContent := snapshotContent(t, s)
			if err := tc.op(t, s, seed); err != nil {
				t.Fatalf("执行写入失败:%v", err)
			}
			after, afterContent := snapshotContent(t, s)
			if !tc.sameWholeSnapshot && beforeContent == afterContent {
				t.Fatalf("操作表条目「%s」没有改变导出快照内容,请修正测试或操作表", tc.name)
			}
			if after <= before {
				t.Fatalf("「%s」后快照修订号 = %d,应大于 %d", tc.name, after, before)
			}
		})
	}
}

// TestSnapshotRevision_TopologyTransactionRollsBack: 拓扑表写入让修订号
// 前进发生在同一个数据库事务里。设备删除在删除网络成员之后失败时，已触
// 发的修订号加一必须随事务一起回滚。
func TestSnapshotRevision_TopologyTransactionRollsBack(t *testing.T) {
	s := newTestStore(t)
	device, err := s.AddDevice("工作站", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddSite("办公室", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddNetwork(models.Network{
		Name: "办公局域网", Type: models.NetworkPhysical, SiteName: "办公室",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddNetworkMembership("工作站", "办公局域网", "192.0.2.20"); err != nil {
		t.Fatal(err)
	}
	before, err := s.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO device_layout_coordinates(view_key,device_id,x,y,created_at,updated_at)
		 VALUES(?,?,?,?,?,?)`, "主视图", device.ID, 1, 2, 1000, 1000,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(
		`CREATE TRIGGER topology_delete_layout_fail
		 BEFORE DELETE ON device_layout_coordinates
		 BEGIN SELECT RAISE(ABORT, 'injected topology rollback failure'); END`,
	); err != nil {
		t.Fatal(err)
	}
	preview, err := s.DeviceDeletePreview("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteDeviceCascading("工作站", preview); err == nil ||
		!strings.Contains(err.Error(), "injected topology rollback failure") {
		t.Fatalf("设备删除应暴露注入的失败，实际:%v", err)
	}
	after, err := s.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("事务回滚后的快照修订号 = %d,应保持 %d", after, before)
	}
}

// TestSnapshotRevision_AuditOnlyWritesDoNotAdvance: 只写审计与设备码状态的
// 操作不改变快照修订号。
func TestSnapshotRevision_AuditOnlyWritesDoNotAdvance(t *testing.T) {
	s, seed := seedSnapshotStore(t)
	tokenID, _, err := s.AddCacheToken("laptop", seed.profileID)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := snapshotContent(t, s)
	writes := []struct {
		name string
		run  func() error
	}{
		{"普通远程命令审计", func() error {
			return s.WriteAudit(AuditRow{Action: "exec", ServerID: seed.serverID, Command: "uptime", Status: "ok"})
		}},
		{"记录设备码最后拉取时间", func() error { return s.TouchCacheToken(tokenID, "1.2.3") }},
		{"吊销设备码", func() error { return s.RevokeCacheToken("laptop") }},
	}
	for _, w := range writes {
		if err := w.run(); err != nil {
			t.Fatalf("%s:%v", w.name, err)
		}
		after, _ := snapshotContent(t, s)
		if after != before {
			t.Fatalf("%s 后快照修订号 = %d,应保持 %d", w.name, after, before)
		}
	}
}

// TestSnapshotRevision_RidesBothExportPaths: 两条导出路径都携带顶层
// snapshot_revision,信封版本号保持 1;导入后的修订号必须大于导入前的当前
// 值与快照携带值,保持单调递增。
func TestSnapshotRevision_RidesBothExportPaths(t *testing.T) {
	s, seed := seedSnapshotStore(t)
	if _, err := s.AddServer(&models.Server{Name: "extra", Host: "192.0.2.11", Port: 22, User: "u", AuthMethod: models.AuthPassword}); err != nil {
		t.Fatal(err)
	}
	rev, err := s.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	whole, err := s.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if whole.Version != 1 || whole.SnapshotRevision != rev {
		t.Fatalf("全库导出:信封版本 = %d,快照修订号 = %d,应为 1/%d", whole.Version, whole.SnapshotRevision, rev)
	}
	scoped, err := s.ExportSnapshotForProfile(seed.profileID, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if scoped.Version != 1 || scoped.SnapshotRevision != rev {
		t.Fatalf("profile 导出:信封版本 = %d,快照修订号 = %d,应为 1/%d", scoped.Version, scoped.SnapshotRevision, rev)
	}

	mk := make([]byte, 32)
	randRead(t, mk)
	target, err := Open(filepath.Join(t.TempDir(), "import.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	// 目标库先做几轮「新增又删除服务器」的空库写入,让导入前的修订号高于
	// 快照携带值,覆盖「取两者较大值」这一侧。
	for i := 0; i < int(rev)+2; i++ {
		id, aerr := target.AddServer(&models.Server{
			Name: "temporary", Host: "192.0.2.99", Port: 22, User: "u", AuthMethod: models.AuthPassword,
		})
		if aerr != nil {
			t.Fatal(aerr)
		}
		if derr := target.DeleteServer(id); derr != nil {
			t.Fatal(derr)
		}
	}
	currentBefore, err := target.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if currentBefore <= rev {
		t.Fatalf("测试前置失败:导入前修订号 = %d,应大于快照携带值 %d", currentBefore, rev)
	}
	if err := target.ImportSnapshot(whole); err != nil {
		t.Fatal(err)
	}
	imported, err := target.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if imported <= currentBefore || imported <= rev {
		t.Fatalf("导入后的快照修订号 = %d,应大于导入前值 %d 与快照携带值 %d", imported, currentBefore, rev)
	}
	if imported != currentBefore+1 {
		t.Fatalf("导入后的快照修订号 = %d,应为导入前值与快照携带值中较大者的后一个值 %d", imported, currentBefore+1)
	}
}

// TestSnapshotRevision_LegacySnapshotDecodesAsZero: 旧版快照结构没有
// snapshot_revision 字段,解码为 0,不影响导入。
func TestSnapshotRevision_LegacySnapshotDecodesAsZero(t *testing.T) {
	legacy := `{"version":1,"credentials":[],"servers":[],"profiles":[],"grants":[],"projects":[],"host_keys":[],"audit":[]}`
	var snap Snapshot
	if err := json.Unmarshal([]byte(legacy), &snap); err != nil {
		t.Fatalf("旧版快照解码失败:%v", err)
	}
	if snap.SnapshotRevision != 0 {
		t.Fatalf("旧版快照修订号 = %d,应为 0", snap.SnapshotRevision)
	}
	s := newTestStore(t)
	if err := s.ImportSnapshot(&snap); err != nil {
		t.Fatal(err)
	}
}
