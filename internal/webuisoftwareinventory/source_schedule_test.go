package webuisoftwareinventory

import (
	"context"
	"errors"
	"testing"

	"ssh-manager-mcp/internal/softwareinventory"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

// TestSourceSavesDailyTimeAndRequestsAllClientCollections 验证软件清单适
// 配层保存全局每日时间，并把「立即全部采集」交给领域层按采集执行者分派。
func TestSourceSavesDailyTimeAndRequestsAllClientCollections(t *testing.T) {
	st, _ := newTestStore(t)
	addTestDevice(t, st, "工作站")
	addTestDevice(t, st, "备份机")
	serverIDs := make([]string, 0, 2)
	for _, device := range []string{"工作站", "备份机"} {
		server, err := st.GetServerByName(device + "-ssh")
		if err != nil || server == nil {
			t.Fatalf("取 %s 服务器条目失败：%v %+v", device, err, server)
		}
		serverIDs = append(serverIDs, server.ID)
	}
	profileID, err := st.AddProfile("全局采集客户端")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, serverIDs); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AddCacheToken("mini6", profileID); err != nil {
		t.Fatal(err)
	}
	for _, device := range []string{"工作站", "备份机"} {
		if err := st.SetSoftwareCollectionCollector(
			device, "mini6", "", store.CLISource(),
		); err != nil {
			t.Fatal(err)
		}
	}
	source := FromOpener(func() (*store.Store, func(), error) { return st, func() {}, nil })

	if err := source.SetSoftwareInventoryDailyTime(
		"02:30", webui.SoftwareInventoryWriteSource{RemoteAddress: "198.51.100.9"},
	); err != nil {
		t.Fatal(err)
	}
	dailyTime, err := source.SoftwareInventoryDailyTime()
	if err != nil {
		t.Fatal(err)
	}
	if dailyTime != "02:30" {
		t.Fatalf("每日采集时间应保存为 02:30，得到 %s", dailyTime)
	}
	auditRows, err := st.QueryAudit(store.AuditFilter{
		Actions: []string{"software-collect-time"}, Source: store.AuditSourceWebUI,
		SourceIP: "198.51.100.9", Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(auditRows) != 1 {
		t.Fatalf("保存每日采集时间应写一条 Web UI 来源审计：%+v", auditRows)
	}
	report, err := source.RequestSoftwareInventoryCollectionAll(
		context.Background(), webui.SoftwareInventoryWriteSource{RemoteAddress: "198.51.100.9"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if report.Requested != 2 || report.Failed != 0 || len(report.Devices) != 2 {
		t.Fatalf("立即全部采集应登记两台客户端设备：%+v", report)
	}
	statuses, err := softwareinventory.New(st).CollectionStatuses(
		softwareinventory.AllDevicesVisible(), "",
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range statuses {
		if !status.PendingImmediate {
			t.Fatalf("设备 %s 应登记立即采集请求：%+v", status.DeviceName, status)
		}
	}
}

// TestSourceReturnsInterruptedCollectionReport 验证「立即全部采集」被取消
// 时，适配层仍把领域层的部分报告交给页面显示。
func TestSourceReturnsInterruptedCollectionReport(t *testing.T) {
	st, _ := newTestStore(t)
	source := FromOpener(func() (*store.Store, func(), error) { return st, func() {}, nil })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	report, err := source.RequestSoftwareInventoryCollectionAll(
		ctx, webui.SoftwareInventoryWriteSource{RemoteAddress: "198.51.100.9"},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("取消的立即全部采集应返回 context.Canceled，得到 %v", err)
	}
	if !report.Interrupted {
		t.Fatalf("部分报告应带被中断标记：%+v", report)
	}
}
