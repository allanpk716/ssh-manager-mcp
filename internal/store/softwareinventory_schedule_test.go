package store

import (
	"encoding/json"
	"testing"
	"time"
)

// TestSoftwareCollectionStatusesForCollector 验证调度服务读到期设备的公开
// 入口：只返回挂了服务器条目的受管设备；从未采集的设备也返回零值状态；
// 采集执行者不是指定实例的设备不返回。
func TestSoftwareCollectionStatusesForCollector(t *testing.T) {
	st := newCollectTestStore(t)
	past := time.Unix(1769000000, 0)
	st.NowFn = func() time.Time { return past }
	server := addCollectServer(t, st, "工作站", "工作站入口")
	addCollectServer(t, st, "备用机", "备用机入口")
	if _, err := st.AddDevice("网络存储", "", ""); err != nil {
		t.Fatal(err)
	}
	branchServer := addCollectServer(t, st, "分支工作站", "分支入口")
	if _, err := st.db.Exec(`INSERT INTO software_collection_status(device_id,collector)
		VALUES(?,'cache-token-9')`, branchServer.DeviceID); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceSoftwareObservedItems("工作站", SoftwareObservedWrite{
		ServerID: server.ID, CommandSetVersion: "linux-v1", SystemType: "ubuntu",
	}, CLISource()); err != nil {
		t.Fatal(err)
	}

	rows, err := st.SoftwareCollectionStatusesForCollector("authority")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("权威端应看到两台受管设备，得到 %d 台：%+v", len(rows), rows)
	}
	if rows[0].DeviceName != "备用机" || rows[0].LastSuccessAt.Unix() != 0 {
		t.Fatalf("从未采集的备用机应排在前面且上次成功时间为零：%+v", rows[0])
	}
	if rows[1].DeviceName != "工作站" || rows[1].LastSuccessAt != past ||
		rows[1].LastServerName != "工作站入口" || rows[1].Collector != "authority" {
		t.Fatalf("工作站状态 = %+v", rows[1])
	}

	clientRows, err := st.SoftwareCollectionStatusesForCollector("cache-token-9")
	if err != nil {
		t.Fatal(err)
	}
	if len(clientRows) != 1 || clientRows[0].DeviceName != "分支工作站" {
		t.Fatalf("客户端实例只应看到自己负责的设备：%+v", clientRows)
	}
}

// TestSoftwareImmediateCollectionRequest 验证立即采集请求的登记与清除：登
// 记后状态带待执行标记并写审计；成功或失败的采集落库都把请求当作已处
// 理。
func TestSoftwareImmediateCollectionRequest(t *testing.T) {
	st := newCollectTestStore(t)
	now := time.Unix(1770000000, 0)
	st.NowFn = func() time.Time { return now }
	server := addCollectServer(t, st, "工作站", "工作站入口")

	if err := st.RequestSoftwareImmediateCollection("不存在", CLISource()); err == nil {
		t.Fatal("对不存在的设备登记请求应报错")
	}
	if err := st.RequestSoftwareImmediateCollection("工作站", CLISource()); err != nil {
		t.Fatal(err)
	}
	status, err := st.SoftwareCollectionStatusForDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if !status.PendingImmediate {
		t.Fatalf("登记后应有待执行的立即采集请求：%+v", status)
	}
	rows, err := st.QueryAudit(AuditFilter{Actions: []string{"software-collect-request"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("登记请求应写一行审计，得到 %d 行", len(rows))
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(rows[0].Command), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["device"] != "工作站" {
		t.Fatalf("请求审计摘要 = %v", summary)
	}

	if err := st.ReplaceSoftwareObservedItems("工作站", SoftwareObservedWrite{
		ServerID: server.ID, CommandSetVersion: "linux-v1", SystemType: "ubuntu",
	}, CLISource()); err != nil {
		t.Fatal(err)
	}
	if status, err = st.SoftwareCollectionStatusForDevice("工作站"); err != nil {
		t.Fatal(err)
	}
	if status.PendingImmediate {
		t.Fatalf("成功采集后请求应清除：%+v", status)
	}

	if err := st.RequestSoftwareImmediateCollection("工作站", CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordSoftwareCollectionFailure("工作站", "连接超时", server.ID, false, CLISource()); err != nil {
		t.Fatal(err)
	}
	if status, err = st.SoftwareCollectionStatusForDevice("工作站"); err != nil {
		t.Fatal(err)
	}
	if status.PendingImmediate || status.LastFailureReason != "连接超时" {
		t.Fatalf("失败的采集同样算处理了请求：%+v", status)
	}
}
