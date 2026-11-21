package softwareinventory

import (
	"context"
	"maps"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// TestCollectForSubmissionDoesNotWriteLocalInventory 验证客户端采集只生成
// 待上交结果，不在客户端本地保存软件清单或采集状态。
func TestCollectForSubmissionDoesNotWriteLocalInventory(t *testing.T) {
	service, st := newCollectService(t)
	servers := addCollectDevice(t, st, "工作站", "甲入口")
	assignment := models.SoftwareCollectionAssignment{
		DeviceName: "工作站", DeviceID: "device-id", ServerID: servers["甲入口"].ID,
		ServerName: "甲入口",
	}
	var recorded recordedExec
	service.RemoteExec = replayCollectExec(fixtureOutput(t, "ubuntu2404-full.txt"), &recorded, nil)

	submission, report, err := service.CollectForSubmission(context.Background(), assignment)
	if err != nil {
		t.Fatal(err)
	}
	if report == nil || report.ServerEntryName != "甲入口" || report.ItemCounts[models.SoftwareCategorySystemPackage] != 1041 {
		t.Fatalf("客户端采集报告 = %+v", report)
	}
	if submission.DeviceName != "工作站" || submission.DeviceID != "device-id" ||
		submission.ServerID != servers["甲入口"].ID || submission.Sudo ||
		submission.CommandSetVersion != POSIXCommandSetVersion ||
		submission.SystemType != "ubuntu" || submission.Failed ||
		len(submission.Items) <= 1041 {
		t.Fatalf("客户端待上交结果 = %+v", submission)
	}
	if recorded.server.ID != servers["甲入口"].ID || recorded.sudo {
		t.Fatalf("客户端采集没有按分派条目执行：%+v", recorded)
	}
	if items, err := st.SoftwareObservedItemsForDevice("工作站"); err != nil || len(items) != 0 {
		t.Fatalf("客户端本地不应保存实测条目：items=%d err=%v", len(items), err)
	}
	if status, err := st.SoftwareCollectionStatusForDevice("工作站"); err != nil ||
		!status.LastSuccessAt.IsZero() || status.Collector.IsClient() {
		t.Fatalf("客户端本地不应保存采集状态：%+v err=%v", status, err)
	}
}

// TestCollectForSubmissionWindowsV2 验证客户端实例用第 2 版 Windows 命令集
// 采集得到与权威端一致的结果：待上交结果的版本、系统类型与条目数和权威
// 端对同样本的一次采集相同，客户端本地仍不落库。
func TestCollectForSubmissionWindowsV2(t *testing.T) {
	service, st := newCollectService(t)
	servers := addCollectDeviceWithShell(t, st, "工作站", models.ShellPowerShell, "甲入口")
	assignment := models.SoftwareCollectionAssignment{
		DeviceName: "工作站", DeviceID: "device-id", ServerID: servers["甲入口"].ID,
		ServerName: "甲入口",
	}
	output := windowsFixtureOutput(t, "win10-powershell-full-v2-derived.txt")
	service.RemoteExec = replayCollectExec(output, &recordedExec{}, nil)

	submission, report, err := service.CollectForSubmission(context.Background(), assignment)
	if err != nil {
		t.Fatal(err)
	}
	if report == nil || report.ServerEntryName != "甲入口" || report.CommandSetVersion != "windows-v2" {
		t.Fatalf("客户端第 2 版采集报告 = %+v", report)
	}
	if submission.DeviceName != "工作站" || submission.DeviceID != "device-id" ||
		submission.CommandSetVersion != "windows-v2" || submission.SystemType != "windows" ||
		submission.Failed || len(submission.Items) == 0 {
		t.Fatalf("客户端第 2 版待上交结果 = %+v", submission)
	}
	// 客户端本地不落库：状态与实测条目都为空。
	if items, err := st.SoftwareObservedItemsForDevice("工作站"); err != nil || len(items) != 0 {
		t.Fatalf("客户端本地不应保存实测条目：items=%d err=%v", len(items), err)
	}
	if status, err := st.SoftwareCollectionStatusForDevice("工作站"); err != nil ||
		!status.LastSuccessAt.IsZero() {
		t.Fatalf("客户端本地不应保存采集状态：%+v err=%v", status, err)
	}

	// 同样本交给权威端链路采集一次：结果必须与客户端一致。
	authorityReport, err := service.Collect(context.Background(), CollectRequest{
		Source: CLISource(), DeviceName: "工作站", ServerEntry: "甲入口",
	})
	if err != nil {
		t.Fatal(err)
	}
	if authorityReport.CommandSetVersion != submission.CommandSetVersion ||
		authorityReport.SystemType != submission.SystemType ||
		!maps.Equal(authorityReport.ItemCounts, submission.ItemCounts) {
		t.Fatalf("权威端与客户端的第 2 版结果不一致：authority=%+v submission=%+v",
			authorityReport, submission)
	}
	items, err := st.SoftwareObservedItemsForDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != len(submission.Items) {
		t.Fatalf("权威端落库条目数 = %d，客户端待上交条目数 = %d", len(items), len(submission.Items))
	}
}

// TestCollectForSubmissionFailure 验证客户端执行失败时生成失败结果，仍不
// 在本地排队或落状态。
func TestCollectForSubmissionFailure(t *testing.T) {
	service, st := newCollectService(t)
	servers := addCollectDevice(t, st, "工作站", "甲入口")
	assignment := models.SoftwareCollectionAssignment{
		DeviceName: "工作站", ServerID: servers["甲入口"].ID,
	}
	var recorded recordedExec
	service.RemoteExec = replayCollectExec("", &recorded, context.DeadlineExceeded)

	submission, _, err := service.CollectForSubmission(context.Background(), assignment)
	if err != nil {
		t.Fatal(err)
	}
	if !submission.Failed || !strings.Contains(submission.Error, "context deadline exceeded") ||
		len(submission.Items) != 0 {
		t.Fatalf("失败结果 = %+v", submission)
	}
	if status, err := st.SoftwareCollectionStatusForDevice("工作站"); err != nil || !status.LastFailureAt.IsZero() {
		t.Fatalf("客户端本地不应保存失败状态：%+v err=%v", status, err)
	}
}
