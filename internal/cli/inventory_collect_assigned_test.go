package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/softwareinventory"
	"ssh-manager-mcp/internal/store"
)

// TestInventoryCollectAssignedHelp 验证帮助文本写明三种系统计划任务的调
// 用示例、专门退出码，并声明不替 owner 安装计划任务。
func TestInventoryCollectAssignedHelp(t *testing.T) {
	out, _, err := runInventoryRoot(t, "", "inventory", "collect-assigned", "--help")
	if err != nil {
		t.Fatalf("查看帮助失败：%v", err)
	}
	for _, want := range []string{
		"launchd", "cron", "Windows 计划任务", "schtasks",
		"0 6 * * *", "不替你安装", "退出码", "5 部分设备采集失败",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("帮助文本缺少 %q：\n%s", want, out)
		}
	}
}

// TestInventoryCollectAssignedLocalRound 覆盖本地路径：输出每台设备的结论，
// 部分设备失败时返回专门退出码 5；全部成功或没有到期设备时退出码 0。
func TestInventoryCollectAssignedLocalRound(t *testing.T) {
	newSecretsEnv(t)
	now := time.Unix(1770000000, 0)
	roundReports := make(chan *softwareinventory.SchedulerRoundReport, 3)
	roundReports <- &softwareinventory.SchedulerRoundReport{
		Due: 2, SkippedNotDue: 1, Succeeded: 1, Failed: 1,
		Devices: []softwareinventory.SchedulerDeviceReport{
			{
				DeviceName: "工作站",
				Report: &softwareinventory.CollectionReport{
					DeviceName: "工作站", ServerEntryName: "entry-a", Sudo: true,
					CommandSetVersion: "linux-v1", SystemType: "ubuntu",
					ItemCounts: map[models.SoftwareCategory]int{
						models.SoftwareCategorySystemPackage: 3,
					},
					StartedAt: now, EndedAt: now,
				},
			},
			{DeviceName: "备份机", Err: "连接超时"},
		},
	}
	roundReports <- &softwareinventory.SchedulerRoundReport{
		Due: 0, SkippedNotDue: 2, Succeeded: 0, Failed: 0,
	}
	roundReports <- &softwareinventory.SchedulerRoundReport{
		Due: 0, SkippedNotDue: 2, Succeeded: 0, Failed: 0,
	}
	previous := inventoryCollectAssignedRunner
	inventoryCollectAssignedRunner = func(_ context.Context, _ *store.Store) (*softwareinventory.SchedulerRoundReport, error) {
		return <-roundReports, nil
	}
	t.Cleanup(func() { inventoryCollectAssignedRunner = previous })

	out, _, err := runInventoryRoot(t, "", "inventory", "collect-assigned")
	if err == nil {
		t.Fatal("部分设备失败时命令必须报错")
	}
	if code := ExitCodeFor(err); code != exitCodePartialCollectFailure {
		t.Fatalf("部分设备失败的退出码 = %d，应为 %d", code, exitCodePartialCollectFailure)
	}
	for _, want := range []string{"到期 2 台", "成功 1 台", "失败 1 台", "未到期跳过 1 台", "工作站", "entry-a", "备份机", "连接超时"} {
		if !strings.Contains(out, want) {
			t.Fatalf("一轮输出缺少 %q：\n%s", want, out)
		}
	}

	out = mustInventoryCli(t, "", "inventory", "collect-assigned")
	if !strings.Contains(out, "没有到期设备") {
		t.Fatalf("没有到期设备时的输出：\n%s", out)
	}

	jsonOut := mustInventoryCli(t, "", "inventory", "collect-assigned", "--json")
	if !strings.Contains(jsonOut, "skipped_not_due") {
		t.Fatalf("JSON 输出应包含统计字段：\n%s", jsonOut)
	}
}

// TestInventoryCollectAssignedClientBranch 覆盖客户端分支的命令行结论：输
// 出一轮报告，部分失败用退出码 5，权威端不可达用退出码 4。
func TestInventoryCollectAssignedClientBranch(t *testing.T) {
	newSecretsEnv(t)
	now := time.Unix(1770000000, 0)
	reports := make(chan *softwareinventory.SchedulerRoundReport, 2)
	reports <- &softwareinventory.SchedulerRoundReport{
		Due: 1, Succeeded: 1,
		Devices: []softwareinventory.SchedulerDeviceReport{{
			DeviceName: "工作站",
			Report: &softwareinventory.CollectionReport{
				DeviceName: "工作站", ServerEntryName: "入口",
				StartedAt: now, EndedAt: now,
			},
		}},
	}
	reports <- &softwareinventory.SchedulerRoundReport{Due: 1, Failed: 1}
	previous := inventoryCollectAssignedClient
	inventoryCollectAssignedClient = func(_ context.Context, _ string) (*softwareinventory.SchedulerRoundReport, error) {
		return <-reports, nil
	}
	t.Cleanup(func() { inventoryCollectAssignedClient = previous })

	var out strings.Builder
	err := runInventoryCollectAssignedClient(context.Background(), "", &out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "工作站") || !strings.Contains(out.String(), "入口") {
		t.Fatalf("客户端采集输出：\n%s", out.String())
	}

	out.Reset()
	err = runInventoryCollectAssignedClient(context.Background(), "", &out)
	if ExitCodeFor(err) != exitCodePartialCollectFailure {
		t.Fatalf("部分失败退出码 = %d，应为 5：%v", ExitCodeFor(err), err)
	}

	unreachableForwarder, uerr := clientops.NewSoftwareInventoryForwarder(clientops.CacheCred{})
	if uerr != nil {
		t.Fatal(uerr)
	}
	defer unreachableForwarder.Close()
	_, unreachable := unreachableForwarder.SoftwareCollectionAssignments(context.Background())
	inventoryCollectAssignedClient = func(context.Context, string) (*softwareinventory.SchedulerRoundReport, error) {
		return nil, unreachable
	}
	if code := ExitCodeFor(runInventoryCollectAssignedClient(context.Background(), "", &out)); code != exitCodeAuthorityUnreachable {
		t.Fatalf("权威端不可达退出码 = %d，应为 4：%v", code, unreachable)
	}
}

// TestInventoryCollectAssignedClientUsesLock 验证计划任务路径先拿本地锁文
// 件；锁被 MCP 桥持有时直接失败，不并发执行。
func TestInventoryCollectAssignedClientUsesLock(t *testing.T) {
	newSecretsEnv(t)
	t.Setenv("SSHMGR_CACHE_DIR", t.TempDir())
	path, err := clientops.SoftwareInventoryCollectionLockPath("")
	if err != nil {
		t.Fatal(err)
	}
	lock, err := clientops.AcquireSoftwareInventoryCollectionLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	previous := inventoryCollectAssignedClient
	inventoryCollectAssignedClient = func(context.Context, string) (*softwareinventory.SchedulerRoundReport, error) {
		t.Fatal("锁被持有时不应执行客户端采集")
		return nil, nil
	}
	t.Cleanup(func() { inventoryCollectAssignedClient = previous })
	err = runInventoryCollectAssignedClient(context.Background(), "", &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "另一份软件清单采集正在执行") {
		t.Fatalf("锁忙时应直接失败：%v", err)
	}
}
