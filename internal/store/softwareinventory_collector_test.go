package store

import (
	"encoding/json"
	"testing"
)

// TestSetSoftwareCollectionCollector 验证采集执行者登记的校验与事务结果：
// 只有活动设备码绑定 profile 中包含的采集用服务器条目可以登记。
func TestSetSoftwareCollectionCollector(t *testing.T) {
	st := newCollectTestStore(t)
	server := addCollectServer(t, st, "工作站", "工作站入口")
	other := addCollectServer(t, st, "分支工作站", "分支入口")
	profile, err := st.AddProfile("采集 profile")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profile, []string{server.ID}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AddCacheToken("laptop", profile); err != nil {
		t.Fatal(err)
	}

	if err := st.SetSoftwareCollectionCollector("工作站", "不存在的实例", "", CLISource()); err == nil {
		t.Fatal("设备码不存在时必须拒绝登记")
	}
	if err := st.SetSoftwareCollectionCollector("工作站", "laptop", other.ID, CLISource()); err == nil ||
		len(err.Error()) == 0 {
		t.Fatalf("采集用服务器条目不在设备码绑定 profile 里时必须拒绝并说明原因：%v", err)
	}
	if err := st.SetSoftwareCollectionCollector("工作站", "laptop", server.ID, CLISource()); err != nil {
		t.Fatal(err)
	}
	status, err := st.SoftwareCollectionStatusForDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if status.Collector != "laptop" || status.PreferredServerID != server.ID {
		t.Fatalf("登记后的采集状态 = %+v", status)
	}
	rows, err := st.QueryAudit(AuditFilter{Actions: []string{"software-collect-collector"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("登记应写一行审计，得到 %d 行", len(rows))
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(rows[0].Command), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["device"] != "工作站" || summary["collector"] != "laptop" || summary["server"] != "工作站入口" {
		t.Fatalf("登记审计摘要 = %v", summary)
	}
}

// TestSoftwareObservedWritePreservesClientCollector 验证客户端上交后的落库
// 不会把这台设备登记的采集执行者改回权威端。
func TestSoftwareObservedWritePreservesClientCollector(t *testing.T) {
	st := newCollectTestStore(t)
	server := addCollectServer(t, st, "工作站", "工作站入口")
	profile, err := st.AddProfile("采集 profile")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profile, []string{server.ID}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AddCacheToken("laptop", profile); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSoftwareCollectionCollector("工作站", "laptop", server.ID, CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceSoftwareObservedItems("工作站", SoftwareObservedWrite{
		Collector: "laptop", ServerID: server.ID,
		CommandSetVersion: "linux-v1", SystemType: "ubuntu",
	}, CLISource()); err != nil {
		t.Fatal(err)
	}
	status, err := st.SoftwareCollectionStatusForDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if status.Collector != "laptop" || status.PendingImmediate {
		t.Fatalf("客户端上交后应保留采集执行者并清除立即采集请求：%+v", status)
	}
}
