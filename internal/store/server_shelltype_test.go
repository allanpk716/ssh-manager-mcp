package store

// 命令策略·服务器 shell 类型字段(ticket #14)的存储层测试:字段属于
// 服务器连接信息(不是服务器元数据——元数据编辑的六字段白名单摸不到它),
// 三类值 POSIX / PowerShell / cmd 原样落库与读回;未标注(零值)按默认
// POSIX;旧库迁移补列后旧行读回 POSIX。

import (
	"database/sql"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// TestServerShellTypePersist: 显式标注与默认值在三条读取路径
// (GetServer / GetServerByName / ListServers)上往返一致;UpdateServer
// 写全行时携带该字段。
func TestServerShellTypePersist(t *testing.T) {
	s := newTestStore(t)
	id, err := s.AddServer(&models.Server{
		Name: "winps", Host: "h", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, ShellType: models.ShellPowerShell,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetServer(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.ShellType != models.ShellPowerShell {
		t.Fatalf("GetServer ShellType = %q, want powershell", got.ShellType)
	}
	byName, err := s.GetServerByName("winps")
	if err != nil || byName == nil {
		t.Fatalf("GetServerByName: %v %v", byName, err)
	}
	if byName.ShellType != models.ShellPowerShell {
		t.Fatalf("GetServerByName ShellType = %q, want powershell", byName.ShellType)
	}
	list, err := s.ListServers()
	if err != nil || len(list) != 1 {
		t.Fatalf("ListServers: %v (err %v)", list, err)
	}
	if list[0].ShellType != models.ShellPowerShell {
		t.Fatalf("ListServers ShellType = %q, want powershell", list[0].ShellType)
	}

	// 改标 cmd:UpdateServer 写全行,字段必须随之更新。
	got.ShellType = models.ShellWindowsCmd
	if err := s.UpdateServer(got); err != nil {
		t.Fatal(err)
	}
	after, _ := s.GetServer(id)
	if after.ShellType != models.ShellWindowsCmd {
		t.Fatalf("after update ShellType = %q, want cmd", after.ShellType)
	}
}

// TestServerShellTypeDefaultsPOSIX: 未标注(零值)的服务器按默认 POSIX
// 落库并读回——POSIX 是规格给定的默认值。
func TestServerShellTypeDefaultsPOSIX(t *testing.T) {
	s := newTestStore(t)
	id, err := s.AddServer(&models.Server{
		Name: "plain", Host: "h", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, // ShellType 留空 = 未标注
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetServer(id)
	if got.ShellType != models.ShellPOSIX {
		t.Fatalf("unset ShellType read back as %q, want posix", got.ShellType)
	}
}

// TestMigrateBackFillsShellTypePOSIX: 无 shell_type 列的库(上一版程序的
// 形态,且 credential_id NOT NULL——顺带触发 servers 表重建)迁移后:列
// 存在、旧行读回 POSIX(ADD COLUMN DEFAULT 回填)。重建先于补列是刻意
// 排序:重建的新表定义不含本列,顺序反了会把刚补的列整表丢掉。
func TestMigrateBackFillsShellTypePOSIX(t *testing.T) {
	// 上一版形态的 servers 表:与当前 schemaSQL 相同,但 credential_id
	// NOT NULL、且没有 shell_type 列。
	const prevShapeServers = `
CREATE TABLE credentials (
  id TEXT PRIMARY KEY,
  type TEXT NOT NULL,
  secret_blob BLOB NOT NULL,
  passphrase_blob BLOB,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE servers (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  host TEXT NOT NULL,
  port INTEGER NOT NULL,
  user TEXT NOT NULL,
  auth_method TEXT NOT NULL,
  credential_id TEXT NOT NULL REFERENCES credentials(id),
  sudo_credential_id TEXT,
  tags TEXT,
  description TEXT DEFAULT '',
  location TEXT DEFAULT '',
  hardware TEXT DEFAULT '',
  services TEXT DEFAULT '',
  role TEXT DEFAULT '',
  caveats TEXT DEFAULT '',
  expose_host INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  revision INTEGER NOT NULL DEFAULT 0
);`
	path := filepath.Join(t.TempDir(), "old-shell.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(prevShapeServers); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO servers (id,name,host,port,user,auth_method,credential_id,tags,created_at,updated_at)
		VALUES ('s1','legacy','h',22,'u','password','c','',1,1)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	mk := make([]byte, 32)
	randRead(t, mk)
	s, err := Open(path, mk) // runs migrate()
	if err != nil {
		t.Fatalf("Open after migrate: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if !hasColumn(t, s.db, "servers", "shell_type") {
		t.Fatal("migrate did not add servers.shell_type")
	}
	got, err := s.GetServerByName("legacy")
	if err != nil || got == nil {
		t.Fatalf("legacy row unreadable after migrate: %v %v", got, err)
	}
	if got.ShellType != models.ShellPOSIX {
		t.Fatalf("legacy row ShellType = %q, want posix (back-filled)", got.ShellType)
	}
}
