package store

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// TestOpenMigratesServerLocationIntoDescription 覆盖旧库升级：非空
// location 文本追加到说明末尾，随后 location 列被删除；重复打开不会第二
// 次追加。测试先用原生 SQL 造出仍带 location 列的旧库形态。
func TestOpenMigratesServerLocationIntoDescription(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")
	masterKey, err := GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := Open(path, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, srv := range []*models.Server{
		{Name: "with-both", Host: "192.0.2.10", Port: 22, User: "deploy", AuthMethod: models.AuthPassword, Description: "已有说明"},
		{Name: "location-only", Host: "192.0.2.11", Port: 22, User: "deploy", AuthMethod: models.AuthPassword},
		{Name: "empty-location", Host: "192.0.2.12", Port: 22, User: "deploy", AuthMethod: models.AuthPassword, Description: "说明保持不变"},
	} {
		if _, err := st.AddServer(srv); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.db.Exec(`ALTER TABLE servers ADD COLUMN location TEXT DEFAULT ''`); err != nil {
		t.Fatal(err)
	}
	for name, location := range map[string]string{
		"with-both": "机房 A", "location-only": "二层机柜", "empty-location": " ",
	} {
		if _, err := st.db.Exec(`UPDATE servers SET location=? WHERE name=?`, location, name); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// 重复打开既检验迁移，也检验幂等：第二次打开不能再追加一次。
	for round := 0; round < 2; round++ {
		migrated, err := Open(path, masterKey)
		if err != nil {
			t.Fatalf("open migrated store round %d: %v", round, err)
		}
		present, err := columnPresent(migrated.db, "servers", "location")
		if err != nil {
			t.Fatal(err)
		}
		if present {
			t.Fatalf("round %d：迁移后 servers 表不应再有 location 列", round)
		}
		for name, want := range map[string]string{
			"with-both":      "已有说明\n原位置：机房 A",
			"location-only":  "原位置：二层机柜",
			"empty-location": "说明保持不变",
		} {
			srv, err := migrated.GetServerByName(name)
			if err != nil || srv == nil {
				t.Fatalf("round %d 读取 %s 失败：%v", round, name, err)
			}
			if srv.Description != want {
				t.Fatalf("round %d：%s 的说明 = %q，应为 %q", round, name, srv.Description, want)
			}
		}
		if err := migrated.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// TestImportSnapshotMergesLegacyLocationIntoDescription 覆盖旧快照兼容：
// 旧版本导出的快照仍带 location 字段，导入时并入说明，不随字段取消丢失。
func TestImportSnapshotMergesLegacyLocationIntoDescription(t *testing.T) {
	st := openTestStore(t)
	raw := `{
		"version": 1,
		"servers": [{
			"id": "srv-legacy", "name": "legacy", "host": "192.0.2.13",
			"port": 22, "user": "deploy", "auth_method": "password", "tags": "",
			"description": "已有说明", "location": "一号机房",
			"created_at": 1, "updated_at": 1
		}]
	}`
	var snapshot Snapshot
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		t.Fatal(err)
	}
	if err := st.ImportSnapshot(&snapshot); err != nil {
		t.Fatal(err)
	}
	srv, err := st.GetServerByName("legacy")
	if err != nil || srv == nil {
		t.Fatalf("读取导入的旧快照服务器失败：%v", err)
	}
	want := "已有说明\n原位置：一号机房"
	if srv.Description != want {
		t.Fatalf("导入后的说明 = %q，应为 %q", srv.Description, want)
	}
}
