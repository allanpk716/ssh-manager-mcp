package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

func mustAddSite(t *testing.T, st *Store, name, note, parent string) *models.Site {
	t.Helper()
	site, err := st.AddSiteWithParent(name, note, parent)
	if err != nil {
		t.Fatalf("登记地点 %s（上级 %q）失败：%v", name, parent, err)
	}
	return site
}

func mustSetDeviceSite(t *testing.T, st *Store, device, site string) {
	t.Helper()
	if _, err := st.EditDevice(device, DeviceChanges{Site: &site}); err != nil {
		t.Fatalf("设置设备 %s 的地点 %q 失败：%v", device, site, err)
	}
}

func deviceSiteID(t *testing.T, st *Store, deviceName string) string {
	t.Helper()
	device, err := st.GetDevice(deviceName)
	if err != nil {
		t.Fatalf("读取设备 %s 失败：%v", deviceName, err)
	}
	return device.SiteID
}

func siteTreeTestString(value string) *string { return &value }

// TestSiteTreeParentAndUniqueness 覆盖多级地点的基本写入规则：同一上级
// 下名字唯一（忽略大小写）、不同上级下可以同名、地点可用完整路径指称。
func TestSiteTreeParentAndUniqueness(t *testing.T) {
	st := newTestStore(t)

	company := mustAddSite(t, st, "公司", "总部", "")
	factory := mustAddSite(t, st, "一厂区", "", company.Name)
	building := mustAddSite(t, st, "3 号楼", "", "公司 / 一厂区")
	home := mustAddSite(t, st, "家", "", "")
	secondBuilding := mustAddSite(t, st, "3 号楼", "另一处的同名楼", home.Name)

	if factory.ParentID != company.ID {
		t.Fatalf("一厂区的上级标识 = %q，应为公司 %q", factory.ParentID, company.ID)
	}
	if building.ParentID != factory.ID {
		t.Fatalf("3 号楼的上级标识 = %q，应为一厂区 %q", building.ParentID, factory.ID)
	}
	if secondBuilding.ParentID != home.ID {
		t.Fatalf("家的 3 号楼上级标识 = %q，应为家 %q", secondBuilding.ParentID, home.ID)
	}

	if _, err := st.AddSiteWithParent("一厂区", "", company.Name); !errors.Is(err, ErrSiteNameExists) {
		t.Fatalf("同一上级下的重名地点应被拒绝，got: %v", err)
	}

	moved, err := st.EditSite("公司 / 一厂区 / 3 号楼", SiteChanges{Note: siteTreeTestString("翻新")})
	if err != nil {
		t.Fatalf("用完整路径修改地点失败：%v", err)
	}
	if moved.ID != building.ID || moved.Note != "翻新" {
		t.Fatalf("完整路径应命中原来的 3 号楼：got %+v", moved)
	}

	if _, err := st.AddSiteWithParent("新楼", "", "公司 / 一厂区"); err != nil {
		t.Fatalf("以完整路径指定上级失败：%v", err)
	}
	if _, err := st.AddSiteWithParent("新楼", "", "公司 / 不存在"); !errors.Is(err, ErrSiteNotFound) {
		t.Fatalf("上级路径不存在应报地点不存在，got: %v", err)
	}

	sites, err := st.ListSites()
	if err != nil {
		t.Fatal(err)
	}
	parents := make(map[string]string, len(sites))
	for _, site := range sites {
		parents[site.ID] = site.ParentID
	}
	if parents[building.ID] != factory.ID {
		t.Fatalf("ListSites 应带出上级地点：got %+v", sites)
	}
}

// TestSiteTreeRejectsCycles 钉住成环保护：把上级改成自己或自己的下级都
// 必须在写入前被拒绝。
func TestSiteTreeRejectsCycles(t *testing.T) {
	st := newTestStore(t)
	company := mustAddSite(t, st, "公司", "", "")
	mustAddSite(t, st, "一厂区", "", company.Name)
	building := mustAddSite(t, st, "3 号楼", "", "公司 / 一厂区")

	if _, err := st.EditSite(company.Name, SiteChanges{Parent: siteTreeTestString(company.Name)}); err == nil ||
		!strings.Contains(err.Error(), "循环") {
		t.Fatalf("把上级改成自己应报循环，got: %v", err)
	}
	if _, err := st.EditSite(company.Name, SiteChanges{Parent: siteTreeTestString(building.Name)}); err == nil ||
		!strings.Contains(err.Error(), "循环") {
		t.Fatalf("把上级改成自己的下级应报循环，got: %v", err)
	}

	sites, err := st.ListSites()
	if err != nil {
		t.Fatal(err)
	}
	for _, site := range sites {
		if site.ParentID == site.ID {
			t.Fatalf("地点 %s 被写成了自己的上级", site.Name)
		}
	}
}

