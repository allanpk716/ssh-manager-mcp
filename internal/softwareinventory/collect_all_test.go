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

// TestCollectAllForScopeReturnsPartialReportWhenCanceled 验证上下文取消后
// 停止遍历：已完成的设备保留在部分报告里，未执行设备不算作失败。
func TestCollectAllForScopeReturnsPartialReportWhenCanceled(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 0, 0, 0, time.Local)
	manager, st := newTestManager(t)
	manager.Now = func() time.Time { return now }
	st.NowFn = func() time.Time { return now }
	mustDevice(t, st, "A工作站", true)
	mustDevice(t, st, "备份机", true)

	ctx, cancel := context.WithCancel(context.Background())
	service := &Service{Store: st, Now: manager.Now, RemoteExec: func(
		_ context.Context, server *models.Server, _ models.ShellType, _ string, _ bool,
	) (RemoteExecResult, error) {
		if server.Name == "A工作站-ssh" {
			cancel()
		}
		return RemoteExecResult{Stdout: fixtureOutput(t, "ubuntu2404-full.txt")}, nil
	}}
	report, err := manager.CollectAllForScope(ctx, AllDevicesVisible(), service, WebUISource("198.51.100.9"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("取消后应返回 context.Canceled，得到 %v", err)
	}
	if report == nil || !report.Interrupted {
		t.Fatalf("部分报告应带被中断标记：%+v", report)
	}
	if report.Succeeded != 1 || report.Failed != 0 || len(report.Devices) != 1 {
		t.Fatalf("取消后应只保留已完成的设备：%+v", report)
	}
	if report.Devices[0].DeviceName != "A工作站" || report.Devices[0].Report == nil {
		t.Fatalf("A工作站的采集结果应保留在部分报告里：%+v", report.Devices[0])
	}
	if report.EndedAt.IsZero() {
		t.Fatal("部分报告应填写结束时间")
	}
	cancel()
}

// TestCollectAllForScopeContinuesWhenDeviceVisibilityReadFails 验证读取某台
// 设备可见范围失败时，这台设备只记为失败，后续设备仍会继续处理。
func TestCollectAllForScopeContinuesWhenDeviceVisibilityReadFails(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 0, 0, 0, time.Local)
	manager, st := newTestManager(t)
	manager.Now = func() time.Time { return now }
	st.NowFn = func() time.Time { return now }
	mustDevice(t, st, "A工作站", true)
	mustDevice(t, st, "备份机", true)

	service := &Service{Store: st, Now: manager.Now, RemoteExec: func(
		_ context.Context, server *models.Server, _ models.ShellType, _ string, _ bool,
	) (RemoteExecResult, error) {
		if server.Name == "A工作站-ssh" {
			_ = st.Close()
			return RemoteExecResult{}, errors.New("连接已关闭")
		}
		return RemoteExecResult{}, errors.New("不应再执行采集")
	}}
	report, err := manager.CollectAllForScope(
		context.Background(), AllDevicesVisible(), service, WebUISource("198.51.100.9"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if report.Failed != 2 || len(report.Devices) != 2 {
		t.Fatalf("可见范围读取失败不应中断后续设备：%+v", report)
	}
	if !strings.Contains(report.Devices[1].Err, "读取设备 备份机 的可见范围失败") {
		t.Fatalf("备份机应带可见范围读取失败原因：%+v", report.Devices[1])
	}
}
