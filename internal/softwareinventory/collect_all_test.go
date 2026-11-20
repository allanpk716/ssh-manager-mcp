package softwareinventory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// TestCollectAllForScopeTraversesEveryManagedDevice 覆盖「立即全部采集」：
// 本实例同步采集成功与失败设备，客户端实例设备只登记请求；单台失败不中
// 断，失败原因落库且旧实测清单保留。
func TestCollectAllForScopeTraversesEveryManagedDevice(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 0, 0, 0, time.Local)
	manager, st := newTestManager(t)
	manager.Now = func() time.Time { return now }
	st.NowFn = func() time.Time { return now }
	mustDevice(t, st, "成功设备", true)
	mustDevice(t, st, "失败设备", true)
	mustDevice(t, st, "客户端采集设备", true)
	mustDevice(t, st, "不受管设备", false)

	failedServer, err := st.GetServerByName("失败设备-ssh")
	if err != nil || failedServer == nil {
		t.Fatalf("取失败设备服务器条目失败：%v %+v", err, failedServer)
	}
	oldAt := now.Add(-8 * 24 * time.Hour)
	st.NowFn = func() time.Time { return oldAt }
	if err := st.ReplaceSoftwareObservedItems("失败设备", store.SoftwareObservedWrite{
		Items: []models.SoftwareObservedItem{{
			Category: models.SoftwareCategorySystemPackage, Name: "openssl", Version: "3.0.2",
		}},
		ServerID: failedServer.ID, CommandSetVersion: POSIXCommandSetVersion, SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	st.NowFn = func() time.Time { return now }

	clientServer, err := st.GetServerByName("客户端采集设备-ssh")
	if err != nil || clientServer == nil {
		t.Fatalf("取客户端采集设备服务器条目失败：%v %+v", err, clientServer)
	}
	profileID, err := st.AddProfile("立即全部采集客户端")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{clientServer.ID}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AddCacheToken("mini6", profileID); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSoftwareCollectionCollector(
		"客户端采集设备", "mini6", "", store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}

	service := &Service{Store: st, Now: manager.Now, RemoteExec: func(
		_ context.Context, server *models.Server, _ models.ShellType, _ string, _ bool,
	) (RemoteExecResult, error) {
		if server.Name == "失败设备-ssh" {
			return RemoteExecResult{}, errors.New("连接超时，端口不通")
		}
		return RemoteExecResult{Stdout: fixtureOutput(t, "ubuntu2404-full.txt")}, nil
	}}
	report, err := manager.CollectAllForScope(
		context.Background(), AllDevicesVisible(), service, WebUISource("198.51.100.9"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if report.Succeeded != 1 || report.Requested != 1 || report.Failed != 1 || len(report.Devices) != 3 {
		t.Fatalf("立即全部采集统计不符：%+v", report)
	}
	var failure *ImmediateCollectionDeviceReport
	for i := range report.Devices {
		if report.Devices[i].DeviceName == "失败设备" {
			failure = &report.Devices[i]
		}
	}
	if failure == nil || failure.Err == "" || !strings.Contains(failure.Err, "连接超时") {
		t.Fatalf("失败设备应带白话失败原因：%+v", failure)
	}
	status, err := st.SoftwareCollectionStatusForDevice("失败设备")
	if err != nil || !status.LastFailureAt.Equal(now) ||
		!strings.Contains(status.LastFailureReason, "连接超时，端口不通") {
		t.Fatalf("失败状态应保留失败时间与原因：%+v %v", status, err)
	}
	items, err := st.SoftwareObservedItemsForDevice("失败设备")
	if err != nil || len(items) != 1 || items[0].Name != "openssl" {
		t.Fatalf("失败后不应清空上次成功清单：%+v %v", items, err)
	}
}
