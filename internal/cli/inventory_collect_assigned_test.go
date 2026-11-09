package cli

import (
	"context"
	"strings"
	"testing"
	"time"

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

// TestInventoryCollectAssignedClientPlaceholder 验证客户端分支目前给出明
// 确的后续版本提示；客户端在线取分派的路径由后续票实现。
func TestInventoryCollectAssignedClientPlaceholder(t *testing.T) {
	err := inventoryCollectAssignedClient(context.Background())
	if err == nil || !strings.Contains(err.Error(), "后续版本") {
		t.Fatalf("客户端占位错误 = %v", err)
	}
}
