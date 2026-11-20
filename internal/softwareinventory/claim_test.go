package softwareinventory

import (
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// mustObservedItems 整体写入一台设备的实测条目，返回写入前的固定采集时间。
func mustObservedItems(
	t *testing.T,
	st *store.Store,
	deviceName string,
	items ...models.SoftwareObservedItem,
) time.Time {
	t.Helper()
	collectedAt := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	for i := range items {
		items[i].CollectedAt = collectedAt
	}
	if err := st.ReplaceSoftwareObservedItems(deviceName, store.SoftwareObservedWrite{
		Items: items, ServerID: "", CommandSetVersion: "posix-v2", SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	return collectedAt
}

// TestClaimKeepsDeclarationButQueryStaysObserved 覆盖声明认领实测后的数据
// 保留：认领关系留在声明行里，使用者可见查询仍只输出实测版本与端口。
func TestClaimKeepsDeclarationButQueryStaysObserved(t *testing.T) {
	manager, st := newTestManager(t)
	mustDevice(t, st, "工作站", true)
	observed := models.SoftwareObservedItem{
		Category: CategoryServiceProgram, Name: "nginx", Version: "1.25.3",
		PackageURL: "pkg:deb/ubuntu/nginx@1.25.3",
		ListeningPorts: []ListeningPort{{
			Address: "0.0.0.0", Port: 443, Protocol: "tcp", Exposure: ExposureAllAddresses,
		}},
	}
	collectedAt := mustObservedItems(t, st, "工作站", observed)
	storedObserved, err := st.SoftwareObservedItemsForDevice("工作站")
	if err != nil || len(storedObserved) != 1 {
		t.Fatalf("准备实测条目失败：%v %+v", err, storedObserved)
	}

	declarationID, err := manager.AddDeclaration(Declaration{
		Device: "工作站", Category: CategoryServiceProgram, Name: "Nginx 网站服务",
		Version: "1.25.0", CPE: "cpe:2.3:a:f5:nginx:1.25.0:*:*:*:*:*:*:*",
		ListeningPorts: []ListeningPort{{
			Address: "0.0.0.0", Port: 443, Protocol: "tcp", Exposure: ExposurePublicNetwork,
		}},
		Note: "对外网站入口",
	}, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.ClaimDeclaration(
		declarationID, storedObserved[0].StableKey, CLISource(),
	); err != nil {
		t.Fatal(err)
	}

	result, err := manager.Query(QueryOptions{Device: "工作站", Visibility: AllDevicesVisible()})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Devices) != 1 || len(result.Devices[0].Items) != 1 {
		t.Fatalf("认领后应只显示一条软件，实际：%+v", result.Devices)
	}
	item := result.Devices[0].Items[0]
	if item.Source != SourceObserved || item.Name != "nginx" ||
		item.Version != "1.25.3" || item.CPE != "" ||
		item.PackageURL != "pkg:deb/ubuntu/nginx@1.25.3" ||
		item.Note != "" || item.ObservedStableKey != storedObserved[0].StableKey ||
		item.CollectedAt == nil || !item.CollectedAt.Equal(collectedAt) {
		t.Fatalf("认领后的可见查询条目 = %+v", item)
	}
	if len(item.ListeningPorts) != 1 || item.ListeningPorts[0].Port != 443 ||
		item.ListeningPorts[0].Exposure != ExposureAllAddresses {
		t.Fatalf("可见查询的监听端口 = %+v", item.ListeningPorts)
	}
	declarations, err := st.SearchSoftwareDeclarations("", AllDevicesVisible())
	if err != nil {
		t.Fatal(err)
	}
	if len(declarations) != 1 || declarations[0].ID != declarationID ||
		declarations[0].ClaimedObservedKey != storedObserved[0].StableKey {
		t.Fatalf("认领关系应保留在声明存储里：%+v", declarations)
	}

	// 整体替换实测结果时保留稳定键，认领关系必须继续有效。
	replacement := observed
	replacement.StableKey = storedObserved[0].StableKey
	replacement.Version = "1.27.0"
	mustObservedItems(t, st, "工作站", replacement)
	result, err = manager.Query(QueryOptions{Device: "工作站", Visibility: AllDevicesVisible()})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Devices) != 1 || len(result.Devices[0].Items) != 1 ||
		result.Devices[0].Items[0].Version != "1.27.0" ||
		result.Devices[0].Items[0].Source != SourceObserved {
		t.Fatalf("整体替换后的实测查询结果 = %+v", result.Devices)
	}

	if err := manager.UnclaimDeclaration(declarationID, CLISource()); err != nil {
		t.Fatal(err)
	}
	result, err = manager.Query(QueryOptions{Device: "工作站", Visibility: AllDevicesVisible()})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Devices) != 1 || len(result.Devices[0].Items) != 1 {
		t.Fatalf("取消认领后可见查询仍应只有一条实测：%+v", result.Devices)
	}
	declarations, err = st.SearchSoftwareDeclarations("", AllDevicesVisible())
	if err != nil {
		t.Fatal(err)
	}
	if len(declarations) != 1 || declarations[0].ClaimedObservedKey != "" {
		t.Fatalf("取消认领后的声明 = %+v", declarations)
	}
}

// TestClaimSurvivesCollectorPackageUpgrade 用 Homebrew 解析器的真实稳定键验
// 证软件包升级：变化历史只记版本变化，声明认领关系不断开。
func TestClaimSurvivesCollectorPackageUpgrade(t *testing.T) {
	manager, st := newTestManager(t)
	mustDevice(t, st, "Mac 工作站", true)
	oldItems := buildDarwinBrewItems(parsedSection{lines: []string{
		"formula\topenssh 9.9p1",
	}})
	newItems := buildDarwinBrewItems(parsedSection{lines: []string{
		"formula\topenssh 10.0p1",
	}})
	mustObservedItems(t, st, "Mac 工作站", oldItems...)
	observed, err := st.SoftwareObservedItemsForDevice("Mac 工作站")
	if err != nil || len(observed) != 1 {
		t.Fatalf("读取旧版本实测条目失败：%v %+v", err, observed)
	}
	declarationID, err := manager.AddDeclaration(Declaration{
		Device: "Mac 工作站", Category: CategorySystemPackage,
		Name: "OpenSSH", Version: "9.9p1",
	}, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.ClaimDeclaration(declarationID, observed[0].StableKey, CLISource()); err != nil {
		t.Fatal(err)
	}

	mustObservedItems(t, st, "Mac 工作站", newItems...)
	history, err := manager.ChangeHistory("Mac 工作站", AllDevicesVisible())
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 {
		t.Fatalf("升级应产生实测基线、版本变化与声明生效历史：%+v", history)
	}
	byKind := map[models.SoftwareChangeKind]models.SoftwareChangeEntry{}
	for _, row := range history {
		byKind[row.Kind] = row
	}
	if byKind[models.SoftwareChangeVersionChanged].OldValue != "9.9p1" ||
		byKind[models.SoftwareChangeVersionChanged].NewValue != "10.0p1" ||
		byKind[models.SoftwareChangeVersionChanged].StableKey != observed[0].StableKey {
		t.Fatalf("升级历史 = %+v", byKind[models.SoftwareChangeVersionChanged])
	}
	result, err := manager.Query(QueryOptions{
		Device: "Mac 工作站", IncludeSystemPackages: true, Visibility: AllDevicesVisible(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Devices) != 1 || len(result.Devices[0].Items) != 1 ||
		result.Devices[0].Items[0].Source != SourceObserved ||
		result.Devices[0].Items[0].Version != "10.0p1" {
		t.Fatalf("升级后的实测查询结果 = %+v", result.Devices)
	}
}
