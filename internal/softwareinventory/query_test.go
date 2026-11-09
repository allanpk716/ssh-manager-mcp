package softwareinventory

import (
	"errors"
	"testing"
)

// seedQueryFixture 造四台设备：受管甲、受管乙挂有服务器条目且都在 profile
// 授权里，受管丙的服务器条目不在授权里，网络存储是不受管设备。声明覆盖
// 名称、暴露级别、类别与系统包各条过滤分支。
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
				{Address: "10.0.0.5", Port: 5432, Protocol: "tcp", Exposure: ExposurePublicNetwork},
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

// TestQueryFiltersAndCollectionStatus 覆盖查询工具的全部过滤条件与采集
// 状态字段：采集状态表还没有数据源，字段必须在且表示「从未采集」。
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
	if item.Name != "PostgreSQL" || item.Source != SourceDeclared || item.CollectedAt != nil {
		t.Fatalf("查询条目 = %+v", item)
	}
	if len(item.ListeningPorts) != 1 || item.ListeningPorts[0].Exposure != ExposurePublicNetwork {
		t.Fatalf("查询条目监听端口 = %+v", item.ListeningPorts)
	}

	// 采集状态：字段在，数据源缺失时表示从未采集、清单过期。
	status := result.Devices[0].Collection
	if !status.NeverCollected || !status.Stale || status.LastSuccessAt != nil ||
		status.LastFailureAt != nil || status.LastFailureReason != "" ||
		status.Collector != "" || status.Incomplete || status.CommandSetVersion != "" {
		t.Fatalf("缺失采集数据源时的采集状态 = %+v", status)
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
		t.Fatalf("公网监听应满足最低暴露级别：%+v", result.Devices)
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
