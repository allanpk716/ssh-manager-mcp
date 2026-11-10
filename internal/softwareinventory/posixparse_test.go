package softwareinventory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// posixFixtureOutput 读入 POSIX 固定命令集的真实输出夹具。
func posixFixtureOutput(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "posix", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestParseIStoreOSFullFixture 覆盖 iStoreOS 的基础采集：只生成系统版本
// 与监听端口，容器、固件等不适用节即使有输出也忽略，且完整结果不标为
// 不完整。
func TestParseIStoreOSFullFixture(t *testing.T) {
	collection, err := parsePOSIXInventoryOutput(posixFixtureOutput(t, "istoreos-full.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if collection.Incomplete {
		t.Fatalf("iStoreOS 完整输出不应标为不完整：%v", collection.Notes)
	}
	operatingSystem := findObservedItem(
		t, collection.Items, models.SoftwareCategoryOperatingSystem, "iStoreOS",
	)
	if operatingSystem.Version != "24.10.5" || operatingSystem.Architecture != "x86_64" {
		t.Fatalf("iStoreOS 系统条目 = %+v", operatingSystem)
	}
	uhttpd := findObservedItemWithPort(t, collection.Items, "0.0.0.0", 80, "tcp")
	if uhttpd.Category != models.SoftwareCategoryUnidentified || uhttpd.ProcessName != "uhttpd" {
		t.Fatalf("uhttpd 监听条目 = %+v", uhttpd)
	}
	noProcess := findObservedItemWithPort(t, collection.Items, "0.0.0.0", 32777, "tcp")
	if noProcess.Category != models.SoftwareCategoryUnidentified || noProcess.ProcessName != "" {
		t.Fatalf("看不到进程名的监听条目 = %+v", noProcess)
	}
	for _, category := range []models.SoftwareCategory{
		models.SoftwareCategorySystemPackage, models.SoftwareCategoryServiceProgram,
		models.SoftwareCategoryContainerImage, models.SoftwareCategoryDriverFirmware,
	} {
		if countObserved(collection.Items, category) != 0 {
			t.Fatalf("iStoreOS 第一版不应生成 %s 条目", category)
		}
	}
}

// TestParseBasicSystemIgnoresIrrelevantSectionFailures 钉住 iStoreOS 与
// ARM 板的取舍：不采集的节即使命令失败，也不把结果标为不完整。
func TestParseBasicSystemIgnoresIrrelevantSectionFailures(t *testing.T) {
	output := strings.Join([]string{
		"SSHMGR-INVENTORY BEGIN posix-v2",
		"SSHMGR-INVENTORY SECTION os",
		"kernel-name=Linux",
		"kernel=6.6.119",
		"machine=x86_64",
		"file=/etc/os-release",
		"NAME=\"iStoreOS\"",
		"VERSION_ID=\"24.10.5\"",
		"ID=\"istoreos\"",
		"SSHMGR-INVENTORY STATUS os 0",
		"SSHMGR-INVENTORY SECTION packages",
		"opkg: not found",
		"SSHMGR-INVENTORY STATUS packages 1",
		"SSHMGR-INVENTORY SECTION listen-netstat",
		"Active Internet connections (only servers)",
		"Proto Recv-Q Send-Q Local Address Foreign Address State PID/Program name",
		"tcp 0 0 0.0.0.0:80 0.0.0.0:* LISTEN 7803/uhttpd",
		"SSHMGR-INVENTORY STATUS listen-netstat 0",
		"SSHMGR-INVENTORY SECTION container-images",
		"docker: not found",
		"SSHMGR-INVENTORY STATUS container-images 1",
		"SSHMGR-INVENTORY END",
	}, "\n")
	collection, err := parsePOSIXInventoryOutput(output)
	if err != nil {
		t.Fatal(err)
	}
	if collection.Incomplete {
		t.Fatalf("基础采集应忽略不适用节的失败：%v", collection.Notes)
	}
	if item := findObservedItemWithPort(t, collection.Items, "0.0.0.0", 80, "tcp"); item.ProcessName != "uhttpd" {
		t.Fatalf("监听进程条目 = %+v", item)
	}
}

// TestParseIStoreOSTruncatedFixture 覆盖 listen-netstat 节中途截断：系统
// 版本保留，整体标为不完整。
func TestParseIStoreOSTruncatedFixture(t *testing.T) {
	collection, err := parsePOSIXInventoryOutput(posixFixtureOutput(t, "istoreos-truncated.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !collection.Incomplete {
		t.Fatal("截断输出应标为不完整")
	}
	operatingSystem := findObservedItem(
		t, collection.Items, models.SoftwareCategoryOperatingSystem, "iStoreOS",
	)
	if operatingSystem.Version != "24.10.5" {
		t.Fatalf("已完成的 os 节应保留：%+v", operatingSystem)
	}
	if unidentified := countObserved(collection.Items, models.SoftwareCategoryUnidentified); unidentified != 4 {
		t.Fatalf("截断前已读到的 4 条监听行应保留，得到 %d 条", unidentified)
	}
}

// TestParseARMBoardHandwrittenFixture 覆盖其他 Linux（ARM 板）的基础采
// 集。手写夹具，待替换为真实输出。
func TestParseARMBoardHandwrittenFixture(t *testing.T) {
	collection, err := parsePOSIXInventoryOutput(
		posixFixtureOutput(t, "arm-board-handwritten.txt"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if collection.Incomplete {
		t.Fatalf("ARM 板完整输出不应标为不完整：%v", collection.Notes)
	}
	operatingSystem := findObservedItem(
		t, collection.Items, models.SoftwareCategoryOperatingSystem, "Embedded Linux",
	)
	if operatingSystem.Version != "1.2" || operatingSystem.Architecture != "armv7l" {
		t.Fatalf("ARM 板系统条目 = %+v", operatingSystem)
	}
	dropbear := findObservedItemWithPort(t, collection.Items, "0.0.0.0", 22, "tcp")
	if dropbear.Category != models.SoftwareCategoryUnidentified ||
		dropbear.ExecutablePath != "/usr/sbin/dropbear" {
		t.Fatalf("ARM 板监听进程条目 = %+v", dropbear)
	}
	if countObserved(collection.Items, models.SoftwareCategoryDriverFirmware) != 0 {
		t.Fatal("ARM 板第一版不应生成驱动与固件条目")
	}
}

// TestParsePOSIXSystemIdentification 覆盖系统识别规则：macOS、群晖
// DSM、iStoreOS / OpenWrt、Ubuntu / Debian 与其他 Linux（ARM 板）。
func TestParsePOSIXSystemIdentification(t *testing.T) {
	tests := []struct {
		fixture    string
		system     string
		incomplete bool
	}{
		{fixture: "ubuntu2404-full.txt", system: "ubuntu"},
		{fixture: "macos-full.txt", system: "darwin"},
		{fixture: "dsm7-full.txt", system: "synology-dsm"},
		{fixture: "istoreos-full.txt", system: "istoreos"},
		// 手写夹具，待替换为真实输出。
		{fixture: "arm-board-handwritten.txt", system: "linux"},
	}
	for _, test := range tests {
		collection, err := parsePOSIXInventoryOutput(posixFixtureOutput(t, test.fixture))
		if err != nil {
			t.Fatalf("%s 解析失败：%v", test.fixture, err)
		}
		if collection.SystemType != test.system {
			t.Fatalf("%s 系统识别结果 = %q，应为 %q", test.fixture, collection.SystemType, test.system)
		}
		if collection.CommandSetVersion != POSIXCommandSetVersion {
			t.Fatalf("%s 命令集版本 = %q", test.fixture, collection.CommandSetVersion)
		}
		if collection.Incomplete {
			t.Fatalf("%s 完整输出不应标为不完整：%v", test.fixture, collection.Notes)
		}
	}
}
