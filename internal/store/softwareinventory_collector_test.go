package store

import (
	"encoding/json"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// TestSetSoftwareCollectionCollector 验证采集执行者登记的校验与事务结果：
// 只有活动设备码绑定 profile 中包含的采集用服务器条目可以登记。
func TestSetSoftwareCollectionCollector(t *testing.T) {
	st := newCollectTestStore(t)
	server := addCollectServer(t, st, "工作站", "工作站入口")
	addCollectServer(t, st, "默认条目设备", "默认条目入口")
	other := addCollectServer(t, st, "分支工作站", "分支入口")
	profile, err := st.AddProfile("采集 profile")
	if err != nil {
		t.Fatal(err)
	}
	defaultServer, err := st.GetServerByName("默认条目入口")
	if err != nil || defaultServer == nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profile, []string{server.ID, defaultServer.ID}); err != nil {
		t.Fatal(err)
	}
	laptopID, _, err := st.AddCacheToken("laptop", profile)
	if err != nil {
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
	if err := st.SetSoftwareCollectionCollector("默认条目设备", "laptop", "", CLISource()); err != nil {
		t.Fatal(err)
	}
	status, err := st.SoftwareCollectionStatusForDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if status.Collector.CacheTokenID != laptopID || status.PreferredServerID != server.ID {
		t.Fatalf("登记后的采集状态 = %+v", status)
	}
	rows, err := st.QueryAudit(AuditFilter{Actions: []string{"software-collect-collector"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("两次登记应写两行审计，得到 %d 行", len(rows))
	}
	var summary map[string]any
	for _, row := range rows {
		var candidate map[string]any
		if err := json.Unmarshal([]byte(row.Command), &candidate); err != nil {
			t.Fatal(err)
		}
		if candidate["device"] == "工作站" {
			summary = candidate
			break
		}
	}
	if summary["device"] != "工作站" || summary["collector"] != "laptop" ||
		summary["collector_id"] != laptopID || summary["server"] != "工作站入口" {
		t.Fatalf("登记审计摘要 = %v", summary)
	}
	defaultStatus, err := st.SoftwareCollectionStatusForDevice("默认条目设备")
	if err != nil || defaultStatus.PreferredServerID != "" {
		t.Fatalf("未指定条目时不得把默认选择写死：%+v %v", defaultStatus, err)
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
	laptopID, _, err := st.AddCacheToken("laptop", profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSoftwareCollectionCollector("工作站", "laptop", server.ID, CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceSoftwareObservedItems("工作站", SoftwareObservedWrite{
		Collector: models.ClientCollector(laptopID), ServerID: server.ID,
		CommandSetVersion: "posix-v2", SystemType: "ubuntu",
	}, CLISource()); err != nil {
		t.Fatal(err)
	}
	status, err := st.SoftwareCollectionStatusForDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if status.Collector.CacheTokenID != laptopID || status.PendingImmediate {
		t.Fatalf("客户端上交后应保留采集执行者并清除立即采集请求：%+v", status)
	}
}

// TestSoftwareCollectionCollectorUsesDeviceCodeID 验证采集执行者存储设备码
// 编号：吊销后同名重签的新设备码不继承分派；名字叫 authority 的设备码只
// 是普通客户端实例。
func TestSoftwareCollectionCollectorUsesDeviceCodeID(t *testing.T) {
	st := newCollectTestStore(t)
	server := addCollectServer(t, st, "工作站", "工作站入口")
	profile, err := st.AddProfile("采集 profile")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profile, []string{server.ID}); err != nil {
		t.Fatal(err)
	}
	oldID, _, err := st.AddCacheToken("mini6", profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSoftwareCollectionCollector("工作站", "mini6", server.ID, CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := st.RevokeCacheToken("mini6"); err != nil {
		t.Fatal(err)
	}
	status, err := st.SoftwareCollectionStatusForDevice("工作站")
	if err != nil || status.Collector.CacheTokenID != oldID || !status.CollectorRevoked ||
		status.CollectorName != "mini6" {
		t.Fatalf("吊销后的登记仍应指向原设备码编号：%+v %v", status, err)
	}
	newID, _, err := st.AddCacheToken("mini6", profile)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := st.SoftwareCollectionStatusesForCollector(models.ClientCollector(newID))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("同名重签的新设备码不得继承采集分派：%+v", rows)
	}

	authorityID, _, err := st.AddCacheToken("authority", profile)
	if err != nil {
		t.Fatal(err)
	}
	other := addCollectServer(t, st, " authority 设备", "authority 入口")
	if err := st.GrantServers(profile, []string{other.ID}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSoftwareCollectionCollector(" authority 设备", "authority", other.ID, CLISource()); err != nil {
		t.Fatal(err)
	}
	status, err = st.SoftwareCollectionStatusForDevice(" authority 设备")
	if err != nil || !status.Collector.IsClient() ||
		status.Collector.CacheTokenID != authorityID {
		t.Fatalf("名字 authority 的设备码应按普通客户端实例登记：%+v %v", status, err)
	}
}
