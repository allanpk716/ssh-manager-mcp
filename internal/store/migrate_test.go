package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// oldShapeServersProjects is the pre-Plan-8 shape: no servers.description, no projects.status.
// Used to prove migrate() upgrades an existing DB in place.
const oldShapeServersProjects = `
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
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE projects (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  token_hash BLOB NOT NULL,
  token_salt BLOB NOT NULL,
  token_prefix TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
`

// serverMetadataColumns is the set of structured-metadata columns on servers.
// Shared by every migration-shape test so the column list lives in one place.
var serverMetadataColumns = []string{"description", "hardware", "services", "role", "caveats"}

func hasColumn(t *testing.T, db *sql.DB, table, col string) bool {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		if name == col {
			return true
		}
	}
	return false
}

// TestFreshSchemaHasNewColumns: a brand-new DB (via Open → initSchema) has both columns.
func TestFreshSchemaHasNewColumns(t *testing.T) {
	s := newTestStore(t)
	for _, col := range serverMetadataColumns {
		if !hasColumn(t, s.db, "servers", col) {
			t.Fatalf("fresh servers table missing %s column", col)
		}
	}
	if !hasColumn(t, s.db, "projects", "status") {
		t.Fatal("fresh projects table missing status column")
	}
}

// TestMigrateAddsColumnsToOldShape: an old-shape DB gets the columns via Open's migrate().
//
// DEFERRED (finding #4 of the final review): the review also asked this test to seed an
// old-shape server row, run Open (migrate), then GetServer and assert the 5 new metadata
// columns read back as "". That assertion does NOT hold today: addColumnIfMissing adds
// the columns as plain "TEXT" (no DEFAULT), so pre-migration rows get SQL NULL, and
// scanServer scans them into a Go string (not sql.NullString) — which errors with
// "converting NULL to string is unsupported". So a pre-Plan-8 DB upgraded in place would
// leave every existing server row UNREADABLE via GetServer/GetServerByName/ListServers.
// Fixing it is a production change (addColumn DEFAULT ” or NullString scan) outside this
// review-fix brief's scope ("Do NOT touch production behavior beyond #2 and #6"). The
// deferral is tracked in the final-review-fix-report; the seed-row + GetServer assertion
// will be added alongside the migration fix.
func TestMigrateAddsColumnsToOldShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(oldShapeServersProjects); err != nil {
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
	for _, col := range serverMetadataColumns {
		if !hasColumn(t, s.db, "servers", col) {
			t.Fatalf("migrate did not add servers.%s", col)
		}
	}
	if !hasColumn(t, s.db, "projects", "status") {
		t.Fatal("migrate did not add projects.status")
	}
}

// TestMigrateIdempotent: reopening a migrated DB (migrate runs again) must not error.
func TestMigrateIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idem.db")
	mk := make([]byte, 32)
	randRead(t, mk)
	s1, err := Open(path, mk)
	if err != nil {
		t.Fatal(err)
	}
	s1.Close()
	s2, err := Open(path, mk) // migrate runs again → must be a no-op, not "duplicate column"
	if err != nil {
		t.Fatalf("reopen (migrate again) failed: %v", err)
	}
	t.Cleanup(func() { s2.Close() })
}

