package softwareinventory

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// seedQueryFixture 造四台设备：受管甲、受管乙挂有服务器条目且都在 profile
// 授权里，受管丙的服务器条目不在授权里，网络存储是不受管设备。声明继续
// 写入以证明数据保留；查询使用同名的实测条目覆盖名称、暴露级别、类别与
// 系统包分支。
func seedQueryFixture(t *testing.T) (*Manager, Visibility) {
	t.Helper()
	manager, st := newTestManager(t)
	mustDevice(t, st, "受管甲", true)
	mustDevice(t, st, "受管乙", true)
	mustDevice(t, st, "受管丙", true)
	mustDevice(t, st, "网络存储", false)
	declarations := []Declaration{
		{
			Device: "受管甲", Category: CategoryServiceProgram, Name: "OpenSSH",
			Version: "1:9.6p1-3ubuntu1",
			ListeningPorts: []ListeningPort{
				{Address: "127.0.0.1", Port: 22, Protocol: "tcp", Exposure: ExposureLocalMachine},
			},
		},
		{
			Device: "受管甲", Category: CategoryServiceProgram, Name: "nginx", Version: "1.25.3",
			ListeningPorts: []ListeningPort{
				{Address: "0.0.0.0", Port: 80, Protocol: "tcp", Exposure: ExposureAllAddresses},
			},
		},
		{Device: "受管甲", Category: CategorySystemPackage, Name: "libc6", Version: "2.39-0ubuntu8"},
		{
			Device: "受管乙", Category: CategoryServiceProgram, Name: "PostgreSQL", Version: "16.2",
			ListeningPorts: []ListeningPort{
				{Address: "10.0.0.5", Port: 5432, Protocol: "tcp", Exposure: ExposureAllAddresses},
			},
		},
		{Device: "网络存储", Category: CategoryOperatingSystem, Name: "NAS 系统", Version: "7.2.1"},
		{Device: "受管丙", Category: CategoryServiceProgram, Name: "Redis", Version: "7.2.4"},
	}
	for _, declaration := range declarations {
		if _, err := manager.AddDeclaration(declaration, CLISource()); err != nil {
			t.Fatal(err)
		}
	}
	observedByName := map[string][]models.SoftwareObservedItem{}
	for _, declaration := range declarations {
		ports := append([]ListeningPort(nil), declaration.ListeningPorts...)
		for i := range ports {
			if ports[i].Exposure != ExposureLocalMachine &&
				ports[i].Exposure != ExposureSpecificAddress &&
				ports[i].Exposure != ExposureAllAddresses {
				ports[i].Exposure = ExposureAllAddresses
			}
		}
		observedByName[declaration.Device] = append(observedByName[declaration.Device], models.SoftwareObservedItem{
			Category: declaration.Category, Name: declaration.Name, Version: declaration.Version,
			ListeningPorts: ports,
		})
	}
	for deviceName, items := range observedByName {
		if err := st.ReplaceSoftwareObservedItems(deviceName, store.SoftwareObservedWrite{
			Items: items, CommandSetVersion: "posix-v2", SystemType: "ubuntu",
		}, store.CLISource()); err != nil {
			t.Fatal(err)
		}
	}
	serverA, err := st.GetServerByName("受管甲-ssh")
	if err != nil || serverA == nil {
		t.Fatalf("取受管甲服务器条目失败：%v %+v", err, serverA)
	}
	serverB, err := st.GetServerByName("受管乙-ssh")
	if err != nil || serverB == nil {
		t.Fatalf("取受管乙服务器条目失败：%v %+v", err, serverB)
	}
	return manager, DevicesVisibleThroughServers(serverA.ID, serverB.ID)
}

func findQueryDevice(result QueryResult, name string) *QueryDeviceResult {
	for i := range result.Devices {
		if result.Devices[i].DeviceName == name {
			return &result.Devices[i]
		}
	}
	return nil
}

func findQueryItem(result QueryResult, value string) *QueryItem {
	for i := range result.Devices {
		for j := range result.Devices[i].Items {
			if result.Devices[i].Items[j].Name == value ||
				result.Devices[i].Items[j].Version == value {
				return &result.Devices[i].Items[j]
			}
		}
	}
	return nil
}

