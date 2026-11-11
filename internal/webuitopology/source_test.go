package webuitopology

// 拓扑来源适配器的测试：从权威 vault 与客户端本地快照读出同一套 Web UI
// 展示数据，断言只看返回的公开数据结构，不断言数据库内部结构。

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

// seedVault 生成覆盖三视图所需的拓扑数据：两个站点的物理局域网、一个虚
// 拟局域网、本机设备、不受管设备、客户端设备、共享存储与既有布局坐标。
// 布局坐标目前只随备份形态写入，因此造库结尾用全量快照导入一次，保持生
// 产路径写入，不给存储层新开测试专用后门。
func seedVault(t *testing.T) (*store.Store, string, string, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SSHMGR_STORE", filepath.Join(dir, "store.db"))
	masterKey, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatalf("generate master key: %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "store.db"), masterKey)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if _, err := st.AddSite("家A", "物理场所"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSite("机房", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSite("云站点", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{
		Name: "家A局域网", Type: models.NetworkPhysical, SiteName: "家A", Note: "家里的网络",
		NetworkRange: "192.168.1.0/24", BandwidthMbps: 1000, Metered: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "机房局域网", Type: models.NetworkPhysical, SiteName: "机房"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "云内网", Type: models.NetworkPhysical, SiteName: "云站点"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "虚拟网", Type: models.NetworkVirtual, Note: "跨站点"}); err != nil {
		t.Fatal(err)
	}

	credential, err := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("password")})
	if err != nil {
		t.Fatal(err)
	}
	workstation, err := st.AddDevice("工作站", "主力机器", "")
	if err != nil {
		t.Fatal(err)
	}
	webID, err := st.AddServer(&models.Server{
		Name: "web-1", Host: "192.0.2.10", Port: 2202, User: "owner", Role: "网站",
		DeviceID: workstation.ID, AuthMethod: models.AuthPassword, CredentialID: credential,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddServer(&models.Server{
		Name: "web-1-admin", Host: "203.0.113.9", Port: 22, User: "admin",
		DeviceID: workstation.ID, AuthMethod: models.AuthPassword, CredentialID: credential,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("NAS", "照片与备份", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("机房机器", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: "db-1", Host: "203.0.113.5", Port: 22, User: "deploy",
	}, &models.Credential{Type: models.CredPassword, Secret: []byte("password")}, nil, "机房机器"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("云主机", "", ""); err != nil {
		t.Fatal(err)
	}

	if _, err := st.AddNetworkMembership("工作站", "家A局域网", "192.168.1.20"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership("工作站", "虚拟网", "100.64.0.20"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership("NAS", "家A局域网", "192.168.1.30"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership("NAS", "虚拟网", "100.64.0.30"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership("机房机器", "机房局域网", "10.0.0.5"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership("机房机器", "虚拟网", "100.64.0.5"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership("云主机", "云内网", "10.88.0.7"); err != nil {
		t.Fatal(err)
	}

	profileID, err := st.AddProfile("运维")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{webID}); err != nil {
		t.Fatal(err)
	}
	tokenID, _, err := st.AddCacheToken("laptop-client", profileID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("笔记本", "", "laptop-client"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership("笔记本", "虚拟网", "100.64.0.9"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecretWithEnvironmentName("NAS_PASSWORD", "NAS 登录密码", []byte("nas-login-secret"), "", store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSharedStorage(store.SharedStorageInput{
		Name: "家NAS共享", Device: "NAS", User: "backup", PasswordSecret: "NAS_PASSWORD", Drive: "N",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}

	snapshot, err := st.ExportSnapshot()
	if err != nil {
		t.Fatalf("export snapshot: %v", err)
	}
	snapshot.DeviceLayoutCoordinates = append(snapshot.DeviceLayoutCoordinates, store.SnapshotDeviceLayoutCoordinate{
		ViewKey: "physical", DeviceID: workstation.ID, X: 12.5, Y: -3, CreatedAt: 1000, UpdatedAt: 1001,
	})
	importedDir := t.TempDir()
	importedPath := filepath.Join(importedDir, "store.db")
	t.Setenv("SSHMGR_STORE", importedPath)
	importedKey, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatalf("generate imported key: %v", err)
	}
	imported, err := store.Open(importedPath, importedKey)
	if err != nil {
		t.Fatalf("open imported store: %v", err)
	}
	if err := imported.ImportSnapshot(snapshot); err != nil {
		t.Fatalf("import snapshot: %v", err)
	}
	t.Cleanup(func() { _ = imported.Close() })
	if err := roles.Save(roles.State{Role: roles.RoleStandalone, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	if err := roles.SetLocalDeviceID(workstation.ID); err != nil {
		t.Fatal(err)
	}
	return imported, importedDir, profileID, tokenID
}

func deviceByName(t *testing.T, data *webui.TopologyData, name string) webui.TopologyDevice {
	t.Helper()
	for _, device := range data.Devices {
		if device.Name == name {
			return device
		}
	}
	t.Fatalf("拓扑数据里找不到设备 %s：%+v", name, data.Devices)
	return webui.TopologyDevice{}
}

func TestStoreSourceReturnsTopologyForWritableRole(t *testing.T) {
	st, _, _, _ := seedVault(t)
	source := FromStore(st)

	data, err := source.Topology("", "self")
	if err != nil {
		t.Fatalf("read topology: %v", err)
	}

	if len(data.Sites) != 3 || data.Sites[0].Name != "云站点" || data.Sites[1].Name != "家A" {
		t.Fatalf("站点数据或排序错误：%+v", data.Sites)
	}
	networkByName := map[string]webui.TopologyNetwork{}
	for _, network := range data.Networks {
		networkByName[network.Name] = network
	}
	home := networkByName["家A局域网"]
	if home.Type != "physical" || home.SiteName != "家A" || home.NetworkRange != "192.168.1.0/24" ||
		home.BandwidthMbps != 1000 || !home.Metered || home.Note != "家里的网络" {
		t.Fatalf("物理局域网字段错误：%+v", home)
	}
	virtual := networkByName["虚拟网"]
	if virtual.Type != "virtual" || virtual.SiteName != "" || virtual.Metered {
		t.Fatalf("虚拟局域网字段错误：%+v", virtual)
	}

	workstation := deviceByName(t, data, "工作站")
	if workstation.Kind != "server" || workstation.Note != "主力机器" || len(workstation.Servers) != 2 {
		t.Fatalf("工作站设备数据错误：%+v", workstation)
	}
	if workstation.Servers[0].Name != "web-1" || workstation.Servers[0].User != "owner" ||
		workstation.Servers[0].Port != 2202 || workstation.Servers[0].Pinned {
		t.Fatalf("工作站服务器摘要错误：%+v", workstation.Servers)
	}
	wantCandidates := []webui.TopologyCandidate{
		{Address: "192.168.1.20", NetworkName: "家A局域网", Source: "physical"},
		{Address: "100.64.0.20", NetworkName: "虚拟网", Source: "virtual"},
		{Address: "192.0.2.10", Source: "fallback", ServerName: "web-1"},
		{Address: "203.0.113.9", Source: "fallback", ServerName: "web-1-admin"},
	}
	if len(workstation.Candidates) != len(wantCandidates) {
		t.Fatalf("工作站候选地址 = %+v，want %+v", workstation.Candidates, wantCandidates)
	}
	for i, want := range wantCandidates {
		if workstation.Candidates[i] != want {
			t.Fatalf("工作站候选地址第 %d 项 = %+v，want %+v", i, workstation.Candidates[i], want)
		}
	}
	if workstation.CandidateNotice != "" {
		t.Fatalf("有共同网络时不应提示只剩余兜底：%q", workstation.CandidateNotice)
	}

	nas := deviceByName(t, data, "NAS")
	if nas.Kind != "unmanaged" {
		t.Fatalf("不受管设备类型错误：%+v", nas)
	}
	if len(nas.SharedStorages) != 1 || nas.SharedStorages[0].Name != "家NAS共享" {
		t.Fatalf("NAS 共享存储错误：%+v", nas.SharedStorages)
	}
	if len(nas.Candidates) != 2 || nas.Candidates[0].Source != "physical" || nas.Candidates[1].Source != "virtual" {
		t.Fatalf("NAS 候选地址应物理局域网优先：%+v", nas.Candidates)
	}

	laptop := deviceByName(t, data, "笔记本")
	if laptop.Kind != "client" || laptop.ClientName != "laptop-client" {
		t.Fatalf("客户端设备数据错误：%+v", laptop)
	}
	rackDevice := deviceByName(t, data, "机房机器")
	if len(rackDevice.Candidates) != 2 || rackDevice.Candidates[0].Source != "virtual" ||
		rackDevice.Candidates[1].Source != "fallback" || rackDevice.Candidates[1].ServerName != "db-1" ||
		rackDevice.Candidates[1].Address != "203.0.113.5" {
		t.Fatalf("机房机器候选地址错误：%+v", rackDevice.Candidates)
	}

	cloud := deviceByName(t, data, "云主机")
	if len(cloud.Candidates) != 0 || cloud.CandidateNotice == "" {
		t.Fatalf("无共同网络设备应有提示且没有候选地址：%+v", cloud)
	}

	if !data.LocalDevice.Known || data.LocalDevice.DeviceName != "工作站" || data.LocalDevice.Reason != "" {
		t.Fatalf("本机设备解析错误：%+v", data.LocalDevice)
	}
	if len(data.LayoutCoordinates) != 1 || data.LayoutCoordinates[0].ViewKey != "physical" ||
		data.LayoutCoordinates[0].DeviceID != workstation.ID || data.LayoutCoordinates[0].X != 12.5 {
		t.Fatalf("布局坐标错误：%+v", data.LayoutCoordinates)
	}
	if len(data.Memberships) != 8 {
		t.Fatalf("网络成员数量 = %d，应为 8：%+v", len(data.Memberships), data.Memberships)
	}
}

func TestStoreSourceOrdersCandidatesBySelectedServerOrigin(t *testing.T) {
	st, _, _, _ := seedVault(t)
	servers, err := st.ListServers()
	if err != nil {
		t.Fatal(err)
	}
	dbServerID := ""
	for _, server := range servers {
		if server.Name == "db-1" {
			dbServerID = server.ID
		}
	}
	if dbServerID == "" {
		t.Fatal("种子库里找不到 db-1 服务器条目")
	}

	data, err := FromStore(st).Topology("", dbServerID)
	if err != nil {
		t.Fatalf("按服务器起点读取拓扑失败：%v", err)
	}
	workstation := deviceByName(t, data, "工作站")
	if len(workstation.Candidates) != 3 ||
		workstation.Candidates[0].Source != "virtual" ||
		workstation.Candidates[1].Source != "fallback" ||
		workstation.Candidates[2].Source != "fallback" {
		t.Fatalf("服务器起点应按起点的设备网络成员排序候选地址：%+v", workstation.Candidates)
	}
	nas := deviceByName(t, data, "NAS")
	if len(nas.Candidates) != 1 || nas.Candidates[0].Source != "virtual" ||
		nas.Candidates[0].NetworkName != "虚拟网" {
		t.Fatalf("服务器起点与 NAS 的共同虚拟局域网应排在候选地址里：%+v", nas.Candidates)
	}
	cloud := deviceByName(t, data, "云主机")
	if len(cloud.Candidates) != 0 || !strings.Contains(cloud.CandidateNotice, "机房机器") {
		t.Fatalf("服务器起点没有共同网络时应给面向该起点的提示：%+v", cloud)
	}
}

func TestClientSnapshotSourceCropsToAuthorizedDevices(t *testing.T) {
	st, _, profileID, tokenID := seedVault(t)
	snapshot, err := st.ExportSnapshotForProfile(profileID, false, tokenID)
	if err != nil {
		t.Fatalf("export client snapshot: %v", err)
	}
	cacheDir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", cacheDir)
	var laptopID string
	for _, device := range snapshot.Devices {
		if device.Name == "笔记本" {
			laptopID = device.ID
		}
	}
	meta, err := json.Marshal(map[string]string{"device_id": laptopID})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "cache.meta.json"), meta, 0o600); err != nil {
		t.Fatal(err)
	}

	var gotInstance string
	source := FromClientSnapshot(func(instance string) (*store.Snapshot, error) {
		gotInstance = instance
		return snapshot, nil
	})
	data, err := source.Topology("lab", "self")
	if err != nil {
		t.Fatalf("read client topology: %v", err)
	}
	if gotInstance != "lab" {
		t.Fatalf("客户端实例应传给快照读取函数，实际 %q", gotInstance)
	}
	if len(data.Sites) != 3 || len(data.Networks) != 4 {
		t.Fatalf("客户端应看到全部站点与网络：sites=%d networks=%d", len(data.Sites), len(data.Networks))
	}
	workstation := deviceByName(t, data, "工作站")
	if len(workstation.Servers) != 1 || workstation.Servers[0].Name != "web-1" {
		t.Fatalf("客户端按授权裁剪服务器摘要：%+v", workstation.Servers)
	}
	deviceNames := make(map[string]bool, len(data.Devices))
	for _, device := range data.Devices {
		deviceNames[device.Name] = true
	}
	for _, allowed := range []string{"工作站", "NAS", "云主机", "笔记本"} {
		if !deviceNames[allowed] {
			t.Fatalf("客户端快照应包含设备 %s，实际：%+v", allowed, deviceNames)
		}
	}
	for _, denied := range []string{"机房机器"} {
		if deviceNames[denied] {
			t.Fatalf("未授权设备 %s 不应出现在客户端拓扑里", denied)
		}
	}
	if !data.LocalDevice.Known || data.LocalDevice.DeviceName != "笔记本" {
		t.Fatalf("客户端本机设备解析错误：%+v", data.LocalDevice)
	}
	workstationID := ""
	for _, device := range snapshot.Devices {
		if device.Name == "工作站" {
			workstationID = device.ID
		}
	}
	if len(data.LayoutCoordinates) != 1 || data.LayoutCoordinates[0].DeviceID != workstationID {
		t.Fatalf("客户端只应收到可见设备的坐标：%+v", data.LayoutCoordinates)
	}
	if laptopID == workstationID {
		t.Fatal("测试数据里笔记本与工作站不应是同一台设备")
	}
}

func TestStoreSourceReportsUnknownLocalDevice(t *testing.T) {
	st, dir, _, _ := seedVault(t)
	if err := os.Remove(filepath.Join(dir, "local-device.json")); err != nil {
		t.Fatalf("remove local device setting: %v", err)
	}
	data, err := FromStore(st).Topology("", "self")
	if err != nil {
		t.Fatalf("read topology: %v", err)
	}
	if data.LocalDevice.Known || data.LocalDevice.Reason == "" {
		t.Fatalf("未设本机设备应返回原因：%+v", data.LocalDevice)
	}
	cloud := deviceByName(t, data, "云主机")
	if cloud.CandidateNotice == "" {
		t.Fatalf("本机设备未知时候选地址应有可读提示：%+v", cloud)
	}
}

func TestSourcesReturnContainerLayoutCoordinates(t *testing.T) {
	st, _, profileID, tokenID := seedVault(t)
	siteID := ""
	for _, site := range mustListSites(t, st) {
		if site.Name == "家A" {
			siteID = site.ID
		}
	}
	networkID := ""
	for _, network := range mustListNetworks(t, st) {
		if network.Name == "虚拟网" {
			networkID = network.ID
		}
	}
	layout := store.ViewLayoutInput{
		Containers: []store.ContainerLayoutInput{
			{EntityType: store.LayoutEntitySite, EntityID: siteID, X: 21, Y: 31},
			{EntityType: store.LayoutEntityNetwork, EntityID: networkID, X: 41, Y: 51},
		},
	}
	if err := st.SaveViewLayout("virtual", layout, "127.0.0.1"); err != nil {
		t.Fatalf("保存站点区域与网络分组框布局失败：%v", err)
	}

	data, err := FromStore(st).Topology("", "self")
	if err != nil {
		t.Fatal(err)
	}
	if len(data.ContainerLayoutCoordinates) != 2 {
		t.Fatalf("权威端来源应返回站点区域或网络分组框坐标：%+v", data.ContainerLayoutCoordinates)
	}
	byKey := make(map[string]webui.TopologyContainerLayoutCoordinate, 2)
	for _, coordinate := range data.ContainerLayoutCoordinates {
		byKey[coordinate.EntityType+"/"+coordinate.EntityID] = coordinate
	}
	if byKey["site/"+siteID].X != 21 || byKey["site/"+siteID].Y != 31 ||
		byKey["network/"+networkID].X != 41 || byKey["network/"+networkID].Y != 51 {
		t.Fatalf("权威端来源的站点区域或网络分组框坐标错误：%+v", data.ContainerLayoutCoordinates)
	}

	snapshot, err := st.ExportSnapshotForProfile(profileID, false, tokenID)
	if err != nil {
		t.Fatal(err)
	}
	clientData, err := FromClientSnapshot(func(string) (*store.Snapshot, error) {
		return snapshot, nil
	}).Topology("", "self")
	if err != nil {
		t.Fatal(err)
	}
	if len(clientData.ContainerLayoutCoordinates) != 2 {
		t.Fatalf("客户端来源应返回快照里的站点区域或网络分组框坐标：%+v", clientData.ContainerLayoutCoordinates)
	}
}

func TestTopologyWriterSavesAndResetsLayout(t *testing.T) {
	st, _, _, _ := seedVault(t)
	writer := WriterFromStore(st)
	siteID := ""
	for _, site := range mustListSites(t, st) {
		if site.Name == "家A" {
			siteID = site.ID
		}
	}
	device := deviceByName2(t, st, "工作站")
	save := webui.TopologyLayoutSave{
		ViewKey: "physical",
		Devices: []webui.TopologyLayoutCoordinate{{DeviceID: device.ID, X: 7.5, Y: -2}},
		Containers: []webui.TopologyContainerLayoutCoordinate{
			{EntityType: "site", EntityID: siteID, X: 100, Y: 200},
		},
	}
	if err := writer.SaveTopologyLayout(save, webui.TopologyWriteSource{RemoteAddress: "192.0.2.9"}); err != nil {
		t.Fatalf("保存布局失败：%v", err)
	}
	deviceRows, err := st.ListDeviceLayoutCoordinates()
	if err != nil {
		t.Fatal(err)
	}
	containerRows, err := st.ListContainerLayoutCoordinates()
	if err != nil {
		t.Fatal(err)
	}
	if len(deviceRows) != 1 || deviceRows[0].ViewKey != "physical" || deviceRows[0].X != 7.5 {
		t.Fatalf("适配层保存后的设备坐标错误：%+v", deviceRows)
	}
	if len(containerRows) != 1 || containerRows[0].EntityType != "site" || containerRows[0].X != 100 {
		t.Fatalf("适配层保存后的站点区域或网络分组框坐标错误：%+v", containerRows)
	}
	audits := layoutAuditRows(t, st)
	if len(audits) != 1 || audits[0].Action != "topology.layout.save" ||
		audits[0].Source != store.AuditSourceWebUI || audits[0].SourceIP != "192.0.2.9" {
		t.Fatalf("适配层保存布局的审计行错误：%+v", audits)
	}

	if err := writer.ResetTopologyLayout("physical", webui.TopologyWriteSource{RemoteAddress: "192.0.2.9"}); err != nil {
		t.Fatalf("重置布局失败：%v", err)
	}
	deviceRows, err = st.ListDeviceLayoutCoordinates()
	if err != nil {
		t.Fatal(err)
	}
	containerRows, err = st.ListContainerLayoutCoordinates()
	if err != nil {
		t.Fatal(err)
	}
	if len(deviceRows) != 0 || len(containerRows) != 0 {
		t.Fatalf("重置后应清空该视图坐标：%+v %+v", deviceRows, containerRows)
	}
	audits = layoutAuditRows(t, st)
	if len(audits) != 2 || audits[0].Action != "topology.layout.reset" ||
		audits[0].Source != store.AuditSourceWebUI {
		t.Fatalf("适配层重置布局的审计行错误：%+v", audits)
	}

	// 引用不存在的设备按接口约定换算成内容非法错误。
	invalid := webui.TopologyLayoutSave{
		ViewKey: "physical",
		Devices: []webui.TopologyLayoutCoordinate{{DeviceID: "missing-device", X: 1, Y: 2}},
	}
	err = writer.SaveTopologyLayout(invalid, webui.TopologyWriteSource{})
	if !errors.Is(err, webui.ErrTopologyLayoutInvalid) {
		t.Fatalf("引用不存在设备的错误应为内容非法，实际：%v", err)
	}
}

func mustListSites(t *testing.T, st *store.Store) []*models.Site {
	t.Helper()
	sites, err := st.ListSites()
	if err != nil {
		t.Fatal(err)
	}
	return sites
}

func mustListNetworks(t *testing.T, st *store.Store) []*models.Network {
	t.Helper()
	networks, err := st.ListNetworks(store.NetworkFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return networks
}

func deviceByName2(t *testing.T, st *store.Store, name string) *models.Device {
	t.Helper()
	devices, err := st.ListDevices(store.DeviceFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, device := range devices {
		if device.Name == name {
			return device
		}
	}
	t.Fatalf("库里找不到设备 %s", name)
	return nil
}

func layoutAuditRows(t *testing.T, st *store.Store) []store.AuditRow {
	t.Helper()
	rows, err := st.AuditRows(100)
	if err != nil {
		t.Fatal(err)
	}
	var matched []store.AuditRow
	for _, row := range rows {
		if strings.HasPrefix(row.Action, "topology.layout.") {
			matched = append(matched, row)
		}
	}
	return matched
}