// TestMigrateSoftwareCollectionContainerShape 证明旧版实测条目表会保留数
// 据与监听端口子表完成重建，并接受第 3 版的「容器」类别。
func TestMigrateSoftwareCollectionContainerShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "software-v2.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	oldShape := `
CREATE TABLE devices (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE
);
CREATE TABLE software_observed_items (
  id TEXT PRIMARY KEY,
  device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  stable_key TEXT NOT NULL,
  category TEXT NOT NULL CHECK(category IN ('operating_system','service_program','container_image','driver_firmware','system_package','unidentified_process')),
  name TEXT NOT NULL,
  name_lower TEXT NOT NULL,
  version TEXT NOT NULL DEFAULT '',
  package_url TEXT NOT NULL DEFAULT '',
  belongs_to TEXT NOT NULL DEFAULT '',
  architecture TEXT NOT NULL DEFAULT '',
  container_tag TEXT NOT NULL DEFAULT '',
  container_digest TEXT NOT NULL DEFAULT '',
  container_running INTEGER NOT NULL DEFAULT 0,
  executable_path TEXT NOT NULL DEFAULT '',
  process_name TEXT NOT NULL DEFAULT '',
  process_id INTEGER NOT NULL DEFAULT 0,
  collected_at INTEGER NOT NULL,
  UNIQUE(device_id, stable_key)
);
CREATE TABLE software_observed_ports (
  id TEXT PRIMARY KEY,
  item_id TEXT NOT NULL REFERENCES software_observed_items(id) ON DELETE CASCADE,
  device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  address TEXT NOT NULL,
  port INTEGER NOT NULL,
  protocol TEXT NOT NULL,
  exposure TEXT NOT NULL,
  UNIQUE(item_id, address, port, protocol)
);
CREATE TABLE software_collection_status (
  device_id TEXT PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
  command_set_version TEXT NOT NULL DEFAULT '',
  system_type TEXT NOT NULL DEFAULT ''
);
CREATE TABLE software_declarations (
  id TEXT PRIMARY KEY,
  device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  category TEXT NOT NULL CHECK(category IN ('operating_system','service_program','container_image','driver_firmware','system_package','unidentified_process')),
  name TEXT NOT NULL,
  name_lower TEXT NOT NULL,
  version TEXT NOT NULL,
  package_url TEXT NOT NULL DEFAULT '',
  cpe TEXT NOT NULL DEFAULT '',
  belongs_to TEXT NOT NULL DEFAULT '',
  note TEXT NOT NULL DEFAULT '',
  claimed_observed_key TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'active' CHECK(status IN ('active','draft')),
  confirmed_at INTEGER NOT NULL,
  created_source TEXT NOT NULL DEFAULT '',
  last_source TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE software_declaration_ports (
  id TEXT PRIMARY KEY,
  declaration_id TEXT NOT NULL REFERENCES software_declarations(id) ON DELETE CASCADE,
  device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  address TEXT NOT NULL,
  port INTEGER NOT NULL,
  protocol TEXT NOT NULL,
  exposure TEXT NOT NULL
);
INSERT INTO devices(id,name) VALUES('device-1','工作站');
INSERT INTO software_observed_items(
  id,device_id,stable_key,category,name,name_lower,collected_at
) VALUES('item-1','device-1','old-key','service_program','nginx','nginx',100);
INSERT INTO software_observed_ports(
  id,item_id,device_id,address,port,protocol,exposure
) VALUES('port-1','item-1','device-1','0.0.0.0',80,'tcp','all_addresses');
INSERT INTO software_declarations(
  id,device_id,category,name,name_lower,version,confirmed_at,created_at,updated_at
) VALUES('declaration-1','device-1','service_program','nginx','nginx','1.18',0,0,0);
INSERT INTO software_declaration_ports(
  id,declaration_id,device_id,address,port,protocol,exposure
) VALUES('declaration-port-1','declaration-1','device-1','0.0.0.0',80,'tcp','all_addresses');
`
	if _, err := db.Exec(oldShape); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	key := make([]byte, 32)
	randRead(t, key)
	st, err := Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, column := range []string{"container_name", "image_pullability"} {
		if !hasColumn(t, st.db, "software_observed_items", column) {
			t.Fatalf("迁移后的实测条目表缺少 %s 列", column)
		}
	}
	for _, column := range []string{"distro_id", "distro_version_id", "unused_images"} {
		if !hasColumn(t, st.db, "software_collection_status", column) {
			t.Fatalf("迁移后的采集状态表缺少 %s 列", column)
		}
	}
	var itemCount, portCount int
	if err := st.db.QueryRow(`SELECT count(*) FROM software_observed_items`).Scan(&itemCount); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRow(`SELECT count(*) FROM software_observed_ports`).Scan(&portCount); err != nil {
		t.Fatal(err)
	}
	if itemCount != 1 || portCount != 1 {
		t.Fatalf("迁移应保留旧条目与监听端口：条目 %d，端口 %d", itemCount, portCount)
	}
	var declarationCount, declarationPortCount int
	if err := st.db.QueryRow(`SELECT count(*) FROM software_declarations`).Scan(&declarationCount); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRow(`SELECT count(*) FROM software_declaration_ports`).Scan(&declarationPortCount); err != nil {
		t.Fatal(err)
	}
	if declarationCount != 1 || declarationPortCount != 1 {
		t.Fatalf("迁移应保留旧声明与声明端口：声明 %d，端口 %d", declarationCount, declarationPortCount)
	}
	if _, err := st.db.Exec(`
INSERT INTO software_observed_items(
  id,device_id,stable_key,category,name,name_lower,collected_at
) VALUES('item-2','device-1','container-key','container','gateway','gateway',200)`); err != nil {
		t.Fatalf("迁移后的类别约束应接受容器条目：%v", err)
	}
	if _, err := st.db.Exec(`
INSERT INTO software_declarations(
  id,device_id,category,name,name_lower,version,confirmed_at,created_at,updated_at
) VALUES('declaration-2','device-1','container','gateway','gateway','1.25.3',0,0,0)`); err != nil {
		t.Fatalf("迁移后的声明类别约束应接受容器声明：%v", err)
	}
}
