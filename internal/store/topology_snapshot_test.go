package store

// 网络拓扑随全量备份与导出保存、不随客户端快照下发的防回归测试。

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// TestTopologySnapshotRoundTripAndClientDelivery 覆盖三件事：全量快照携带
// 站点与网络并在导入后原样恢复；profile 裁剪快照不携带这两段；登记拓扑
// 数据不改变快照修订号。
func TestTopologySnapshotRoundTripAndClientDelivery(t *testing.T) {
	dir := t.TempDir()
	masterKey, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("generate master key: %v", err)
	}
	st, err := Open(filepath.Join(dir, "source.db"), masterKey)
	if err != nil {
		t.Fatalf("open source store: %v", err)
	}
	defer st.Close()

	profileID, err := st.AddProfile("client-profile")
	if err != nil {
		t.Fatal(err)
	}
	clientTokenID, _, err := st.AddCacheToken("laptop", profileID)
	if err != nil {
		t.Fatal(err)
	}
	revisionBeforeTopology, err := st.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSite("家A", "物理场所"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{
		Name: "家A局域网", Type: models.NetworkPhysical, SiteName: "家A", Note: "家里的网络",
		NetworkRange: "192.168.1.0/24", BandwidthMbps: 1000, Metered: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{
		Name: "虚拟网", Type: models.NetworkVirtual, Note: "跨站点网络",
	}); err != nil {
		t.Fatal(err)
	}
	device, err := st.AddDevice("工作站", "主力机器", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	membership, err := st.AddNetworkMembership("工作站", "家A局域网", " 192.168.1.20 ")
	if err != nil {
		t.Fatal(err)
	}
	revisionAfterTopology, err := st.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if revisionAfterTopology != revisionBeforeTopology {
		t.Fatalf("topology writes must not change snapshot revision: before=%d after=%d",
			revisionBeforeTopology, revisionAfterTopology)
	}

	snapshot, err := st.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Sites) != 1 {
		t.Fatalf("full snapshot sites = %+v, want one site", snapshot.Sites)
	}
	site := snapshot.Sites[0]
	if site.Name != "家A" || site.Note != "物理场所" {
		t.Fatalf("full snapshot site = %+v", site)
	}
	if len(snapshot.Networks) != 2 {
		t.Fatalf("full snapshot networks = %+v, want two networks", snapshot.Networks)
	}
	gotHome := false
	gotVirtual := false
	for _, network := range snapshot.Networks {
		switch network.Name {
		case "家A局域网":
			gotHome = network.Type == string(models.NetworkPhysical) && network.SiteID == site.ID &&
				network.Note == "家里的网络" && network.CIDR == "192.168.1.0/24" &&
				network.BandwidthMbps == 1000 && network.Metered
		case "虚拟网":
			gotVirtual = network.Type == string(models.NetworkVirtual) && network.SiteID == "" &&
				network.Note == "跨站点网络" && !network.Metered
		}
	}
	if !gotHome || !gotVirtual {
		t.Fatalf("full snapshot network fields are wrong: %+v", snapshot.Networks)
	}
	if len(snapshot.Devices) != 1 {
		t.Fatalf("full snapshot devices = %+v, want one device", snapshot.Devices)
	}
	gotDevice := snapshot.Devices[0]
	if gotDevice.ID != device.ID || gotDevice.Name != "工作站" || gotDevice.Note != "主力机器" ||
		gotDevice.ClientTokenID != clientTokenID || gotDevice.ClientName != "laptop" {
		t.Fatalf("full snapshot device fields are wrong: %+v", gotDevice)
	}
	if len(snapshot.NetworkMemberships) != 1 {
		t.Fatalf("full snapshot network memberships = %+v, want one row", snapshot.NetworkMemberships)
	}
	gotMembership := snapshot.NetworkMemberships[0]
	if gotMembership.DeviceID != device.ID || gotMembership.NetworkID != membership.NetworkID ||
		gotMembership.Address != "192.168.1.20" {
		t.Fatalf("full snapshot network membership fields are wrong: %+v", gotMembership)
	}
	clientSnapshot, err := st.ExportSnapshotForProfile(profileID, false)
	if err != nil {
		t.Fatal(err)
	}
	if clientSnapshot.Sites != nil || clientSnapshot.Networks != nil ||
		clientSnapshot.Devices != nil || clientSnapshot.NetworkMemberships != nil {
		t.Fatalf("client snapshot must not carry topology sections: sites=%+v networks=%+v devices=%+v memberships=%+v",
			clientSnapshot.Sites, clientSnapshot.Networks, clientSnapshot.Devices, clientSnapshot.NetworkMemberships)
	}
	encoded, err := json.Marshal(clientSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{`"sites"`, `"networks"`, `"devices"`, `"network_memberships"`} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("client snapshot JSON must not contain %s:\n%s", forbidden, encoded)
		}
	}

	targetKey, err := GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	target, err := Open(filepath.Join(dir, "target.db"), targetKey)
	if err != nil {
		t.Fatalf("open target store: %v", err)
	}
	defer target.Close()
	if err := target.ImportSnapshot(snapshot); err != nil {
		t.Fatalf("import topology snapshot: %v", err)
	}
	restored, err := target.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshot.Sites, restored.Sites) || !reflect.DeepEqual(snapshot.Networks, restored.Networks) ||
		!reflect.DeepEqual(snapshot.Devices, restored.Devices) ||
		!reflect.DeepEqual(snapshot.NetworkMemberships, restored.NetworkMemberships) {
		t.Fatalf("topology sections changed after import:\nsites: %+v -> %+v\nnetworks: %+v -> %+v\ndevices: %+v -> %+v\nmemberships: %+v -> %+v",
			snapshot.Sites, restored.Sites, snapshot.Networks, restored.Networks,
			snapshot.Devices, restored.Devices,
			snapshot.NetworkMemberships, restored.NetworkMemberships)
	}
}
