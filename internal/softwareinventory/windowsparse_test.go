package softwareinventory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// windowsFixtureOutput 读入 Windows 固定命令集的真实输出夹具。
func windowsFixtureOutput(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "windows", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestParseWindowsFullFixtures 用 PowerShell 条目与 cmd 条目两份完整真实输
// 出覆盖注册表系统版本、64 位与 32 位卸载项、端口反查、服务、BIOS 与显卡
// 驱动。
func TestParseWindowsFullFixtures(t *testing.T) {
	tests := []struct {
		fixture      string
		buildNumber  string
		udpPort      int
		unknownCount int
	}{
		{fixture: "win10-powershell-full.txt", buildNumber: "19045", udpPort: 51820, unknownCount: 34},
		{fixture: "win10-cmd-full.txt", buildNumber: "22621", udpPort: 52785, unknownCount: 14},
	}
	for _, test := range tests {
		t.Run(test.fixture, func(t *testing.T) {
			collection, err := parseWindowsInventoryOutput(windowsFixtureOutput(t, test.fixture))
			if err != nil {
				t.Fatal(err)
			}
			// 夹具是第 1 版输出；钉住字面量，保证第 1 版样例仍按第 1
			// 版解析。
			if collection.CommandSetVersion != "windows-v1" ||
				collection.SystemType != "windows" || collection.Incomplete {
				t.Fatalf("解析结论 = 版本 %q、系统 %q、不完整 %v、说明 %v",
					collection.CommandSetVersion, collection.SystemType,
					collection.Incomplete, collection.Notes)
			}

			operatingSystem := findObservedItem(
				t, collection.Items, models.SoftwareCategoryOperatingSystem, "Windows 10 Pro",
			)
			if operatingSystem.Version != "22H2" || operatingSystem.Architecture != "AMD64" {
				t.Fatalf("注册表系统版本条目 = %+v", operatingSystem)
			}
			for _, item := range collection.Items {
				if item.Category == models.SoftwareCategoryDriverFirmware && item.Name == "Windows 内核" {
					t.Fatalf("固定命令集没有 Windows 内核条目，解析器不应额外推导：%+v", item)
				}
			}

			docker := findObservedItem(
				t, collection.Items, models.SoftwareCategorySystemPackage, "Docker Desktop",
			)
			if docker.Version == "" || docker.Architecture != "64" ||
				docker.BelongsTo != "Docker Inc." {
				t.Fatalf("64 位注册表卸载项 = %+v", docker)
			}
			if test.buildNumber == "19045" {
				synology := findObservedItem(
					t, collection.Items, models.SoftwareCategorySystemPackage, "Synology Drive Client",
				)
				if synology.Version != "7.2.0.13258" || synology.Architecture != "32" {
					t.Fatalf("32 位注册表卸载项 = %+v", synology)
				}
			}

			netbird := findObservedItem(
				t, collection.Items, models.SoftwareCategorySystemPackage, "Netbird",
			)
			if netbird.Version == "" || findObservedPort(netbird, "127.0.0.1", 41731, "tcp") == nil {
				t.Fatalf("注册表卸载项应认领同名监听进程：%+v", netbird)
			}
			if port := findObservedPort(netbird, "0.0.0.0", test.udpPort, "udp"); port == nil ||
				port.Exposure != models.SoftwareExposureAllAddresses {
				t.Fatalf("Netbird 的 UDP 监听端口 = %+v", netbird.ListeningPorts)
			}

			sshService := findObservedItem(
				t, collection.Items, models.SoftwareCategoryServiceProgram, "sshd",
			)
			if findObservedPort(sshService, "0.0.0.0", 22, "tcp") == nil ||
				sshService.ExecutablePath == "" || sshService.ProcessName != "sshd" {
				t.Fatalf("监听进程应反查到服务：%+v", sshService)
			}

			bios := findObservedItem(
				t, collection.Items, models.SoftwareCategoryDriverFirmware, "BIOS",
			)
			if bios.Version == "" {
				t.Fatalf("BIOS 版本条目 = %+v", bios)
			}
			if test.buildNumber == "19045" {
				gpu := findObservedItem(
					t, collection.Items, models.SoftwareCategoryDriverFirmware, "NVIDIA GeForce RTX 3060",
				)
				if gpu.Version != "32.0.15.6070" {
					t.Fatalf("显卡驱动版本条目 = %+v", gpu)
				}
			}

			if countObserved(collection.Items, models.SoftwareCategoryUnidentified) != test.unknownCount {
				t.Fatalf("未识别进程数量 = %d，应为 %d",
					countObserved(collection.Items, models.SoftwareCategoryUnidentified), test.unknownCount)
			}
			ferryman := findObservedItemWithPort(t, collection.Items, "127.0.0.1", 15700, "tcp")
			if ferryman.Category != models.SoftwareCategoryUnidentified ||
				ferryman.ExecutablePath == "" || ferryman.ProcessName != "ferryman" {
				t.Fatalf("看得到进程但注册表认不出的监听进程 = %+v", ferryman)
			}
		})
	}
}

// TestParseWindowsV2FullFixture 钉住第 2 版输出的解析口径：操作系统条目带
// 完整四段版本号并收编系统自带服务的监听端口；第三方监听程序带文件版本；
// 未识别进程条目名优先取产品名。
func TestParseWindowsV2FullFixture(t *testing.T) {
	// 派生夹具，待替换为真实输出。
	collection, err := parseWindowsInventoryOutput(
		windowsFixtureOutput(t, "win10-powershell-full-v2-derived.txt"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if collection.CommandSetVersion != "windows-v2" ||
		collection.SystemType != "windows" || collection.Incomplete {
		t.Fatalf("解析结论 = 版本 %q、系统 %q、不完整 %v、说明 %v",
			collection.CommandSetVersion, collection.SystemType,
			collection.Incomplete, collection.Notes)
	}

	operatingSystem := findObservedItem(
		t, collection.Items, models.SoftwareCategoryOperatingSystem, "Windows 10 Pro",
	)
	if operatingSystem.Version != "10.0.19045.6456" {
		t.Fatalf("操作系统条目版本 = %q，应为完整四段版本号", operatingSystem.Version)
	}
	if operatingSystem.ProcessID != 0 {
		t.Fatalf("系统自带服务的端口来自多个进程，操作系统条目不应带进程编号：%+v",
			operatingSystem)
	}
	if findObservedPort(operatingSystem, "::", 135, "tcp") == nil ||
		findObservedPort(operatingSystem, "::", 445, "tcp") == nil {
		t.Fatalf("操作系统条目应收编系统自带服务的监听端口 135 与 445：%+v",
			operatingSystem.ListeningPorts)
	}

	// 系统自带服务不单独成条：服务程序类别里查不到它们。
	for _, serviceName := range []string{"RpcSs", "CDPSvc", "Spooler", "EventLog", "Dnscache"} {
		for _, item := range collection.Items {
			if item.Category == models.SoftwareCategoryServiceProgram && item.Name == serviceName {
				t.Fatalf("系统自带服务 %s 不应单独成条：%+v", serviceName, item)
			}
		}
	}
	// 第三方服务带文件版本。
	sshService := findObservedItem(
		t, collection.Items, models.SoftwareCategoryServiceProgram, "sshd",
	)
	if sshService.Version != "9.9.2.0" {
		t.Fatalf("sshd 服务程序条目应带文件版本 9.9.2.0：%+v", sshService)
	}

	// 未识别进程条目名优先取产品名；读不到文件版本的条目版本为空。
	deepSeek := findObservedItem(
		t, collection.Items, models.SoftwareCategoryUnidentified, "DeepSeek Harness",
	)
	if deepSeek.Version != "1.2.3.4" || deepSeek.ProcessName != "DeepSeek Harness" ||
		deepSeek.ExecutablePath == "" {
		t.Fatalf("未识别进程 DeepSeek Harness 应带文件版本与可执行文件路径：%+v", deepSeek)
	}
	antFeeding := findObservedItem(
		t, collection.Items, models.SoftwareCategoryUnidentified, "ant-feeding-log",
	)
	if antFeeding.Version != "" {
		t.Fatalf("没有版本资源的可执行文件应留空版本：%+v", antFeeding)
	}
}

// TestParseWindowsV1FallsBackWhenVersionKeysMissing 钉住第 2 版解析对老系
// 统的回退：主版本或次版本号键缺失时，操作系统条目版本回退第 1 版口径，
// 不拼出不完整的四段版本号。
func TestParseWindowsV1FallsBackWhenVersionKeysMissing(t *testing.T) {
	// 派生夹具：去掉 os 节的两个版本号键，模拟 Windows 10 之前的老系统。
	base := windowsFixtureOutput(t, "win10-powershell-full-v2-derived.txt")
	trimmed := strings.Replace(base, "CurrentMajorVersionNumber\t10\r\n", "", 1)
	trimmed = strings.Replace(trimmed, "CurrentMinorVersionNumber\t0\r\n", "", 1)
	collection, err := parseWindowsInventoryOutput(trimmed)
	if err != nil {
		t.Fatal(err)
	}
	operatingSystem := findObservedItem(
		t, collection.Items, models.SoftwareCategoryOperatingSystem, "Windows 10 Pro",
	)
	if operatingSystem.Version != "22H2" {
		t.Fatalf("版本号键缺失时操作系统条目版本应回退 DisplayVersion：%+v", operatingSystem)
	}
}

// TestParseWindowsNoProcessNamesFixture 覆盖权限不足的输出：进程名与可执
// 行文件都看不到时，监听端口必须落成未识别进程。
func TestParseWindowsNoProcessNamesFixture(t *testing.T) {
	// 派生夹具，待替换为真实输出。
	collection, err := parseWindowsInventoryOutput(
		windowsFixtureOutput(t, "win10-cmd-no-process-names-derived.txt"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if collection.Incomplete {
		t.Fatalf("权限不足但各节完整的输出不应标为不完整：%v", collection.Notes)
	}
	if countObserved(collection.Items, models.SoftwareCategoryUnidentified) != 36 {
		t.Fatalf("看不到进程名的未识别进程数量 = %d",
			countObserved(collection.Items, models.SoftwareCategoryUnidentified))
	}
	for _, item := range collection.Items {
		if item.Category != models.SoftwareCategoryUnidentified {
			continue
		}
		if item.ProcessName != "" || item.ExecutablePath != "" || len(item.ListeningPorts) == 0 {
			t.Fatalf("看不到进程名的端口应保留为未识别进程：%+v", item)
		}
	}
}

// TestWindowsInstalledStableKeyIgnoresRegistryKey 验证注册表卸载项升级后
// 仍使用同一个稳定键；显示名称与发布者冲突时才用注册表键名区分。
func TestWindowsInstalledStableKeyIgnoresRegistryKey(t *testing.T) {
	base := parsedSection{lines: []string{
		"64bit\tOpenSSH\t9.9p1\tOpenSSH\tHKLM\\old",
	}}
	oldItems, _ := buildWindowsInstalledItems(base)
	upgraded, _ := buildWindowsInstalledItems(parsedSection{lines: []string{
		"64bit\tOpenSSH\t10.0p1\tOpenSSH\tHKLM\\new",
	}})
	if len(oldItems) != 1 || len(upgraded) != 1 {
		t.Fatalf("注册表卸载项数量异常：%+v %+v", oldItems, upgraded)
	}
	if oldItems[0].StableKey != upgraded[0].StableKey {
		t.Fatalf("同一软件升级并迁移注册表键名后稳定键变化：%q -> %q",
			oldItems[0].StableKey, upgraded[0].StableKey)
	}

	conflict, _ := buildWindowsInstalledItems(parsedSection{lines: []string{
		"64bit\tOpenSSH\t9.9p1\tOpenSSH\tHKLM\\one",
		"64bit\tOpenSSH\t10.0p1\tOpenSSH\tHKLM\\two",
	}})
	if len(conflict) != 2 || conflict[0].StableKey == conflict[1].StableKey {
		t.Fatalf("显示名称、位数、发布者都相同时必须用注册表键名区分：%+v", conflict)
	}
}

// TestParseWindowsTruncatedFixture 覆盖 listen-udp 节中途截断的输出：已完
// 成节与截断前已收到的行保留，整体标为不完整。
func TestParseWindowsTruncatedFixture(t *testing.T) {
	collection, err := parseWindowsInventoryOutput(
		windowsFixtureOutput(t, "win10-cmd-truncated.txt"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !collection.Incomplete {
		t.Fatal("截断输出必须标为不完整")
	}
	if collection.SystemType != "windows" {
		t.Fatalf("系统类型 = %q", collection.SystemType)
	}
	if countObserved(collection.Items, models.SoftwareCategorySystemPackage) == 0 ||
		countObserved(collection.Items, models.SoftwareCategoryUnidentified) == 0 {
		t.Fatalf("截断前已解析的注册表卸载项与监听端口应保留：%+v", collection.Items)
	}
	for _, note := range collection.Notes {
		if note == "listen-udp 节未完成" {
			return
		}
	}
	t.Fatalf("截断节应写明未完成：%v", collection.Notes)
}
