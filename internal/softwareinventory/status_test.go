package softwareinventory

import (
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// seedCollectedDevice 新增一台带服务器条目的设备并记录一次成功采集，返
// 回采集时间。时钟固定，供过期边界测试推进读取时刻。
func seedCollectedDevice(t *testing.T, manager *Manager, st *store.Store, deviceName string) time.Time {
	t.Helper()
	collectedAt := time.Unix(1770000000, 0)
	st.NowFn = func() time.Time { return collectedAt }
	servers, err := st.FullServersForDevice(deviceName)
	if err != nil || len(servers) == 0 {
		t.Fatalf("取 %s 的服务器条目失败：%v %+v", deviceName, err, servers)
	}
	if err := st.ReplaceSoftwareObservedItems(deviceName, store.SoftwareObservedWrite{
		Items: []models.SoftwareObservedItem{{
			Category: models.SoftwareCategorySystemPackage, Name: "openssh-server", Version: "1:9.6p1-1",
		}},
		ServerID: servers[0].ID, CommandSetVersion: "posix-v2", SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	return collectedAt
}

// TestCollectionStaleBoundaries 用固定时钟钉住第 7 天与第 8 天的边界：恰好
// 7 天不过期，超过 7 天过期；从未成功采集的受管设备算过期，不受管设备
// 不算。
func TestCollectionStaleBoundaries(t *testing.T) {
	manager, st := newTestManager(t)
	mustDevice(t, st, "受管设备", true)
	mustDevice(t, st, "网络存储", false)
	collectedAt := seedCollectedDevice(t, manager, st, "受管设备")

	manager.Now = func() time.Time { return collectedAt.Add(7 * 24 * time.Hour) }
	statuses, err := manager.CollectionStatuses(AllDevicesVisible(), "")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]CollectionStatusView{}
	for _, status := range statuses {
		byName[status.DeviceName] = status
	}
	if status := byName["受管设备"]; status.Stale {
		t.Fatalf("恰好 7 天不应过期：%+v", status)
	}
	if status := byName["网络存储"]; status.Stale || !status.NeverCollected || status.Managed {
		t.Fatalf("从未采集的不受管设备不应过期：%+v", status)
	}

	manager.Now = func() time.Time { return collectedAt.Add(7*24*time.Hour + time.Second) }
	statuses, err = manager.CollectionStatuses(AllDevicesVisible(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range statuses {
		if status.DeviceName == "受管设备" && !status.Stale {
			t.Fatalf("超过 7 天应过期：%+v", status)
		}
	}

	manager.Now = func() time.Time { return collectedAt.Add(8 * 24 * time.Hour) }
	statuses, err = manager.CollectionStatuses(AllDevicesVisible(), "受管设备")
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || !statuses[0].Stale || statuses[0].NeverCollected {
		t.Fatalf("第 8 天的采集状态 = %+v", statuses)
	}
}

// TestCollectionStatusesShowFailureAndVisibility 覆盖采集状态查询的字段与
// 可见范围：失败原因、不完整标志与命令集版本可见；profile 范围看不到未
// 授权设备，未授权设备单查报错。
func TestCollectionStatusesShowFailureAndVisibility(t *testing.T) {
	manager, st := newTestManager(t)
	mustDevice(t, st, "受管甲", true)
	mustDevice(t, st, "受管乙", true)
	mustDevice(t, st, "网络存储", false)
	collectedAt := seedCollectedDevice(t, manager, st, "受管甲")
	failedAt := collectedAt.Add(time.Hour)
	st.NowFn = func() time.Time { return failedAt }
	serverB, err := st.GetServerByName("受管乙-ssh")
	if err != nil || serverB == nil {
		t.Fatalf("取受管乙服务器条目失败：%v %+v", err, serverB)
	}
	if err := st.RecordSoftwareCollectionFailure("受管乙", "连接超时", serverB.ID, false, store.CLISource()); err != nil {
		t.Fatal(err)
	}

	manager.Now = func() time.Time { return collectedAt.Add(time.Hour) }
	serverA, err := st.GetServerByName("受管甲-ssh")
	if err != nil {
		t.Fatal(err)
	}
	visibility := DevicesVisibleThroughServers(serverA.ID)
	statuses, err := manager.CollectionStatuses(visibility, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].DeviceName != "受管甲" {
		t.Fatalf("profile 范围只能看到授权设备：%+v", statuses)
	}
	status := statuses[0]
	if status.DeviceID == "" || !status.Managed || status.Collector != "authority" ||
		status.ServerEntry != "受管甲-ssh" || status.LastSuccessAt == nil ||
		status.LastSuccessAt.Unix() != collectedAt.Unix() || status.CommandSetVersion != "posix-v2" {
		t.Fatalf("受管甲的采集状态 = %+v", status)
	}
	if _, err := manager.CollectionStatuses(visibility, "受管乙"); err == nil {
		t.Fatal("范围外设备的采集状态单查应报错")
	}
	if _, err := manager.CollectionStatuses(visibility, "网络存储"); err == nil {
		t.Fatal("不受管设备不得进入 profile 采集状态查询")
	}

	all, err := manager.CollectionStatuses(AllDevicesVisible(), "受管乙")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].DeviceName != "受管乙" || all[0].LastFailureAt == nil ||
		all[0].LastFailureReason != "连接超时" || all[0].LastSuccessAt != nil ||
		!all[0].Stale || !all[0].NeverCollected {
		t.Fatalf("受管乙的采集状态 = %+v", all)
	}
}

// TestManagerChangeHistoryRespectsVisibility 覆盖变化历史查询：时间倒序、
// 稳定键可见，并按可见设备范围过滤。
func TestManagerChangeHistoryRespectsVisibility(t *testing.T) {
	manager, st := newTestManager(t)
	mustDevice(t, st, "受管甲", true)
	mustDevice(t, st, "受管乙", true)
	firstAt := seedCollectedDevice(t, manager, st, "受管甲")
	secondAt := firstAt.Add(time.Hour)
	st.NowFn = func() time.Time { return secondAt }
	serversA, err := st.FullServersForDevice("受管甲")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceSoftwareObservedItems("受管甲", store.SoftwareObservedWrite{
		Items: []models.SoftwareObservedItem{{
			Category: models.SoftwareCategorySystemPackage, Name: "nginx", Version: "1.24.0",
		}},
		ServerID: serversA[0].ID, CommandSetVersion: "posix-v2", SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}

	history, err := manager.ChangeHistory("受管甲", AllDevicesVisible())
	if err != nil {
		t.Fatal(err)
	}
	if len(history) < 3 {
		t.Fatalf("变化历史行数不足：%+v", history)
	}
	for i := 1; i < len(history); i++ {
		if history[i-1].OccurredAt.Before(history[i].OccurredAt) {
			t.Fatalf("变化历史未按时间倒序返回：%+v", history)
		}
	}
	if !history[0].OccurredAt.Equal(secondAt) || !history[len(history)-1].OccurredAt.Equal(firstAt) {
		t.Fatalf("变化历史的起止时间不正确：%+v", history)
	}
	if history[0].StableKey == "" || history[0].Category == "" ||
		history[0].Category.Label() == "" {
		t.Fatalf("变化历史缺少条目标识或类别：%+v", history[0])
	}

	serverA, err := st.GetServerByName("受管甲-ssh")
	if err != nil {
		t.Fatal(err)
	}
	serverB, err := st.GetServerByName("受管乙-ssh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ChangeHistory("受管甲", DevicesVisibleThroughServers(serverB.ID)); err == nil {
		t.Fatal("范围外设备的变化历史查询应报错")
	}
	if _, err := manager.ChangeHistory("受管甲", DevicesVisibleThroughServers(serverA.ID)); err != nil {
		t.Fatal(err)
	}
}