// TestQueryKeepsDevicesWithoutItemsAndFailureReason 验证没有条目过滤时，没
// 有条目的受管设备仍返回；从未成功但失败过的设备保留执行者、失败时间与原
// 因，CycloneDX 导出也标出从未采集。
func TestQueryKeepsDevicesWithoutItemsAndFailureReason(t *testing.T) {
	manager, st := newTestManager(t)
	mustDevice(t, st, "空清单工作站", true)
	mustDevice(t, st, "失败工作站", true)
	mustDevice(t, st, "范围外设备", true)
	emptyServer, err := st.GetServerByName("空清单工作站-ssh")
	if err != nil || emptyServer == nil {
		t.Fatal(err)
	}
	failedServer, err := st.GetServerByName("失败工作站-ssh")
	if err != nil || failedServer == nil {
		t.Fatal(err)
	}
	if _, err := manager.AddDeclaration(Declaration{
		Device: "范围外设备", Category: CategoryServiceProgram, Name: "Redis", Version: "7.2",
	}, CLISource()); err != nil {
		t.Fatal(err)
	}
	profile, err := st.AddProfile("空设备 profile")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profile, []string{emptyServer.ID, failedServer.ID}); err != nil {
		t.Fatal(err)
	}
	visibility := DevicesVisibleThroughServers(emptyServer.ID, failedServer.ID)
	failureAt := time.Unix(1770001200, 0)
	st.NowFn = func() time.Time { return failureAt }
	if err := st.RecordSoftwareCollectionFailureFromCollector(
		"失败工作站", models.ClientCollector("cache-token-failed"), "SSH 连接超时", failedServer.ID, false,
		store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}

	result, err := manager.Query(QueryOptions{Visibility: visibility})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Devices) != 2 {
		t.Fatalf("没有条目过滤时应返回范围内全部两台设备：%+v", result.Devices)
	}
	empty := findQueryDevice(result, "空清单工作站")
	failed := findQueryDevice(result, "失败工作站")
	if empty == nil || len(empty.Items) != 0 || !empty.Collection.NeverCollected || !empty.Collection.Stale {
		t.Fatalf("空清单设备 = %+v", empty)
	}
	if failed == nil || !failed.Collection.NeverCollected || !failed.Collection.Stale ||
		failed.Collection.Collector != "cache-token-failed" ||
		failed.Collection.LastFailureAt == nil || failed.Collection.LastFailureAt.Unix() != failureAt.Unix() ||
		failed.Collection.LastFailureReason != "SSH 连接超时" {
		t.Fatalf("从未成功设备的失败状态 = %+v", failed.Collection)
	}

	document, err := MarshalCycloneDX(result, CycloneDXExportOptions{
		GeneratedAt: failureAt.Add(time.Minute), SSHManagerVersion: "test", ProfileName: "profile",
	})
	if err != nil {
		t.Fatal(err)
	}
	value := decodeCycloneDX(t, document)
	if len(anySlice(t, value["components"], "components")) != 2 {
		t.Fatalf("导出应包含范围内所有设备：%v", value)
	}
	for _, rawDevice := range anySlice(t, value["components"], "components") {
		device := nestedMap(t, rawDevice, "设备组件")
		properties := propertiesByName(t, device)
		if properties["sshmgr:never-collected"] != "true" || properties["sshmgr:inventory-stale"] != "true" {
			t.Fatalf("从未采集的设备必须在导出中标出：%+v", device)
		}
	}
}

