package webuisoftwareinventory

import (
	"strings"
	"testing"

	"ssh-manager-mcp/internal/softwareinventory"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

// TestSourceSavesDailyTimeAndStartsClientCollectionRound 验证软件清单适配层
// 保存全局每日时间，并把「立即全部采集」发起为后台采集轮次：采集执行者是
// 客户端实例的设备登记立即采集请求。
func TestSourceSavesDailyTimeAndStartsClientCollectionRound(t *testing.T) {
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

	if err := source.StartSoftwareInventoryCollectionRound(
		"", webui.SoftwareInventoryWriteSource{RemoteAddress: "198.51.100.9"},
	); err != nil {
		t.Fatal(err)
	}
	view := waitRoundView(t, source, func(view *webui.SoftwareInventoryRoundView) bool {
		return !view.Running && strings.Contains(view.SummaryText, "等待客户端 2 台")
	})
	if view.StatusText != "本机采集已完成，等待客户端实例回传结果" {
		t.Fatalf("整轮状态 = %q", view.StatusText)
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
