package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// legacyProjectsProfileIDShape 是 projects.profile_id 仍为 NOT NULL 的旧库
// 形态。规格 #144（票 #145）把它改为可空：已吊销项目令牌的绑定在删除
// profile 时可以置空，不再阻塞删除。
const legacyProjectsProfileIDShape = `
CREATE TABLE profiles (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE projects (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  token_hash BLOB NOT NULL,
  token_salt BLOB NOT NULL,
  token_prefix TEXT NOT NULL,
  profile_id TEXT NOT NULL REFERENCES profiles(id),
  status TEXT NOT NULL DEFAULT 'active',
  pair_generated INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
`

// TestMigrateProjectsProfileIDNullable 旧库在打开时把 projects.profile_id
// 重建为可空：行与绑定原样保留，外部键引用仍在。
func TestMigrateProjectsProfileIDNullable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "store.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(legacyProjectsProfileIDShape); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO profiles (id,name,created_at,updated_at) VALUES ('p1','team-a',1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id,name,token_hash,token_salt,token_prefix,profile_id,status,pair_generated,created_at,updated_at)
		VALUES ('j1','proj-web',x'00',x'00','prefix001','p1','active',0,1,1)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s := openStoreAt(t, dbPath)
	var notnull int
	if err := s.db.QueryRow(`SELECT "notnull" FROM pragma_table_info('projects') WHERE name='profile_id'`).Scan(&notnull); err != nil {
		t.Fatal(err)
	}
	if notnull != 0 {
		t.Fatalf("projects.profile_id 迁移后仍为 NOT NULL")
	}
	proj, err := s.GetProject("j1")
	if err != nil || proj == nil || proj.ProfileID != "p1" || proj.Status != "active" {
		t.Fatalf("迁移后项目行应原样保留：%+v %v", proj, err)
	}
	// 外部键仍然生效：指向不存在 profile 的项目插不进去。
	if _, _, err := s.AddProject("bogus-ref", "no-such-profile"); err == nil {
		t.Fatal("指向不存在 profile 的项目应被外部键拒绝")
	}
	// 快照修订号触发器在表重建后仍由结构初始化补回：写 projects 行会推进修订号。
	before, err := s.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AddProject("after-migrate", "p1"); err != nil {
		t.Fatal(err)
	}
	after, err := s.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if after <= before {
		t.Fatalf("迁移后写 projects 应推进快照修订号：%d -> %d", before, after)
	}
}

// TestSnapshotRoundTripRevokedProjectWithoutProfile 已吊销项目令牌的空绑定
// （profile 删除后留下的惰性绑定）在导出→导入回环里保持 NULL 语义：空
// 字符串过不了外部键，也不该凭空指向一个不存在的 profile。
func TestSnapshotRoundTripRevokedProjectWithoutProfile(t *testing.T) {
	a := newTestStore(t)
	profID, err := a.AddProfile("team-a")
	if err != nil {
		t.Fatal(err)
	}
	projID, _, err := a.AddProject("proj-web", profID)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SetProjectStatus(projID, models.ProjectRevoked); err != nil {
		t.Fatal(err)
	}
	// 项目已吊销，profile 删除把它的绑定置空。
	if err := a.DeleteProfile(profID); err != nil {
		t.Fatal(err)
	}
	snap, err := a.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Projects) != 1 || snap.Projects[0].ProfileID != "" {
		t.Fatalf("导出应把空绑定保持为空字符串：%+v", snap.Projects)
	}

	b := newTestStore(t)
	if err := b.ImportSnapshot(snap); err != nil {
		t.Fatalf("导入含空绑定项目的快照失败：%v", err)
	}
	proj, err := b.GetProject(projID)
	if err != nil || proj == nil || proj.ProfileID != "" || proj.Status != models.ProjectRevoked {
		t.Fatalf("导入后的项目应为空绑定且已吊销：%+v %v", proj, err)
	}
}
