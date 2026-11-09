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

// TestClaimCombinesObservedAndDeclaredItems 覆盖声明认领实测后的合成结果：
// 一条软件只出现一条，来源为两者合成，版本与端口来自实测，暴露级别与
// 声明提供的标识、备注来自声明。
func TestClaimCombinesObservedAndDeclaredItems(t *testing.T) {
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
	if item.Source != SourceCombined || item.Name != "Nginx 网站服务" ||
		item.Version != "1.25.3" || item.CPE != "cpe:2.3:a:f5:nginx:1.25.0:*:*:*:*:*:*:*" ||
		item.Note != "对外网站入口" || item.ObservedStableKey != storedObserved[0].StableKey ||
		item.CollectedAt == nil || !item.CollectedAt.Equal(collectedAt) {
		t.Fatalf("认领后的合成条目 = %+v", item)
	}
	if len(item.ListeningPorts) != 1 || item.ListeningPorts[0].Port != 443 ||
		item.ListeningPorts[0].Exposure != ExposurePublicNetwork {
		t.Fatalf("合成条目的监听端口 = %+v", item.ListeningPorts)
	}
	if !item.VersionMismatch {
		t.Fatalf("声明与实测版本不同应标出版本不一致：%+v", item)
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
		result.Devices[0].Items[0].Source != SourceCombined {
		t.Fatalf("整体替换后认领关系失效：%+v", result.Devices)
	}

	if err := manager.UnclaimDeclaration(declarationID, CLISource()); err != nil {
		t.Fatal(err)
	}
	result, err = manager.Query(QueryOptions{Device: "工作站", Visibility: AllDevicesVisible()})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Devices) != 1 || len(result.Devices[0].Items) != 2 {
		t.Fatalf("取消认领后应重新显示声明与实测两条：%+v", result.Devices)
	}
}