// TestSiteDeleteGuards 覆盖地点删除守卫：有下级地点、有机器、有物理局
// 域网指向时都要拒绝，并说明原因。
func TestSiteDeleteGuards(t *testing.T) {
	st := newTestStore(t)
	company := mustAddSite(t, st, "公司", "", "")
	building := mustAddSite(t, st, "3 号楼", "", company.Name)

	if err := st.DeleteSite(company.Name); err == nil || !strings.Contains(err.Error(), "下级地点") {
		t.Fatalf("有下级地点时删除应被拒绝，got: %v", err)
	}

	if _, err := st.AddDeviceAtSite("工作站", "", "", building.Name); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteSite(building.Name); err == nil || !strings.Contains(err.Error(), "机器") {
		t.Fatalf("有机器指向时删除应被拒绝，got: %v", err)
	}

	emptyBuilding := mustAddSite(t, st, "空楼", "", company.Name)
	if _, err := st.AddNetwork(models.Network{
		Name: "办公网", Type: models.NetworkPhysical, SiteName: emptyBuilding.Name,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteSite(emptyBuilding.Name); err == nil || !strings.Contains(err.Error(), "物理局域网") {
		t.Fatalf("有物理局域网指向时删除应被拒绝，got: %v", err)
	}

	if _, err := st.AddDeviceAtSite("临时机", "", "", ""); err != nil {
		t.Fatal(err)
	}
	mustSetDeviceSite(t, st, "工作站", "")
	if err := st.DeleteSite(building.Name); err != nil {
		t.Fatalf("解除全部引用后仍删除失败：%v", err)
	}
}

// TestDeviceSiteRegistrationAndFilter 覆盖机器直接登记地点与按地点树筛
// 选设备。
func TestDeviceSiteRegistrationAndFilter(t *testing.T) {
	st := newTestStore(t)
	company := mustAddSite(t, st, "公司", "", "")
	factory := mustAddSite(t, st, "一厂区", "", company.Name)
	building := mustAddSite(t, st, "3 号楼", "", factory.Name)
	mustAddSite(t, st, "家", "", "")
	mustAddSite(t, st, "3 号楼", "", "家")

	workstation, err := st.AddDeviceAtSite("工作站", "", "", "公司 / 一厂区 / 3 号楼")
	if err != nil {
		t.Fatal(err)
	}
	if workstation.SiteID != building.ID || workstation.SiteName != building.Name {
		t.Fatalf("机器应直接登记到最细一级地点：got %+v", workstation)
	}
	if _, err := st.AddDeviceAtSite("迷路机", "", "", "3 号楼"); err == nil ||
		!strings.Contains(err.Error(), "多个地点") {
		t.Fatalf("裸名字有多个同名地点时应报歧义，got: %v", err)
	}
	if _, err := st.AddDeviceAtSite("不存在机", "", "", "不存在"); !errors.Is(err, ErrSiteNotFound) {
		t.Fatalf("机器地点不存在应报地点不存在，got: %v", err)
	}

	mustSetDeviceSite(t, st, "工作站", company.Name)
	if got := deviceSiteID(t, st, "工作站"); got != company.ID {
		t.Fatalf("修改机器地点失败：got %q want %q", got, company.ID)
	}
	mustSetDeviceSite(t, st, "工作站", "")
	if got := deviceSiteID(t, st, "工作站"); got != "" {
		t.Fatalf("清空机器地点失败：got %q", got)
	}
	mustSetDeviceSite(t, st, "工作站", "公司 / 一厂区")

	if _, err := st.AddDeviceAtSite("厂房机", "", "", factory.Name); err != nil {
		t.Fatal(err)
	}
	devices, err := st.ListDevices(DeviceFilter{Site: company.Name})
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(devices))
	for _, device := range devices {
		names = append(names, device.Name)
	}
	if strings.Join(names, ",") != "厂房机,工作站" {
		t.Fatalf("按上级地点筛选应包含下级地点的机器：got %v", names)
	}
	if len(devices) != 2 {
		t.Fatalf("设备列表应带出机器地点：got %+v", devices)
	}
	sitesByName := make(map[string]string, len(devices))
	for _, device := range devices {
		sitesByName[device.Name] = device.SiteID
	}
	if sitesByName["厂房机"] != factory.ID || sitesByName["工作站"] != factory.ID {
		t.Fatalf("设备列表应带出机器地点：got %+v", devices)
	}
}

// TestSiteTreeSnapshotRoundTrip 覆盖快照携带地点上级与机器直接地点。
func TestSiteTreeSnapshotRoundTrip(t *testing.T) {
	dir := t.TempDir()
	masterKey, err := GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := Open(filepath.Join(dir, "source.db"), masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	company := mustAddSite(t, st, "公司", "", "")
	factory := mustAddSite(t, st, "一厂区", "", company.Name)
	building := mustAddSite(t, st, "3 号楼", "", factory.Name)
	if _, err := st.AddNetwork(models.Network{
		Name: "办公网", Type: models.NetworkPhysical, SiteName: "公司 / 一厂区 / 3 号楼",
	}); err != nil {
		t.Fatal(err)
	}
	workstation, err := st.AddDeviceAtSite("工作站", "", "", building.Name)
	if err != nil {
		t.Fatal(err)
	}

	snapshot, err := st.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Sites) != 3 {
		t.Fatalf("快照地点段 = %+v", snapshot.Sites)
	}
	byName := make(map[string]SnapshotSite, len(snapshot.Sites))
	for _, site := range snapshot.Sites {
		byName[site.Name] = site
	}
	if byName["一厂区"].ParentID != company.ID || byName["3 号楼"].ParentID != factory.ID {
		t.Fatalf("快照应携带地点上级：got %+v", snapshot.Sites)
	}
	if len(snapshot.Devices) != 1 || snapshot.Devices[0].SiteID != building.ID {
		t.Fatalf("快照应携带机器直接地点：got %+v", snapshot.Devices)
	}

	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"parent_id"`, `"site_id"`} {
		if !strings.Contains(string(encoded), field) {
			t.Fatalf("快照 JSON 缺少字段 %s：%s", field, encoded)
		}
	}

	target, err := Open(filepath.Join(dir, "target.db"), masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if err := target.ImportSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	restored, err := target.GetDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if restored.SiteID != workstation.SiteID {
		t.Fatalf("导入后机器地点丢失：got %+v", restored)
	}
	restoredSites, err := target.ListSites()
	if err != nil {
		t.Fatal(err)
	}
	for _, site := range restoredSites {
		if site.Name == "3 号楼" && site.ParentID != factory.ID {
			t.Fatalf("导入后地点上级丢失：got %+v", site)
		}
	}
}

// TestSiteTreeMigration 构造升级前的平铺地点库：sites 没有上级列，
// devices 没有地点列。打开后必须完成三件事：重建地点表允许不同上级下
// 同名；机器地点取它第一个物理局域网的地点（按网络名、地址排序）；
// 多物理局域网的机器在迁移审计里记一笔。重复打开不再改库。
func TestSiteTreeMigration(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "store.db")
	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	oldShape := `
CREATE TABLE sites (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  name_lower TEXT NOT NULL UNIQUE,
  note TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE networks (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  name_lower TEXT NOT NULL UNIQUE,
  type TEXT NOT NULL,
  site_id TEXT REFERENCES sites(id),
  note TEXT NOT NULL DEFAULT '',
  cidr TEXT NOT NULL DEFAULT '',
  bandwidth_mbps INTEGER NOT NULL DEFAULT 0,
  metered INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE devices (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  name_lower TEXT NOT NULL UNIQUE,
  note TEXT NOT NULL DEFAULT '',
  client_token_id TEXT UNIQUE,
  client_name TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE network_memberships (
  device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  network_id TEXT NOT NULL REFERENCES networks(id) ON DELETE CASCADE,
  address TEXT NOT NULL,
  address_lower TEXT NOT NULL,
  registration_order INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (device_id, network_id),
  UNIQUE (network_id, address_lower)
);
CREATE TABLE snapshot_revision (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  revision INTEGER NOT NULL
);
INSERT INTO snapshot_revision(id, revision) VALUES (1, 41);
INSERT INTO sites(id,name,name_lower,note,created_at,updated_at) VALUES
  ('site-office','公司','公司','',1700000000,1700000000),
  ('site-home','家','家','',1700000000,1700000000);
INSERT INTO networks(id,name,name_lower,type,site_id,note,cidr,bandwidth_mbps,metered,created_at,updated_at) VALUES
  ('net-a','A 网','a 网','physical','site-office','','',0,0,1700000000,1700000000),
  ('net-b','B 网','b 网','physical','site-home','','',0,0,1700000000,1700000000);
INSERT INTO devices(id,name,name_lower,note,client_token_id,client_name,created_at,updated_at) VALUES
  ('dev-multi','多网卡机','多网卡机','',NULL,'',1700000000,1700000000),
  ('dev-single','单网卡机','单网卡机','',NULL,'',1700000000,1700000000);
INSERT INTO network_memberships(device_id,network_id,address,address_lower,registration_order,created_at,updated_at) VALUES
  ('dev-multi','net-b','192.168.1.5','192.168.1.5',1,1700000000,1700000000),
  ('dev-multi','net-a','10.0.0.5','10.0.0.5',2,1700000000,1700000000),
  ('dev-single','net-b','192.168.1.9','192.168.1.9',3,1700000000,1700000000);
`
	if _, err := legacy.Exec(oldShape); err != nil {
		legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	mk, err := GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(dbPath, mk)
	if err != nil {
		t.Fatalf("升级打开旧库失败：%v", err)
	}
	if got := deviceSiteID(t, migrated, "多网卡机"); got != "site-office" {
		t.Fatalf("多物理局域网的机器应取第一个（按网络名、地址排序）的地点：got %q", got)
	}
	if got := deviceSiteID(t, migrated, "单网卡机"); got != "site-home" {
		t.Fatalf("单物理局域网机器的地点迁移错误：got %q", got)
	}

	auditRows, err := migrated.QueryAudit(AuditFilter{Actions: []string{"device.site-migrate"}})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range auditRows {
		var summary map[string]any
		if err := json.Unmarshal([]byte(row.Command), &summary); err != nil {
			t.Fatalf("迁移审计摘要不是 JSON：%v（%s）", err, row.Command)
		}
		devices, _ := summary["devices"].([]any)
		for _, item := range devices {
			entry, _ := item.(map[string]any)
			if entry["name"] != "多网卡机" {
				continue
			}
			found = true
			if entry["site"] != "公司" {
				t.Fatalf("迁移审计应记录选中的地点：got %+v", entry)
			}
			networks, _ := entry["physical-networks"].([]any)
			if len(networks) != 2 {
				t.Fatalf("多物理局域网机器的迁移审计应记录全部候选网络：got %+v", entry)
			}
		}
	}
	if !found {
		t.Fatalf("找不到多物理局域网机器的迁移审计：%+v", auditRows)
	}

	if _, err := migrated.AddSiteWithParent("园区", "", "公司"); err != nil {
		t.Fatal(err)
	}
	if _, err := migrated.AddSiteWithParent("园区", "", "家"); err != nil {
		t.Fatalf("不同上级下的同名地点应被允许：%v", err)
	}

	observer, err := sql.Open("sqlite", dbPath)
	if err != nil {
		migrated.Close()
		t.Fatal(err)
	}
	var dataVersionBeforeReopen int
	if err := observer.QueryRow(`PRAGMA data_version`).Scan(&dataVersionBeforeReopen); err != nil {
		t.Fatal(err)
	}
	if err := migrated.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dbPath, mk)
	if err != nil {
		observer.Close()
		t.Fatalf("重复打开升级后的库失败：%v", err)
	}
	var dataVersionAfterReopen int
	if err := observer.QueryRow(`PRAGMA data_version`).Scan(&dataVersionAfterReopen); err != nil {
		t.Fatal(err)
	}
	if dataVersionAfterReopen != dataVersionBeforeReopen {
		reopened.Close()
		observer.Close()
		t.Fatalf("重复打开仍改库：data_version %d -> %d", dataVersionBeforeReopen, dataVersionAfterReopen)
	}
	if got := deviceSiteID(t, reopened, "多网卡机"); got != "site-office" {
		reopened.Close()
		observer.Close()
		t.Fatalf("重复打开后机器地点被重算：got %q", got)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if err := observer.Close(); err != nil {
		t.Fatal(err)
	}
}
