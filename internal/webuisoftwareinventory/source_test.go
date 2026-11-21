package webuisoftwareinventory

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/softwareinventory"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

func newTestStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	key, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "vault.db")
	st, err := store.Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, path
}

func addTestDevice(t *testing.T, st *store.Store, name string) {
	t.Helper()
	if _, err := st.AddDevice(name, "", ""); err != nil {
		t.Fatal(err)
	}
	credentialID, err := st.SetCredential(&models.Credential{
		Type: models.CredPassword, Secret: []byte("test-password"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: name + "-ssh", Host: "192.0.2.10", Port: 22, User: "owner",
		AuthMethod: models.AuthPassword, CredentialID: credentialID,
	}, nil, nil, name); err != nil {
		t.Fatal(err)
	}
}

// TestSourceBuildsDeviceCardsAndSearchFromUnifiedQueryFilters 钉住首页的
// 公共数据契约：设备摘要来自统一实测查询，搜索复用精确版本、地点与网络
// 条件，声明数据不进入页面。
func TestSourceBuildsDeviceCardsAndSearchFromUnifiedQueryFilters(t *testing.T) {
	st, _ := newTestStore(t)
	for _, site := range []struct{ name, parent string }{
		{name: "公司"},
		{name: "总部机房", parent: "公司"},
		{name: "分部机房"},
	} {
		if _, err := st.AddSiteWithParent(site.name, "", site.parent); err != nil {
			t.Fatal(err)
		}
	}
	for _, network := range []models.Network{
		{Name: "总部物理局域网", Type: models.NetworkPhysical, SiteName: "公司"},
		{Name: "跨地点虚拟局域网", Type: models.NetworkVirtual},
		{Name: "分部物理局域网", Type: models.NetworkPhysical, SiteName: "分部机房"},
	} {
		if _, err := st.AddNetwork(network); err != nil {
			t.Fatal(err)
		}
	}
	credentialID, err := st.SetCredential(&models.Credential{
		Type: models.CredPassword, Secret: []byte("test-password"),
	})
	if err != nil {
		t.Fatal(err)
	}
	servers := map[string]string{}
	for _, device := range []struct{ name, site, network string }{
		{name: "工作站", site: "总部机房", network: "总部物理局域网"},
		{name: "虚拟网设备", site: "公司", network: "跨地点虚拟局域网"},
		{name: "分部失败设备", site: "分部机房", network: "分部物理局域网"},
	} {
		if _, err := st.AddDeviceAtSite(device.name, "", "", device.site); err != nil {
			t.Fatal(err)
		}
		if _, err := st.AddNetworkMembership(device.name, device.network, "192.0.2.10"); err != nil {
			t.Fatal(err)
		}
		server, err := st.AddServerWithCredentialsToDevice(&models.Server{
			Name: device.name + "-ssh", Host: "192.0.2.10", Port: 22, User: "owner",
			AuthMethod: models.AuthPassword, CredentialID: credentialID,
		}, nil, nil, device.name)
		if err != nil {
			t.Fatal(err)
		}
		servers[device.name] = server.ServerID
	}
	if _, err := st.AddDeviceAtSite("不受管存储", "", "", "分部机房"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	seed := func(device string, collectedAt time.Time, items []models.SoftwareObservedItem) {
		t.Helper()
		st.NowFn = func() time.Time { return collectedAt }
		if err := st.ReplaceSoftwareObservedItems(device, store.SoftwareObservedWrite{
			Items: items, ServerID: servers[device],
			CommandSetVersion: softwareinventory.POSIXCommandSetVersion, SystemType: "ubuntu",
		}, store.CLISource()); err != nil {
			t.Fatal(err)
		}
	}
	seed("工作站", now.Add(-10*time.Hour), []models.SoftwareObservedItem{
		{Category: models.SoftwareCategoryOperatingSystem, Name: "Ubuntu", Version: "24.04.1 LTS"},
		{Category: models.SoftwareCategoryDriverFirmware, Name: "Linux 内核", Version: "6.8.0-49-generic"},
		{Category: models.SoftwareCategoryServiceProgram, Name: "nginx", Version: "1.24.0"},
		{Category: models.SoftwareCategoryUnidentified, Name: "unknown-gateway", ExecutablePath: "/opt/gateway/gateway"},
		{Category: models.SoftwareCategorySystemPackage, Name: "openssl", Version: "3.0.2-0ubuntu1.18"},
		{Category: models.SoftwareCategorySystemPackage, Name: "base-files", Version: "13.5ubuntu3"},
	})
	seed("虚拟网设备", now.Add(-10*time.Hour), []models.SoftwareObservedItem{
		{Category: models.SoftwareCategoryServiceProgram, Name: "OpenSSL", Version: "3.0.2"},
	})
	seed("分部失败设备", now.Add(-9*24*time.Hour), []models.SoftwareObservedItem{
		{Category: models.SoftwareCategorySystemPackage, Name: "openssl", Version: "3.0.2-0ubuntu1.18"},
	})
	st.NowFn = func() time.Time { return now.Add(-time.Hour) }
	if err := st.RecordSoftwareCollectionFailure(
		"分部失败设备", "连接超时，端口不通", servers["分部失败设备"], false, store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	st.NowFn = nil
	source := FromOpener(func() (*store.Store, func(), error) { return st, func() {}, nil })

	devices, err := source.SoftwareInventoryDevices(webui.SoftwareInventoryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 4 {
		t.Fatalf("设备卡片应包含全部设备：%+v", devices)
	}
	var workstation, failed, unmanaged *webui.SoftwareInventoryDeviceSummary
	for i := range devices {
		switch devices[i].Name {
		case "工作站":
			workstation = &devices[i]
		case "分部失败设备":
			failed = &devices[i]
		case "不受管存储":
			unmanaged = &devices[i]
		}
	}
	if workstation == nil || failed == nil || unmanaged == nil {
		t.Fatalf("设备卡片缺失：%+v", devices)
	}
	if workstation.SystemName != "Ubuntu" || workstation.SystemVersion != "24.04.1 LTS" ||
		workstation.KernelName != "Linux 内核" || workstation.KernelVersion != "6.8.0-49-generic" ||
		workstation.ImportantSoftwareCount != 4 || workstation.AllSoftwareCount != 6 ||
		workstation.UnidentifiedProcessCount != 1 ||
		len(workstation.StatusLabels) != 1 || workstation.StatusLabels[0] != "正常" {
		t.Fatalf("工作站设备摘要不符：%+v", workstation)
	}
	if len(failed.StatusLabels) != 2 || failed.StatusLabels[0] != "采集失败" ||
		failed.StatusLabels[1] != "清单过期" || failed.Status.LastFailureReason != "连接超时，端口不通" {
		t.Fatalf("采集失败且过期的设备摘要不符：%+v", failed)
	}
	if len(unmanaged.StatusLabels) != 1 || unmanaged.StatusLabels[0] != "不采集（不受管设备）" {
		t.Fatalf("不受管设备状态不符：%+v", unmanaged)
	}

	inCompany, err := source.SoftwareInventoryDevices(webui.SoftwareInventoryFilter{Site: "公司"})
	if err != nil {
		t.Fatal(err)
	}
	if len(inCompany) != 2 || inCompany[0].Name != "工作站" || inCompany[1].Name != "虚拟网设备" {
		t.Fatalf("上级地点应包含下级地点设备：%+v", inCompany)
	}
	inPhysical, err := source.SoftwareInventoryDevices(webui.SoftwareInventoryFilter{Network: "总部物理局域网"})
	if err != nil {
		t.Fatal(err)
	}
	if len(inPhysical) != 1 || inPhysical[0].Name != "工作站" {
		t.Fatalf("物理局域网应按成员筛选设备：%+v", inPhysical)
	}
	inVirtual, err := source.SoftwareInventoryDevices(webui.SoftwareInventoryFilter{Network: "跨地点虚拟局域网"})
	if err != nil {
		t.Fatal(err)
	}
	if len(inVirtual) != 1 || inVirtual[0].Name != "虚拟网设备" {
		t.Fatalf("虚拟局域网应按成员筛选设备：%+v", inVirtual)
	}

	exact, err := source.SearchSoftwareInventory(webui.SoftwareInventoryFilter{
		Name: "openssl", ExactVersion: "3.0.2-0ubuntu1.18",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(exact) != 2 || exact[0].DeviceName != "分部失败设备" || exact[1].DeviceName != "工作站" {
		t.Fatalf("精确版本搜索应只返回拥有对应条目的设备：%+v", exact)
	}
	if len(exact[1].Rows) != 1 || exact[1].Rows[0].Item.Name != "openssl" ||
		exact[1].Rows[0].Item.CategoryLabel != "系统包" {
		t.Fatalf("精确版本搜索应返回逐字相等的系统包：%+v", exact[1])
	}
	exactInCompany, err := source.SearchSoftwareInventory(webui.SoftwareInventoryFilter{
		Name: "openssl", ExactVersion: "3.0.2-0ubuntu1.18", Site: "公司",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(exactInCompany) != 1 || exactInCompany[0].DeviceName != "工作站" {
		t.Fatalf("地点条件应同时缩小搜索结果：%+v", exactInCompany)
	}
	withoutVersion, err := source.SearchSoftwareInventory(webui.SoftwareInventoryFilter{Name: "openssl"})
	if err != nil {
		t.Fatal(err)
	}
	if len(withoutVersion) != 3 || withoutVersion[0].DeviceName != "分部失败设备" ||
		withoutVersion[1].DeviceName != "工作站" || withoutVersion[2].DeviceName != "虚拟网设备" {
		t.Fatalf("版本留空表示不筛版本：%+v", withoutVersion)
	}
	linux, err := source.SearchSoftwareInventory(webui.SoftwareInventoryFilter{Name: "Linux"})
	if err != nil {
		t.Fatal(err)
	}
	if len(linux) != 1 || len(linux[0].Rows) != 1 ||
		linux[0].Rows[0].Item.SearchCategoryLabel != "操作系统与内核" {
		t.Fatalf("搜索默认视图应把操作系统与内核合成一个突出类别：%+v", linux)
	}
}

// TestSourceExportsCSVFromUnifiedQuery 验证导出接口复用统一查询条件，并
// 输出能被标准解析器读回的平铺 CSV 总表。
func TestSourceExportsCSVFromUnifiedQuery(t *testing.T) {
	st, _ := newTestStore(t)
	if _, err := st.AddSite("公司", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSite("分部机房", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{
		Name: "总部物理局域网", Type: models.NetworkPhysical, SiteName: "公司",
	}); err != nil {
		t.Fatal(err)
	}
	for _, device := range []struct{ name, site string }{
		{name: "工作站", site: "公司"}, {name: "分部工作站", site: "分部机房"},
	} {
		if _, err := st.AddDeviceAtSite(device.name, "", "", device.site); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.AddNetworkMembership("工作站", "总部物理局域网", "192.0.2.10"); err != nil {
		t.Fatal(err)
	}
	seed := func(device string, items ...models.SoftwareObservedItem) {
		t.Helper()
		if err := st.ReplaceSoftwareObservedItems(device, store.SoftwareObservedWrite{
			Items: items, CommandSetVersion: "posix-v2", SystemType: "ubuntu",
		}, store.CLISource()); err != nil {
			t.Fatal(err)
		}
	}
	seed("工作站",
		models.SoftwareObservedItem{
			Category: softwareinventory.CategoryServiceProgram,
			Name:     "nginx", Version: "1.25.3",
			ListeningPorts: []softwareinventory.ListeningPort{{
				Address: "0.0.0.0", Port: 80, Protocol: "tcp",
				Exposure: softwareinventory.ExposureAllAddresses,
			}},
		},
		models.SoftwareObservedItem{
			Category: softwareinventory.CategorySystemPackage,
			Name:     "libc6", Version: "2.39-0ubuntu8",
		},
	)
	seed("分部工作站", models.SoftwareObservedItem{
		Category: softwareinventory.CategoryServiceProgram,
		Name:     "nginx", Version: "1.24.0",
	})
	source := FromOpener(func() (*store.Store, func(), error) { return st, func() {}, nil })

	blob, err := source.ExportSoftwareInventoryCSV(webui.SoftwareInventoryFilter{
		Name: "nginx", ExactVersion: "1.25.3", Site: "公司", Network: "总部物理局域网",
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := readExportCSV(t, blob)
	if len(rows) != 2 || rows[1][0] != "工作站" || rows[1][1] != "nginx" ||
		rows[1][3] != "" || rows[1][6] != "监听所有地址" {
		t.Fatalf("当前筛选导出行 = %#v", rows)
	}

	blob, err = source.ExportSoftwareInventoryCSV(webui.SoftwareInventoryFilter{
		Device: "分部工作站",
	})
	if err != nil {
		t.Fatal(err)
	}
	rows = readExportCSV(t, blob)
	if len(rows) != 2 || rows[1][0] != "分部工作站" || rows[1][2] != "1.24.0" {
		t.Fatalf("单台设备导出行 = %#v", rows)
	}
}

func readExportCSV(t *testing.T, blob []byte) [][]string {
	t.Helper()
	if !bytes.HasPrefix(blob, []byte("\uFEFF")) {
		t.Fatalf("导出缺少 UTF-8 字节顺序标记：%q", blob)
	}
	rows, err := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(blob, []byte("\uFEFF")))).ReadAll()
	if err != nil {
		t.Fatalf("导出必须能用标准 CSV 解析器读回：%v\n%q", err, blob)
	}
	if len(rows) == 0 || strings.Join(rows[0], "|") !=
		"设备|软件名|版本|缺版本|类别|监听端口|暴露级别|采集时间" {
		t.Fatalf("导出表头 = %#v", rows)
	}
	return rows
}

// TestSourceBuildsDeviceViewFromUnifiedQuery 验证适配层把统一查询、采集
// 状态与变化历史合成 Web UI 页面数据，并把系统包整理成默认折叠分组。
func TestSourceBuildsDeviceViewFromUnifiedQuery(t *testing.T) {
	st, _ := newTestStore(t)
	addTestDevice(t, st, "工作站")
	manager := softwareinventory.New(st)
	confirmedAt := time.Unix(1770000000, 0)
	st.NowFn = func() time.Time { return confirmedAt }
	if _, err := manager.AddDeclaration(softwareinventory.Declaration{
		Device: "工作站", Category: softwareinventory.CategoryServiceProgram,
		Name: "自建网关", Version: "1.2.3+build.7",
		ListeningPorts: []softwareinventory.ListeningPort{{
			Address: "10.0.0.5", Port: 8443, Protocol: "tcp",
			Exposure: softwareinventory.ExposureLocalAreaNetwork,
		}},
	}, softwareinventory.CLISource()); err != nil {
		t.Fatal(err)
	}
	server, err := st.GetServerByName("工作站-ssh")
	if err != nil || server == nil {
		t.Fatalf("取采集用服务器条目失败：%v %+v", err, server)
	}
	collectedAt := confirmedAt.Add(time.Hour)
	st.NowFn = func() time.Time { return collectedAt }
	if err := st.ReplaceSoftwareObservedItems("工作站", store.SoftwareObservedWrite{
		Items: []models.SoftwareObservedItem{
			{Category: models.SoftwareCategoryOperatingSystem, Name: "Ubuntu", Version: "24.04.1 LTS"},
			{Category: models.SoftwareCategoryServiceProgram, Name: "nginx", Version: "1.24.0"},
			{Category: models.SoftwareCategoryContainerImage, Name: "postgres", Version: "16.4",
				BelongsTo: "Docker", ContainerTag: "16-alpine", ContainerRunning: true},
			{Category: models.SoftwareCategoryDriverFirmware, Name: "Linux 内核", Version: "6.8.0-49-generic"},
			{Category: models.SoftwareCategoryDriverFirmware, Name: "NVIDIA 显卡驱动", Version: "550.120"},
			{Category: models.SoftwareCategorySystemPackage, Name: "curl", Version: "8.9.1-1"},
			{Category: models.SoftwareCategorySystemPackage, Name: "openssl", Version: "3.0.2"},
			{Category: models.SoftwareCategoryUnidentified, Name: "unknown-gateway",
				ProcessName: "gateway", ExecutablePath: "/opt/gateway/gateway", ProcessID: 8123,
				ListeningPorts: []models.SoftwareListeningPort{{
					Address: "127.0.0.1", Port: 8443, Protocol: "tcp",
					Exposure: models.SoftwareExposureLocal,
				}}},
		},
		ServerID: server.ID, CommandSetVersion: "linux-v1", SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	profileID, err := st.AddProfile("运维")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{server.ID}); err != nil {
		t.Fatal(err)
	}
	clientID, _, err := st.AddCacheToken("laptop-client", profileID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSoftwareCollectSudo(server.Name, true, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSoftwareCollectionCollector(
		"工作站", "laptop-client", server.Name, store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	st.NowFn = func() time.Time { return collectedAt.Add(time.Hour) }
	source := FromOpener(func() (*store.Store, func(), error) { return st, func() {}, nil })

	device, err := source.SoftwareInventoryDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if device.Name != "工作站" || !device.Status.Managed {
		t.Fatalf("设备页面基础数据不符：%+v", device)
	}
	if device.Status.Collector != clientID || device.Status.CollectorName != "laptop-client" ||
		device.Status.CollectorLabel != "客户端实例 laptop-client" ||
		device.Status.PreferredServerEntry != server.Name {
		t.Fatalf("设备页面采集执行者状态不符：%+v", device.Status)
	}
	if device.Summary.SystemName != "Ubuntu" || device.Summary.SystemVersion != "24.04.1 LTS" ||
		device.Summary.KernelName != "Linux 内核" || device.Summary.KernelVersion != "6.8.0-49-generic" ||
		device.Summary.ImportantSoftwareCount != 6 || device.Summary.AllSoftwareCount != 8 ||
		device.Summary.UnidentifiedProcessCount != 1 {
		t.Fatalf("设备页面摘要应与首页卡片同口径：%+v", device.Summary)
	}
	cards, err := source.SoftwareInventoryDevices(webui.SoftwareInventoryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 1 || !reflect.DeepEqual(cards[0], device.Summary) {
		t.Fatalf("首页卡片与设备详情的摘要与状态口径应完全一致：%+v %+v", cards, device.Summary)
	}
	labels := make([]string, 0, len(device.Groups))
	for _, group := range device.Groups {
		labels = append(labels, group.CategoryLabel)
	}
	wantedLabels := []string{
		"操作系统与内核", "服务程序", "容器镜像", "驱动与固件", "系统包", "未识别进程",
	}
	if strings.Join(labels, "|") != strings.Join(wantedLabels, "|") {
		t.Fatalf("设备页面分组顺序不符：%v", labels)
	}
	if len(device.Groups[0].Items) != 2 || device.Groups[0].Items[0].Name != "Ubuntu" ||
		device.Groups[0].Items[1].Name != "Linux 内核" {
		t.Fatalf("操作系统与内核应合并为一个分组：%+v", device.Groups[0].Items)
	}
	if device.Groups[0].CategoryCode != softwareinventory.DisplayCategoryOperatingSystemKernel.Code() ||
		device.Groups[0].Items[0].CategoryCode != softwareinventory.DisplayCategoryOperatingSystemKernel.Code() ||
		device.Groups[0].Items[1].CategoryCode != softwareinventory.DisplayCategoryOperatingSystemKernel.Code() ||
		device.Groups[0].Items[1].SearchCategoryLabel != "操作系统与内核" {
		t.Fatalf("页面分组与搜索类别应使用领域展示类别：%+v", device.Groups[0])
	}
	if len(device.Groups[3].Items) != 1 || device.Groups[3].Items[0].Name != "NVIDIA 显卡驱动" {
		t.Fatalf("内核不应重复出现在驱动与固件分组：%+v", device.Groups[3].Items)
	}
	if !device.Groups[4].SystemPackages || device.Groups[4].SystemPackageCount != 2 {
		t.Fatalf("系统包分组应折叠并给出数量：%+v", device.Groups[4])
	}
	if device.Groups[4].CategoryCode != softwareinventory.DisplayCategorySystemPackage.Code() ||
		device.Groups[4].Items[0].CategoryCode != softwareinventory.DisplayCategorySystemPackage.Code() {
		t.Fatalf("系统包分组与条目应带稳定类别代号：%+v", device.Groups[4])
	}
	process := device.Groups[5].Items[0]
	if process.ProcessName != "gateway" || process.ExecutablePath != "/opt/gateway/gateway" ||
		process.ProcessID != 8123 || process.ListeningPorts[0].ExposureLabel != "仅本机" {
		t.Fatalf("未识别进程条目应保留进程可得信息：%+v", process)
	}
	if len(device.Advanced.ClientInstances) != 1 ||
		device.Advanced.ClientInstances[0].Name != "laptop-client" ||
		device.Advanced.ClientInstances[0].ProfileName != "运维" {
		t.Fatalf("高级区客户端实例数据不符：%+v", device.Advanced.ClientInstances)
	}
	if len(device.Advanced.ServerEntries) != 1 ||
		device.Advanced.ServerEntries[0].Name != server.Name ||
		!device.Advanced.ServerEntries[0].SudoEnabled {
		t.Fatalf("高级区采集用服务器条目数据不符：%+v", device.Advanced.ServerEntries)
	}
	for _, group := range device.Groups {
		for _, item := range group.Items {
			if item.Name == "自建网关" || item.Source == "声明" {
				t.Fatalf("声明不应进入设备页面：%+v", item)
			}
		}
	}
	declarations, err := st.SearchSoftwareDeclarations("", softwareinventory.AllDevicesVisible())
	if err != nil || len(declarations) != 1 || declarations[0].Version != "1.2.3+build.7" {
		t.Fatalf("声明应保留在存储里：%v %+v", err, declarations)
	}

	if _, err := source.SoftwareInventoryDevice("不存在"); err == nil {
		t.Fatal("不存在的设备应返回错误")
	} else {
		var missing *webui.SoftwareInventoryDeviceNotFoundError
		if !errors.As(err, &missing) {
			t.Fatalf("不存在的设备应返回页面层可识别的错误：%v", err)
		}
	}
}

// TestSourceRequestsCollectionForClientCollector 验证客户端实例作为采集
// 执行者时，立即采集只登记请求并写 Web UI 来源审计。
func TestSourceRequestsCollectionForClientCollector(t *testing.T) {
	st, path := newTestStore(t)
	addTestDevice(t, st, "工作站")
	firstAt := time.Unix(1770000000, 0)
	st.NowFn = func() time.Time { return firstAt }
	server, err := st.GetServerByName("工作站-ssh")
	if err != nil || server == nil {
		t.Fatalf("取服务器条目失败：%v %+v", err, server)
	}
	if err := st.ReplaceSoftwareObservedItems("工作站", store.SoftwareObservedWrite{
		Items: []models.SoftwareObservedItem{{
			Category: models.SoftwareCategoryOperatingSystem, Name: "Ubuntu", Version: "22.04.5",
		}}, ServerID: server.ID, CommandSetVersion: softwareinventory.POSIXCommandSetVersion, SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(
		`UPDATE software_collection_status SET collector='laptop-client' WHERE device_id=?`, server.DeviceID,
	); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	source := FromOpener(func() (*store.Store, func(), error) { return st, func() {}, nil })

	result, err := source.RequestSoftwareInventoryCollection(
		context.Background(), "工作站",
		webui.SoftwareInventoryWriteSource{RemoteAddress: "198.51.100.9"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Requested {
		t.Fatalf("客户端采集执行者应只登记请求：%+v", result)
	}
	statuses, err := softwareinventory.New(st).CollectionStatuses(softwareinventory.AllDevicesVisible(), "工作站")
	if err != nil || len(statuses) != 1 {
		t.Fatalf("读取采集状态失败：%v %+v", err, statuses)
	}
	if !statuses[0].PendingImmediate || statuses[0].Collector != "laptop-client" {
		t.Fatalf("立即采集请求应保持客户端采集执行者并置为待执行：%+v", statuses[0])
	}
	auditRows, err := st.QueryAudit(store.AuditFilter{
		Actions: []string{"software-collect-request"}, Source: store.AuditSourceWebUI,
		SourceIP: "198.51.100.9", Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(auditRows) != 1 {
		t.Fatalf("立即采集请求应写一条 Web UI 来源审计：%+v", auditRows)
	}
}

// TestSourceBuildsTodosFromDomainResult 验证待办页适配层直接使用领域模块
// 的三类待办结果，字段不经过第二套计算。
func TestSourceBuildsTodosFromDomainResult(t *testing.T) {
	st, _ := newTestStore(t)
	addTestDevice(t, st, "工作站")
	server, err := st.GetServerByName("工作站-ssh")
	if err != nil || server == nil {
		t.Fatalf("取服务器条目失败：%v %+v", err, server)
	}
	if err := st.ReplaceSoftwareObservedItems("工作站", store.SoftwareObservedWrite{
		Items: []models.SoftwareObservedItem{{
			StableKey: "unidentified-8443", Category: models.SoftwareCategoryUnidentified,
			Name: "unknown-gateway", ProcessName: "gateway", ExecutablePath: "/opt/gateway/gateway",
			ListeningPorts: []models.SoftwareListeningPort{{
				Address: "0.0.0.0", Port: 8443, Protocol: "tcp",
				Exposure: models.SoftwareExposureAllAddresses,
			}},
		}},
		ServerID: server.ID, CommandSetVersion: "linux-v1", SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	source := FromOpener(func() (*store.Store, func(), error) { return st, func() {}, nil })

	todos, err := source.SoftwareInventoryTodos()
	if err != nil {
		t.Fatal(err)
	}
	if len(todos.PendingExposure) != 1 || todos.PendingExposure[0].DeviceName != "工作站" ||
		len(todos.PendingExposure[0].Ports) != 1 ||
		todos.PendingExposure[0].Ports[0].ObservedStableKey != "unidentified-8443" ||
		todos.PendingExposure[0].Ports[0].ExposureLabel != "监听所有地址" {
		t.Fatalf("待确认暴露页面数据不符：%+v", todos.PendingExposure)
	}
	if len(todos.UnidentifiedProcesses) != 1 ||
		todos.UnidentifiedProcesses[0].ProcessName != "gateway" ||
		todos.UnidentifiedProcesses[0].ExecutablePath != "/opt/gateway/gateway" ||
		len(todos.UnidentifiedProcesses[0].ListeningPorts) != 1 {
		t.Fatalf("未识别进程页面数据不符：%+v", todos.UnidentifiedProcesses)
	}
	if len(todos.Mismatches) != 0 || len(todos.StaleDevices) != 0 {
		t.Fatalf("没有对应数据的待办不应凭空出现：%+v", todos)
	}
}

// TestSourceDeviceAdvancedSettingsAndSudo 验证设备页高级采集设置：登记
// 校验错误透出、成功登记改变设备视图、sudo 开关写入 Web UI 来源审计。
func TestSourceDeviceAdvancedSettingsAndSudo(t *testing.T) {
	st, _ := newTestStore(t)
	addTestDevice(t, st, "工作站")
	server, err := st.GetServerByName("工作站-ssh")
	if err != nil || server == nil {
		t.Fatalf("取服务器条目失败：%v %+v", err, server)
	}
	profileID, err := st.AddProfile("运维")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{server.ID}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AddCacheToken("laptop-client", profileID); err != nil {
		t.Fatal(err)
	}
	source := FromOpener(func() (*store.Store, func(), error) { return st, func() {}, nil })

	device, err := source.SoftwareInventoryDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if len(device.Advanced.ClientInstances) != 1 ||
		device.Advanced.ClientInstances[0].Name != "laptop-client" ||
		device.Advanced.ClientInstances[0].ProfileName != "运维" {
		t.Fatalf("设备页高级区客户端实例数据不符：%+v", device.Advanced.ClientInstances)
	}
	if len(device.Advanced.ServerEntries) != 1 ||
		device.Advanced.ServerEntries[0].Name != server.Name ||
		device.Advanced.ServerEntries[0].SudoEnabled {
		t.Fatalf("设备页高级区采集用服务器条目数据不符：%+v", device.Advanced.ServerEntries)
	}
	err = source.SetSoftwareInventoryCollector(webui.SoftwareInventoryCollectorForm{
		Device: "工作站", Collector: "不存在的客户端", ServerEntry: server.Name,
	}, webui.SoftwareInventoryWriteSource{RemoteAddress: "192.0.2.7"})
	var formErr *webui.SoftwareInventoryFormError
	if !errors.As(err, &formErr) || !strings.Contains(formErr.Message, "不是有效的活动设备码") {
		t.Fatalf("采集执行者登记校验错误应可在页面展示：%v", err)
	}
	if err := source.SetSoftwareInventoryCollector(webui.SoftwareInventoryCollectorForm{
		Device: "工作站", Collector: "laptop-client", ServerEntry: server.Name,
	}, webui.SoftwareInventoryWriteSource{RemoteAddress: "192.0.2.7"}); err != nil {
		t.Fatal(err)
	}
	device, err = source.SoftwareInventoryDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if device.Status.CollectorLabel != "客户端实例 laptop-client" ||
		device.Status.PreferredServerEntry != server.Name {
		t.Fatalf("登记后的设备页采集执行者状态不符：%+v", device.Status)
	}
	if err := source.SetSoftwareInventorySudo(
		server.Name, true, webui.SoftwareInventoryWriteSource{RemoteAddress: "192.0.2.7"},
	); err != nil {
		t.Fatal(err)
	}
	device, err = source.SoftwareInventoryDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if !device.Advanced.ServerEntries[0].SudoEnabled {
		t.Fatalf("sudo 开关打开后的设备页数据不符：%+v", device.Advanced.ServerEntries[0])
	}
	rows, err := st.QueryAudit(store.AuditFilter{
		Actions: []string{"software-collect-collector", "software-collect-sudo"},
		Source:  store.AuditSourceWebUI, SourceIP: "192.0.2.7", Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("采集执行者与 sudo 开关应各写一条 Web UI 来源审计：%+v", rows)
	}
}