// TestQueryPreservesWebDisplayFields 覆盖 Web UI 需要但过滤条件不用的字
// 段：实测采集时间，以及容器镜像的所属、标签、摘要与运行状态。
func TestQueryPreservesWebDisplayFields(t *testing.T) {
	manager, st := newTestManager(t)
	mustDevice(t, st, "工作站", true)
	confirmedAt := time.Unix(1770000600, 0)
	st.NowFn = func() time.Time { return confirmedAt }
	if _, err := manager.AddDeclaration(Declaration{
		Device: "工作站", Category: CategoryServiceProgram,
		Name: "自建网关", Version: "1.2.3+build.7",
	}, CLISource()); err != nil {
		t.Fatal(err)
	}
	collectedAt := confirmedAt.Add(time.Hour)
	st.NowFn = func() time.Time { return collectedAt }
	server, err := st.GetServerByName("工作站-ssh")
	if err != nil || server == nil {
		t.Fatalf("取采集用服务器条目失败：%v %+v", err, server)
	}
	if err := st.ReplaceSoftwareObservedItems("工作站", store.SoftwareObservedWrite{
		Items: []models.SoftwareObservedItem{{
			Category: models.SoftwareCategoryContainerImage, Name: "postgres", Version: "16.4",
			BelongsTo: "Docker", ContainerTag: "16-alpine",
			ContainerDigest: "sha256:abcdef", ContainerRunning: true,
		}},
		ServerID: server.ID, CommandSetVersion: "linux-v1", SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}

	result, err := manager.Query(QueryOptions{
		Device: "工作站", IncludeSystemPackages: true, Visibility: AllDevicesVisible(),
	})
	if err != nil {
		t.Fatal(err)
	}
	device := findQueryDevice(result, "工作站")
	if device == nil || len(device.Items) != 1 {
		t.Fatalf("按设备查询结果 = %+v", result.Devices)
	}
	container := device.Items[0]
	if container.CollectedAt == nil || container.CollectedAt.Unix() != collectedAt.Unix() ||
		container.BelongsTo != "Docker" || container.ContainerTag != "16-alpine" ||
		container.ContainerDigest != "sha256:abcdef" || !container.ContainerRunning {
		t.Fatalf("容器镜像条目应保留 Web UI 展示字段：%+v", container)
	}
}

// TestQueryPreservesUnidentifiedProcessFields 钉住未识别进程在查询结果里
// 保留进程可得信息：设备详情页要展示进程名、可执行文件路径与进程号。
func TestQueryPreservesUnidentifiedProcessFields(t *testing.T) {
	manager, st := newTestManager(t)
	mustDevice(t, st, "工作站", true)
	collectedAt := time.Unix(1770000600, 0)
	st.NowFn = func() time.Time { return collectedAt }
	server, err := st.GetServerByName("工作站-ssh")
	if err != nil || server == nil {
		t.Fatalf("取采集用服务器条目失败：%v %+v", err, server)
	}
	if err := st.ReplaceSoftwareObservedItems("工作站", store.SoftwareObservedWrite{
		Items: []models.SoftwareObservedItem{{
			Category: models.SoftwareCategoryUnidentified, Name: "unknown-gateway",
			ProcessName: "gateway", ExecutablePath: "/opt/gateway/gateway", ProcessID: 8123,
			ListeningPorts: []models.SoftwareListeningPort{{
				Address: "0.0.0.0", Port: 8443, Protocol: "tcp",
				Exposure: models.SoftwareExposureAllAddresses,
			}},
		}},
		ServerID: server.ID, CommandSetVersion: "linux-v1", SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}

	result, err := manager.Query(QueryOptions{
		Device: "工作站", IncludeSystemPackages: true, Visibility: AllDevicesVisible(),
	})
	if err != nil {
		t.Fatal(err)
	}
	device := findQueryDevice(result, "工作站")
	if device == nil || len(device.Items) != 1 {
		t.Fatalf("按设备查询结果 = %+v", result.Devices)
	}
	process := device.Items[0]
	if process.Name != "unknown-gateway" || process.ProcessName != "gateway" ||
		process.ExecutablePath != "/opt/gateway/gateway" || process.ProcessID != 8123 {
		t.Fatalf("未识别进程条目应保留进程可得信息：%+v", process)
	}
}

// TestQueryFiltersAndCollectionStatus 覆盖查询工具的全部过滤条件与采集
// 状态字段。
func TestQueryFiltersAndCollectionStatus(t *testing.T) {
	manager, visibility := seedQueryFixture(t)

	// 名称子串大小写不敏感。
	result, err := manager.Query(QueryOptions{Name: "SQL", Visibility: visibility})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Devices) != 1 || result.Devices[0].DeviceName != "受管乙" {
		t.Fatalf("名称子串查询结果 = %+v", result.Devices)
	}
	item := result.Devices[0].Items[0]
	if item.Name != "PostgreSQL" || item.Source != SourceObserved || item.CollectedAt == nil {
		t.Fatalf("查询条目 = %+v", item)
	}
	if len(item.ListeningPorts) != 1 || item.ListeningPorts[0].Exposure != ExposureAllAddresses {
		t.Fatalf("查询条目监听端口 = %+v", item.ListeningPorts)
	}

	// 采集状态来自实测成功记录。
	status := result.Devices[0].Collection
	if status.NeverCollected || status.Stale || status.LastSuccessAt == nil ||
		status.LastFailureAt != nil || status.LastFailureReason != "" ||
		status.Incomplete || status.CommandSetVersion != "posix-v2" {
		t.Fatalf("实测成功后的采集状态 = %+v", status)
	}

	// 最低暴露级别：仅本机监听的 OpenSSH 不应出现。
	result, err = manager.Query(QueryOptions{
		MinimumExposure: ExposureAllAddresses, Visibility: visibility,
	})
	if err != nil {
		t.Fatal(err)
	}
	managedA := findQueryDevice(result, "受管甲")
	if managedA == nil || len(managedA.Items) != 1 || managedA.Items[0].Name != "nginx" {
		t.Fatalf("最低暴露级别查询里受管甲的结果 = %+v", result.Devices)
	}
	managedB := findQueryDevice(result, "受管乙")
	if managedB == nil || len(managedB.Items) != 1 {
		t.Fatalf("监听所有地址应满足最低暴露级别：%+v", result.Devices)
	}

	// 类别过滤。
	result, err = manager.Query(QueryOptions{
		Category: CategoryOperatingSystem, Visibility: visibility,
	})
	if err != nil {
		t.Fatal(err)
	}
	if findQueryDevice(result, "受管甲") != nil || findQueryDevice(result, "受管乙") != nil {
		t.Fatalf("类别过滤后的可见设备结果 = %+v", result.Devices)
	}

	// 系统包默认不含，显式要求后返回。
	result, err = manager.Query(QueryOptions{Device: "受管甲", Visibility: visibility})
	if err != nil {
		t.Fatal(err)
	}
	device := findQueryDevice(result, "受管甲")
	if device == nil {
		t.Fatalf("按设备查询结果 = %+v", result.Devices)
	}
	for _, got := range device.Items {
		if got.Category == CategorySystemPackage {
			t.Fatalf("系统包默认不应返回：%+v", got)
		}
	}
	result, err = manager.Query(QueryOptions{
		Device: "受管甲", IncludeSystemPackages: true, Visibility: visibility,
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, got := range findQueryDevice(result, "受管甲").Items {
		if got.Name == "libc6" {
			found = true
		}
	}
	if !found {
		t.Fatalf("显式包含系统包失败：%+v", result.Devices)
	}

	// 设备在可见范围外：不受管设备与授权外的受管设备都拒绝。
	for _, deviceName := range []string{"网络存储", "受管丙"} {
		_, err := manager.Query(QueryOptions{Device: deviceName, Visibility: visibility})
		if err == nil || !errors.Is(err, ErrDeviceNotVisible) {
			t.Fatalf("查询范围外设备 %s 应返回不可见错误，实际：%v", deviceName, err)
		}
	}

	// owner 范围能看不受管设备。
	result, err = manager.Query(QueryOptions{Name: "NAS", Visibility: AllDevicesVisible()})
	if err != nil {
		t.Fatal(err)
	}
	if findQueryDevice(result, "网络存储") == nil {
		t.Fatalf("owner 查询应包含不受管设备：%+v", result.Devices)
	}
}

// TestQueryVisibilityOmitsUnmanagedDevicesForProfile 钉住 profile 的可见
// 设备范围：不受管设备的清单绝不出现在查询结果里。
func TestQueryVisibilityOmitsUnmanagedDevicesForProfile(t *testing.T) {
	manager, visibility := seedQueryFixture(t)
	result, err := manager.Query(QueryOptions{Visibility: visibility})
	if err != nil {
		t.Fatal(err)
	}
	if findQueryDevice(result, "网络存储") != nil {
		t.Fatalf("不受管设备出现在 profile 查询结果里：%+v", result.Devices)
	}
	if findQueryDevice(result, "受管甲") == nil {
		t.Fatalf("受管设备缺失：%+v", result.Devices)
	}
}

// TestQueryObservedOnlyExactVersionLocationAndNetworkFilters 钉住本票的
// 领域契约：查询只输出实测；精确版本逐字相等；地点包含下级；网络按设备
// 成员筛选；带软件名时自动包含系统包。
func TestQueryObservedOnlyExactVersionLocationAndNetworkFilters(t *testing.T) {
	manager, st := newTestManager(t)
	if _, err := st.AddSite("公司", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSiteWithParent("总部机房", "", "公司"); err != nil {
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
	if _, err := st.AddNetwork(models.Network{
		Name: "跨地点虚拟局域网", Type: models.NetworkVirtual,
	}); err != nil {
		t.Fatal(err)
	}
	devices := []struct {
		name    string
		site    string
		network string
		version string
	}{
		{name: "虚拟网设备", site: "公司", network: "跨地点虚拟局域网", version: "3.0.2"},
		{name: "下级机房设备", site: "总部机房", network: "总部物理局域网", version: "3.0.2-0ubuntu1.18"},
		{name: "地点外设备", site: "分部机房", version: "3.0.2-0ubuntu1.18"},
	}
	for _, device := range devices {
		if _, err := st.AddDeviceAtSite(device.name, "", "", device.site); err != nil {
			t.Fatal(err)
		}
		if device.network != "" {
			if _, err := st.AddNetworkMembership(
				device.name, device.network, "192.0.2."+fmt.Sprint(len(device.name)),
			); err != nil {
				t.Fatal(err)
			}
		}
		if err := st.ReplaceSoftwareObservedItems(device.name, store.SoftwareObservedWrite{
			Items: []models.SoftwareObservedItem{
				{Category: CategoryServiceProgram, Name: "OpenSSL", Version: device.version},
				{Category: CategorySystemPackage, Name: "openssl-library", Version: device.version},
			},
			CommandSetVersion: "posix-v2", SystemType: "ubuntu",
		}, store.CLISource()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := manager.AddDeclaration(Declaration{
		Device: "下级机房设备", Category: CategoryServiceProgram,
		Name: "OpenSSL", Version: "3.0.2-0ubuntu1.99",
	}, CLISource()); err != nil {
		t.Fatal(err)
	}
	draftIDs, err := manager.SubmitDeclarationDrafts([]DeclarationDraft{{
		SoftwareDeclaration: Declaration{
			Device: "下级机房设备", Category: CategoryServiceProgram,
			Name: "手工网关", Version: "1.2.3",
		},
	}}, AllDevicesVisible(), AgentSource())
	if err != nil {
		t.Fatal(err)
	}

	exact, err := manager.Query(QueryOptions{
		Name: "openssl", ExactVersion: " 3.0.2-0ubuntu1.18 ",
		Visibility: AllDevicesVisible(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(exact.Devices) != 2 ||
		findQueryDevice(exact, "下级机房设备") == nil ||
		findQueryDevice(exact, "地点外设备") == nil ||
		findQueryDevice(exact, "虚拟网设备") != nil {
		t.Fatalf("精确版本查询结果 = %+v", exact.Devices)
	}
	for _, device := range exact.Devices {
		if len(device.Items) != 2 {
			t.Fatalf("带软件名时服务程序与系统包都应返回：%+v", device)
		}
		for _, item := range device.Items {
			if item.Source != SourceObserved || item.Version != "3.0.2-0ubuntu1.18" {
				t.Fatalf("精确版本应只返回逐字相等的实测条目：%+v", item)
			}
		}
	}

	inSite, err := manager.Query(QueryOptions{
		Name: "openssl", ExactVersion: "3.0.2-0ubuntu1.18", Site: "公司",
		Visibility: AllDevicesVisible(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(inSite.Devices) != 1 || findQueryDevice(inSite, "下级机房设备") == nil {
		t.Fatalf("上级地点应包含下级地点设备：%+v", inSite.Devices)
	}

	physical, err := manager.Query(QueryOptions{
		Name: "openssl", Network: "总部物理局域网", Visibility: AllDevicesVisible(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(physical.Devices) != 1 || findQueryDevice(physical, "下级机房设备") == nil {
		t.Fatalf("物理局域网应按设备成员筛选：%+v", physical.Devices)
	}
	virtual, err := manager.Query(QueryOptions{
		Name: "openssl", Network: "跨地点虚拟局域网", Visibility: AllDevicesVisible(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(virtual.Devices) != 1 || findQueryDevice(virtual, "虚拟网设备") == nil {
		t.Fatalf("虚拟局域网应按设备成员筛选：%+v", virtual.Devices)
	}

	otherVersion, err := manager.Query(QueryOptions{
		Name: "OpenSSL", ExactVersion: "3.0.2", Visibility: AllDevicesVisible(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(otherVersion.Devices) != 1 || findQueryDevice(otherVersion, "虚拟网设备") == nil {
		t.Fatalf("版本不同的条目不应匹配：%+v", otherVersion.Devices)
	}

	all, err := manager.Query(QueryOptions{Visibility: AllDevicesVisible()})
	if err != nil {
		t.Fatal(err)
	}
	if findQueryItem(all, "手工网关") != nil || findQueryItem(all, "3.0.2-0ubuntu1.99") != nil {
		t.Fatalf("声明与草稿不应出现在使用者可见查询里：%+v", all.Devices)
	}
	declarations, err := st.SearchSoftwareDeclarations("", AllDevicesVisible())
	if err != nil {
		t.Fatal(err)
	}
	if len(declarations) != 1 || declarations[0].Version != "3.0.2-0ubuntu1.99" {
		t.Fatalf("生效声明应保留在存储里：%+v", declarations)
	}
	drafts, err := manager.DeclarationDrafts(AllDevicesVisible())
	if err != nil {
		t.Fatal(err)
	}
	if len(drafts) != 1 || len(draftIDs) != 1 || drafts[0].ID != draftIDs[0] {
		t.Fatalf("声明草稿应保留在存储里：%+v", drafts)
	}
}

// TestQueryExactVersionIsVerbatimAndBlankMeansAny 钉住精确版本的逐字相等
// 语义：大小写不同不算相等；条件去空白后为空时不限制版本；条件存在时
// 版本为空的实测条目不匹配。
func TestQueryExactVersionIsVerbatimAndBlankMeansAny(t *testing.T) {
	manager, st := newTestManager(t)
	mustDevice(t, st, "工作站", true)
	if err := st.ReplaceSoftwareObservedItems("工作站", store.SoftwareObservedWrite{
		Items: []models.SoftwareObservedItem{
			{Category: CategoryServiceProgram, Name: "自定义网关", Version: "1.0Alpha"},
			{Category: CategoryServiceProgram, Name: "未标版本服务", Version: ""},
		},
		CommandSetVersion: "posix-v2", SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}

	differentCase, err := manager.Query(QueryOptions{
		ExactVersion: "1.0alpha", Visibility: AllDevicesVisible(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(differentCase.Devices) != 0 {
		t.Fatalf("大小写不同的版本不应匹配：%+v", differentCase.Devices)
	}

	blank, err := manager.Query(QueryOptions{
		Name: "自定义网关", ExactVersion: "   ", Visibility: AllDevicesVisible(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(blank.Devices) != 1 || findQueryItem(blank, "自定义网关") == nil {
		t.Fatalf("去空白后为空的精确版本不应限制版本：%+v", blank.Devices)
	}

	withFilter, err := manager.Query(QueryOptions{
		ExactVersion: "1.0Alpha", Visibility: AllDevicesVisible(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if findQueryItem(withFilter, "未标版本服务") != nil {
		t.Fatalf("版本为空的条目在带精确版本时不应匹配：%+v", withFilter.Devices)
	}
}

// TestQueryRejectsInvalidFilters 校验查询参数的合法性。
func TestQueryRejectsInvalidFilters(t *testing.T) {
	manager, _ := seedQueryFixture(t)
	if _, err := manager.Query(QueryOptions{Category: "not-a-category"}); err == nil {
		t.Fatal("类别代号不合法应报错")
	}
	if _, err := manager.Query(QueryOptions{MinimumExposure: "not-an-exposure"}); err == nil {
		t.Fatal("暴露级别代号不合法应报错")
	}
}
