package softwareinventory

import (
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// TestDeviceDeleteIncludesObservedItemsAndHistory 覆盖设备删除预览与级联：
// 预览列出实测条目与变化历史数量；确认删除后实测条目、监听端口、采集
// 状态、sudo 开关与变化历史全部级联删除。
func TestDeviceDeleteIncludesObservedItemsAndHistory(t *testing.T) {
	_, st := newTestManager(t)
	mustDevice(t, st, "工作站", true)
	server, err := st.GetServerByName("工作站-ssh")
	if err != nil || server == nil {
		t.Fatalf("取服务器条目失败：%v %+v", err, server)
	}
	firstAt := time.Unix(1770000000, 0)
	secondAt := firstAt.Add(time.Hour)
	st.NowFn = func() time.Time { return firstAt }
	if err := st.ReplaceSoftwareObservedItems("工作站", store.SoftwareObservedWrite{
		Items: []models.SoftwareObservedItem{{
			Category: models.SoftwareCategorySystemPackage, Name: "openssh-server",
			Version: "1:9.6p1-1",
		}},
		ServerID: server.ID, CommandSetVersion: "linux-v1", SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	st.NowFn = func() time.Time { return secondAt }
	if err := st.ReplaceSoftwareObservedItems("工作站", store.SoftwareObservedWrite{
		Items: []models.SoftwareObservedItem{{
			Category: models.SoftwareCategorySystemPackage, Name: "openssh-server",
			Version: "1:9.9p1-1",
			ListeningPorts: []models.SoftwareListeningPort{{
				Address: "0.0.0.0", Port: 22, Protocol: "tcp",
				Exposure: models.SoftwareExposureAllAddresses,
			}},
		}},
		ServerID: server.ID, CommandSetVersion: "linux-v1", SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSoftwareCollectSudo(server.ID, true, store.CLISource()); err != nil {
		t.Fatal(err)
	}

	preview, err := st.DeviceDeletePreview("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if preview.SoftwareObservedItemCount != 1 || preview.SoftwareChangeHistoryCount < 2 {
		t.Fatalf("设备删除预览缺少实测与历史数量：实测 %d、历史 %d",
			preview.SoftwareObservedItemCount, preview.SoftwareChangeHistoryCount)
	}
	if _, err := st.DeleteDeviceCascading("工作站", preview); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("工作站", "", ""); err != nil {
		t.Fatal(err)
	}
	items, err := st.SoftwareObservedItemsForDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("设备删除后实测条目必须级联删除：%+v", items)
	}
	history, err := st.SoftwareChangeHistoryForDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 0 {
		t.Fatalf("设备删除后变化历史必须级联删除：%+v", history)
	}
	status, err := st.SoftwareCollectionStatusForDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if status.LastSuccessAt.Unix() > 0 || status.LastFailureAt.Unix() > 0 ||
		status.LastFailureReason != "" || status.LastIncomplete ||
		status.CommandSetVersion != "" {
		t.Fatalf("设备删除后采集状态必须级联删除：%+v", status)
	}
	sudo, err := st.SoftwareCollectSudoEnabled()
	if err != nil {
		t.Fatal(err)
	}
	if len(sudo) != 0 {
		t.Fatalf("设备删除后 sudo 采集开关必须随服务器条目级联删除：%+v", sudo)
	}
}
