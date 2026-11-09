package softwareinventory

import (
	"context"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// TestCollectPOSIXSystemsThroughSQLite 用真实 SQLite 临时库与回放执行函数
// 覆盖一次采集落库：macOS、群晖 DSM 全量，iStoreOS 与 ARM 板只保留系统
// 版本和监听端口。
func TestCollectPOSIXSystemsThroughSQLite(t *testing.T) {
	tests := []struct {
		device     string
		fixture    string
		system     string
		incomplete bool
		category   models.SoftwareCategory
		name       string
		version    string
	}{
		{
			device: "Mac 工作站", fixture: "macos-full.txt", system: "darwin",
			category: models.SoftwareCategoryOperatingSystem, name: "macOS", version: "27.0.1",
		},
		{
			device: "群晖", fixture: "dsm7-full.txt", system: "synology-dsm",
			category: models.SoftwareCategorySystemPackage, name: "Docker", version: "20.10.3-1308",
		},
		{
			device: "路由器", fixture: "istoreos-full.txt", system: "istoreos",
			category: models.SoftwareCategoryOperatingSystem, name: "iStoreOS", version: "24.10.5",
		},
		{
			// 手写夹具，待替换为真实输出。
			device: "ARM 板", fixture: "arm-board-handwritten.txt", system: "linux",
			category: models.SoftwareCategoryOperatingSystem, name: "Embedded Linux", version: "1.2",
		},
	}
	for _, test := range tests {
		service, st := newCollectService(t)
		addCollectDevice(t, st, test.device, "entry-a")
		var recorded recordedExec
		service.RemoteExec = replayCollectExec(
			posixFixtureOutput(t, test.fixture), &recorded, nil,
		)

		report, err := service.Collect(context.Background(), CollectRequest{DeviceName: test.device})
		if err != nil {
			t.Fatalf("%s 采集失败：%v", test.device, err)
		}
		expectedCommand, err := BuildInventoryCommand(models.ShellPOSIX)
		if err != nil {
			t.Fatal(err)
		}
		if report.SystemType != test.system || report.CommandSetVersion != POSIXCommandSetVersion ||
			report.Incomplete != test.incomplete || recorded.command != expectedCommand {
			t.Fatalf("%s 采集报告或命令不正确：report=%+v", test.device, report)
		}
		items, err := st.SoftwareObservedItemsForDevice(test.device)
		if err != nil {
			t.Fatal(err)
		}
		item := findObservedItem(t, items, test.category, test.name)
		if item.Version != test.version || item.StableKey == "" || item.CollectedAt != service.Now() {
			t.Fatalf("%s 的 %s 条目 = %+v", test.device, test.name, item)
		}
		status, err := st.SoftwareCollectionStatusForDevice(test.device)
		if err != nil {
			t.Fatal(err)
		}
		if status.SystemType != test.system || status.CommandSetVersion != POSIXCommandSetVersion ||
			status.LastIncomplete != test.incomplete || status.LastSuccessAt != service.Now() {
			t.Fatalf("%s 的采集状态 = %+v", test.device, status)
		}
	}
}
