package softwareinventory

import (
	"path/filepath"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// TestSoftwareCollectionBackupProfileSnapshotAndRevision 钉住实测与变化历史
// 各表的备份边界：全量导出包含实测条目、实测监听端口、采集状态、sudo 开
// 关与变化历史并能逐字段恢复；profile 导出一律不含；这些表的数据变化不
// 推进快照修订号。
func TestSoftwareCollectionBackupProfileSnapshotAndRevision(t *testing.T) {
	_, st := newTestManager(t)
	deviceID := mustDevice(t, st, "受管甲", true)
	mustDevice(t, st, "网络存储", false)
	server, err := st.GetServerByName("受管甲-ssh")
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

	firstAt := time.Unix(1770000000, 0)
	secondAt := firstAt.Add(time.Hour)
	st.NowFn = func() time.Time { return firstAt }
	if err := st.ReplaceSoftwareObservedItems("受管甲", store.SoftwareObservedWrite{
		Items: []models.SoftwareObservedItem{{
			Category: models.SoftwareCategorySystemPackage, Name: "openssh-server",
			Version: "1:9.6p1-1",
			ListeningPorts: []models.SoftwareListeningPort{{
				Address: "0.0.0.0", Port: 22, Protocol: "tcp",
				Exposure: models.SoftwareExposureAllAddresses,
			}},
		}},
		ServerID: server.ID, CommandSetVersion: "posix-v2", SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	st.NowFn = func() time.Time { return secondAt }
	if err := st.ReplaceSoftwareObservedItems("受管甲", store.SoftwareObservedWrite{
		Items: []models.SoftwareObservedItem{{
			Category: models.SoftwareCategorySystemPackage, Name: "openssh-server",
			Version: "1:9.9p1-1",
			ListeningPorts: []models.SoftwareListeningPort{{
				Address: "127.0.0.1", Port: 22, Protocol: "tcp",
				Exposure: models.SoftwareExposureLocal,
			}},
		}},
		ServerID: server.ID, CommandSetVersion: "posix-v2", SystemType: "ubuntu",
		Incomplete: true,
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSoftwareCollectSudo(server.ID, true, store.CLISource()); err != nil {
		t.Fatal(err)
	}

	profileSnapshot, err := st.ExportSnapshotForProfile(profileID, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(profileSnapshot.SoftwareObservedItems) != 0 ||
		len(profileSnapshot.SoftwareObservedPorts) != 0 ||
		len(profileSnapshot.SoftwareCollectionStatuses) != 0 ||
		len(profileSnapshot.SoftwareCollectSudoSettings) != 0 ||
		len(profileSnapshot.SoftwareChangeHistory) != 0 {
		t.Fatalf("profile 导出不得携带软件清单：%+v %+v %+v %+v %+v",
			profileSnapshot.SoftwareObservedItems, profileSnapshot.SoftwareObservedPorts,
			profileSnapshot.SoftwareCollectionStatuses, profileSnapshot.SoftwareCollectSudoSettings,
			profileSnapshot.SoftwareChangeHistory)
	}

	revisionBefore, err := st.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	st.NowFn = func() time.Time { return secondAt.Add(time.Hour) }
	if err := st.RecordSoftwareCollectionFailure("受管甲", "连接超时", server.ID, false, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSoftwareCollectSudo(server.ID, false, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	revisionAfter, err := st.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if revisionBefore != revisionAfter {
		t.Fatalf("软件清单采集数据推进了快照修订号：%d -> %d", revisionBefore, revisionAfter)
	}

	fullSnapshot, err := st.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(fullSnapshot.SoftwareObservedItems) != 1 ||
		len(fullSnapshot.SoftwareObservedPorts) != 1 ||
		len(fullSnapshot.SoftwareCollectionStatuses) != 1 ||
		len(fullSnapshot.SoftwareCollectSudoSettings) != 1 ||
		len(fullSnapshot.SoftwareChangeHistory) == 0 {
		t.Fatalf("全量导出缺少软件清单段：%+v %+v %+v %+v %+v",
			fullSnapshot.SoftwareObservedItems, fullSnapshot.SoftwareObservedPorts,
			fullSnapshot.SoftwareCollectionStatuses, fullSnapshot.SoftwareCollectSudoSettings,
			fullSnapshot.SoftwareChangeHistory)
	}

	targetKey, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	target, err := store.Open(filepath.Join(t.TempDir(), "restored.db"), targetKey)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if err := target.ImportSnapshot(fullSnapshot); err != nil {
		t.Fatal(err)
	}

	items, err := target.SoftwareObservedItemsForDevice("受管甲")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Name != "openssh-server" ||
		items[0].Version != "1:9.9p1-1" || items[0].StableKey == "" ||
		items[0].CollectedAt.Unix() != secondAt.Unix() ||
		len(items[0].ListeningPorts) != 1 ||
		items[0].ListeningPorts[0].Exposure != models.SoftwareExposureLocal {
		t.Fatalf("恢复后的实测条目 = %+v", items)
	}
	status, err := target.SoftwareCollectionStatusForDevice("受管甲")
	if err != nil {
		t.Fatal(err)
	}
	if !status.Managed || status.LastServerID != server.ID ||
		status.LastSuccessAt.Unix() != secondAt.Unix() ||
		status.LastFailureReason != "连接超时" || !status.LastIncomplete ||
		status.CommandSetVersion != "posix-v2" || status.SystemType != "ubuntu" {
		t.Fatalf("恢复后的采集状态 = %+v", status)
	}
	sudo, err := target.SoftwareCollectSudoEnabled()
	if err != nil {
		t.Fatal(err)
	}
	if sudo[server.ID] {
		t.Fatalf("恢复后的 sudo 采集开关 = %+v", sudo)
	}
	history, err := target.SoftwareChangeHistoryForDevice("受管甲")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) < 3 || history[0].DeviceID != deviceID {
		t.Fatalf("恢复后的变化历史 = %+v", history)
	}
	if _, err := New(target).ChangeHistory("受管甲", AllDevicesVisible()); err != nil {
		t.Fatalf("恢复后的库必须能继续查询变化历史：%v", err)
	}
}
