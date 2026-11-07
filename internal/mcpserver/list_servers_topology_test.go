package mcpserver

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// TestListServersTopologySurvivesProfileSnapshotRoundTrip 走权威端导出、客
// 户端缓存导入、两边的 ListServersForProfile 三个现有外部接口，覆盖拓扑
// 裁剪与新增的 site、networks、device 字段。
func TestListServersTopologySurvivesProfileSnapshotRoundTrip(t *testing.T) {
	source := newStore(t)
	profileID, err := source.AddProfile("agent-profile")
	if err != nil {
		t.Fatal(err)
	}
	laptopTokenID, _, err := source.AddCacheToken("laptop", profileID)
	if err != nil {
		t.Fatal(err)
	}
	phoneTokenID, _, err := source.AddCacheToken("phone", profileID)
	if err != nil {
		t.Fatal(err)
	}

	for _, site := range []struct{ name, note string }{
		{"家A", "家里"},
		{"公司B", "办公室"},
	} {
		if _, err := source.AddSite(site.name, site.note); err != nil {
			t.Fatal(err)
		}
	}
	networks := []models.Network{
		{Name: "家里物理网", Type: models.NetworkPhysical, SiteName: "家A"},
		{Name: "公司物理网", Type: models.NetworkPhysical, SiteName: "公司B"},
		{Name: "虚拟B", Type: models.NetworkVirtual},
		{Name: "虚拟A", Type: models.NetworkVirtual},
	}
	for _, network := range networks {
		if _, err := source.AddNetwork(network); err != nil {
			t.Fatal(err)
		}
	}

	authorizedDevice, err := source.AddDevice("授权机器", "", "phone")
	if err != nil {
		t.Fatal(err)
	}
	unauthorizedDevice, err := source.AddDevice("未授权机器", "", "")
	if err != nil {
		t.Fatal(err)
	}
	unmanagedDevice, err := source.AddDevice("不受管NAS", "", "")
	if err != nil {
		t.Fatal(err)
	}
	laptopDevice, err := source.AddDevice("发起客户端", "", "laptop")
	if err != nil {
		t.Fatal(err)
	}

	gpuResult, err := source.AddServerWithCredentialsToDevice(&models.Server{
		Name: "gpu", Host: "192.0.2.10", Port: 22, User: "deploy",
	}, nil, nil, "授权机器")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.AddServerWithCredentialsToDevice(&models.Server{
		Name: "hidden-sibling", Host: "192.0.2.11", Port: 22, User: "deploy",
	}, nil, nil, "授权机器"); err != nil {
		t.Fatal(err)
	}
	if _, err := source.AddServerWithCredentialsToDevice(&models.Server{
		Name: "forbidden", Host: "192.0.2.12", Port: 22, User: "deploy",
	}, nil, nil, "未授权机器"); err != nil {
		t.Fatal(err)
	}
	legacyID, err := source.AddServer(&models.Server{
		Name: "legacy", Host: "192.0.2.13", Port: 22, User: "deploy",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := source.GrantServers(profileID, []string{gpuResult.ServerID, legacyID}); err != nil {
		t.Fatal(err)
	}

	memberships := []struct {
		device, network, address string
	}{
		{"授权机器", "公司物理网", "198.51.100.10"},
		{"授权机器", "家里物理网", "198.51.100.11"},
		{"授权机器", "虚拟B", "198.51.100.12"},
		{"授权机器", "虚拟A", "198.51.100.13"},
		{"未授权机器", "家里物理网", "198.51.100.20"},
		{"不受管NAS", "公司物理网", "198.51.100.30"},
		{"发起客户端", "家里物理网", "198.51.100.40"},
	}
	for _, membership := range memberships {
		if _, err := source.AddNetworkMembership(
			membership.device, membership.network, membership.address,
		); err != nil {
			t.Fatal(err)
		}
	}
	fullSnapshot, err := source.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	devicesForLayout := []string{
		authorizedDevice.ID, unauthorizedDevice.ID, unmanagedDevice.ID, laptopDevice.ID,
	}
	for i, deviceID := range devicesForLayout {
		fullSnapshot.DeviceLayoutCoordinates = append(
			fullSnapshot.DeviceLayoutCoordinates,
			store.SnapshotDeviceLayoutCoordinate{
				ViewKey: "主视图", DeviceID: deviceID,
				X: float64(i), CreatedAt: 1000, UpdatedAt: 1000,
			},
		)
	}
	// 布局坐标没有公开写入口；通过全量快照导入是 owner 备份的现有外部路径。
	source = newStore(t)
	if err := source.ImportSnapshot(fullSnapshot); err != nil {
		t.Fatalf("seed authoritative store with layout coordinates: %v", err)
	}

	fullSnapshot, err = source.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	clientSnapshot, err := source.ExportSnapshotForProfile(profileID, false, laptopTokenID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(clientSnapshot.Sites, fullSnapshot.Sites) ||
		!reflect.DeepEqual(clientSnapshot.Networks, fullSnapshot.Networks) {
		t.Fatalf("client snapshot must carry every site and network:\nsites: %+v\nnetworks: %+v",
			clientSnapshot.Sites, clientSnapshot.Networks)
	}
	clientServersByName := make(map[string]store.SnapshotServer, len(clientSnapshot.Servers))
	for _, server := range clientSnapshot.Servers {
		clientServersByName[server.Name] = server
	}
	if len(clientServersByName) != 2 {
		t.Fatalf("client servers = %+v, want only gpu and legacy", clientSnapshot.Servers)
	}
	if clientServersByName["gpu"].DeviceID != authorizedDevice.ID {
		t.Fatalf("gpu device reference = %q, want %q",
			clientServersByName["gpu"].DeviceID, authorizedDevice.ID)
	}

	target := newStore(t)
	if err := target.ImportSnapshotForClientCache(clientSnapshot); err != nil {
		t.Fatalf("import client cache snapshot: %v", err)
	}
	targetDevices, err := target.ListDevices(store.DeviceFilter{})
	if err != nil {
		t.Fatal(err)
	}
	gotDevices := make(map[string]store.SnapshotDevice, len(targetDevices))
	for _, device := range targetDevices {
		gotDevices[device.Name] = store.SnapshotDevice{
			ID: device.ID, Name: device.Name, Note: device.Note,
			ClientTokenID: device.ClientTokenID, ClientName: device.ClientName,
		}
	}
	wantDevices := map[string]store.SnapshotDevice{
		"授权机器":   {ID: authorizedDevice.ID, Name: "授权机器"},
		"不受管NAS": {ID: unmanagedDevice.ID, Name: "不受管NAS"},
		"发起客户端": {
			ID: laptopDevice.ID, Name: "发起客户端",
			ClientTokenID: laptopTokenID, ClientName: "laptop",
		},
	}
	if !reflect.DeepEqual(gotDevices, wantDevices) {
		t.Fatalf("client devices = %+v, want %+v", gotDevices, wantDevices)
	}
	if gotDevices["授权机器"].ClientTokenID == phoneTokenID {
		t.Fatalf("another client instance association must be cleared: %+v", gotDevices["授权机器"])
	}

	targetSnapshot, err := target.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	targetMemberships := make(map[string][]string)
	for _, membership := range targetSnapshot.NetworkMemberships {
		targetMemberships[membership.DeviceID] = append(
			targetMemberships[membership.DeviceID], membership.NetworkID,
		)
	}
	if len(targetMemberships) != 3 ||
		len(targetMemberships[authorizedDevice.ID]) != 4 ||
		len(targetMemberships[unmanagedDevice.ID]) != 1 ||
		len(targetMemberships[laptopDevice.ID]) != 1 {
		t.Fatalf("client network memberships = %+v, want only rows for delivered devices", targetMemberships)
	}
	if len(targetSnapshot.DeviceLayoutCoordinates) != 3 {
		t.Fatalf("client layout coordinates = %+v, want only delivered devices",
			targetSnapshot.DeviceLayoutCoordinates)
	}
	auditRows, err := target.AuditRows(100)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range auditRows {
		if row.Action == "device.migrate" {
			t.Fatalf("client cache import must not run device-generation migration: %+v", row)
		}
	}

	assertTopologyList := func(t *testing.T, st *store.Store) []ServerInfo {
		t.Helper()
		servers, err := ListServersForProfile(st, profileID)
		if err != nil {
			t.Fatal(err)
		}
		serversByName := make(map[string]ServerInfo, len(servers))
		for _, server := range servers {
			serversByName[server.Name] = server
		}
		if len(serversByName) != 2 {
			t.Fatalf("list servers = %+v, want gpu and legacy", servers)
		}
		gpu := serversByName["gpu"]
		if gpu.Device != "授权机器" {
			t.Fatalf("gpu device = %q, want 授权机器", gpu.Device)
		}
		if gpu.Site != "公司B、家A" {
			t.Fatalf("gpu site = %q, want 公司B、家A", gpu.Site)
		}
		wantNetworks := []string{"公司物理网", "家里物理网", "虚拟A", "虚拟B"}
		if !reflect.DeepEqual(gpu.Networks, wantNetworks) {
			t.Fatalf("gpu networks = %+v, want %+v", gpu.Networks, wantNetworks)
		}
		legacy := serversByName["legacy"]
		if legacy.Device != "" || legacy.Site != "" || legacy.Networks == nil {
			t.Fatalf("legacy topology fields = device %q site %q networks %+v, want empty and non-nil",
				legacy.Device, legacy.Site, legacy.Networks)
		}
		encoded, err := json.Marshal(servers)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(encoded), `"networks":[]`) {
			t.Fatalf("legacy server must encode networks as an empty array: %s", encoded)
		}
		for _, address := range []string{"198.51.100.10", "198.51.100.11", "198.51.100.12", "198.51.100.13"} {
			if strings.Contains(string(encoded), address) {
				t.Fatalf("list_servers must not leak topology addresses: %s", encoded)
			}
		}
		return servers
	}
	assertTopologyList(t, source)
	clientServers := assertTopologyList(t, target)
	clientServerInfos := make(map[string]ServerInfo, len(clientServers))
	for _, server := range clientServers {
		clientServerInfos[server.Name] = server
	}
	if !reflect.DeepEqual(clientServerInfos["gpu"], ServerInfo{
		ID: gpuResult.ServerID, Name: "gpu", Host: "hidden", User: "deploy",
		Device: "授权机器", Site: "公司B、家A",
		Networks: []string{"公司物理网", "家里物理网", "虚拟A", "虚拟B"},
		Tags:     []string{},
	}) {
		t.Fatalf("client gpu server info = %+v", clientServerInfos["gpu"])
	}
}
