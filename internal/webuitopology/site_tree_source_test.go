package webuitopology

// 多级地点在 Web UI 拓扑数据源里的呈现测试：权威存储与客户端快照两条
// 来源都必须输出完整地点路径与机器地点不一致提示。

import (
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

func seedSiteTreeVault(t *testing.T) (*store.Store, *store.Snapshot) {
	t.Helper()
	dir := t.TempDir()
	masterKey, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "store.db"), masterKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	company, err := st.AddSiteWithParent("公司", "", "")
	if err != nil {
		t.Fatal(err)
	}
	factory, err := st.AddSiteWithParent("一厂区", "", company.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSiteWithParent("3 号楼", "", factory.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSiteWithParent("二厂区", "", company.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{
		Name: "办公网", Type: models.NetworkPhysical, SiteName: "公司 / 一厂区",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDeviceAtSite("工作站", "", "", "公司 / 二厂区"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDeviceAtSite("机房机器", "", "", "公司 / 一厂区 / 3 号楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership("工作站", "办公网", "192.0.2.10"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership("机房机器", "办公网", "192.0.2.11"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := st.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	return st, snapshot
}

func siteByPath(t *testing.T, data *webui.TopologyData, path string) webui.TopologySite {
	t.Helper()
	for _, site := range data.Sites {
		if site.LocationPath == path {
			return site
		}
	}
	t.Fatalf("拓扑数据里找不到地点 %s：%+v", path, data.Sites)
	return webui.TopologySite{}
}

func assertLocationTreeData(t *testing.T, data *webui.TopologyData) {
	t.Helper()
	company := siteByPath(t, data, "公司")
	factory := siteByPath(t, data, "公司 / 一厂区")
	building := siteByPath(t, data, "公司 / 一厂区 / 3 号楼")
	if company.ParentID != "" || factory.ParentID != company.ID || building.ParentID != factory.ID {
		t.Fatalf("地点数据缺上级关系：%+v", data.Sites)
	}

	var office webui.TopologyNetwork
	found := false
	for _, network := range data.Networks {
		if network.Name == "办公网" {
			office = network
			found = true
		}
	}
	if !found || office.LocationPath != "公司 / 一厂区" {
		t.Fatalf("网络应输出覆盖地点的完整路径：%+v", data.Networks)
	}

	workstation := deviceByName(t, data, "工作站")
	if workstation.SiteID == "" || workstation.LocationPath != "公司 / 二厂区" {
		t.Fatalf("机器应输出直接登记的地点：%+v", workstation)
	}
	if len(workstation.LocationMismatches) != 1 {
		t.Fatalf("机器地点与网络覆盖地点不一致时应返回一条提示：%+v", workstation)
	}
	mismatch := workstation.LocationMismatches[0]
	if mismatch.NetworkName != "办公网" ||
		mismatch.DeviceLocationPath != "公司 / 二厂区" ||
		mismatch.NetworkLocationPath != "公司 / 一厂区" {
		t.Fatalf("地点不一致提示的路径错误：%+v", mismatch)
	}

	serverRoom := deviceByName(t, data, "机房机器")
	if serverRoom.LocationPath != "公司 / 一厂区 / 3 号楼" || len(serverRoom.LocationMismatches) != 0 {
		t.Fatalf("机器地点被网络覆盖地点覆盖时不应提示不一致：%+v", serverRoom)
	}
}

// TestStoreSourceReturnsLocationTree 覆盖权威存储来源的完整地点路径与不
// 一致提示字段。
func TestStoreSourceReturnsLocationTree(t *testing.T) {
	st, _ := seedSiteTreeVault(t)
	data, err := FromStore(st).Topology("", "self")
	if err != nil {
		t.Fatal(err)
	}
	assertLocationTreeData(t, data)
}

// TestSnapshotSourceReturnsLocationTree 覆盖客户端快照来源的同一套地点路
// 径与不一致提示，保证客户端角色看到相同语义。
func TestSnapshotSourceReturnsLocationTree(t *testing.T) {
	_, snapshot := seedSiteTreeVault(t)
	source := FromClientSnapshot(func(string) (*store.Snapshot, error) {
		return snapshot, nil
	})
	data, err := source.Topology("", "self")
	if err != nil {
		t.Fatal(err)
	}
	assertLocationTreeData(t, data)
}
