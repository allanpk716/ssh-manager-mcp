package softwareinventory

import (
	"os"
	"path/filepath"
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
			if collection.CommandSetVersion != WindowsCommandSetVersion ||
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
