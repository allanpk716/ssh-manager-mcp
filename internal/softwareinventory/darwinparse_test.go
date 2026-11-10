package softwareinventory

import (
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// TestParseDarwinFullFixture 用 macOS 27 的完整真实输出覆盖：系统版本、
// 应用版本、Homebrew 软件包统一地址、监听端口反查应用或包、未识别进程、
// 多版本 Homebrew 条目与容器镜像。
func TestParseDarwinFullFixture(t *testing.T) {
	collection, err := parsePOSIXInventoryOutput(posixFixtureOutput(t, "macos-full.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if collection.Incomplete {
		t.Fatalf("完整输出不应标为不完整：%v", collection.Notes)
	}
	operatingSystem := findObservedItem(
		t, collection.Items, models.SoftwareCategoryOperatingSystem, "macOS",
	)
	if operatingSystem.Version != "27.0.1" || operatingSystem.Architecture != "arm64" {
		t.Fatalf("macOS 系统条目 = %+v", operatingSystem)
	}
	kernel := findObservedItem(
		t, collection.Items, models.SoftwareCategoryDriverFirmware, "Darwin 内核",
	)
	if kernel.Version != "27.0.0" {
		t.Fatalf("Darwin 内核条目 = %+v", kernel)
	}
	if apps := countObservedCollectionApplications(collection.Items); apps != 13 {
		t.Fatalf("应用数量 = %d", apps)
	}
	orca := findObservedItem(t, collection.Items, models.SoftwareCategorySystemPackage, "Orca")
	if orca.Version != "1.4.253" || orca.PackageURL != "" {
		t.Fatalf("Orca 应用条目 = %+v", orca)
	}
	rustDesk := findObservedItem(t, collection.Items, models.SoftwareCategorySystemPackage, "RustDesk")
	if rustDesk.Version != "1.5.0" ||
		findObservedPort(rustDesk, "*", 21119, "udp") == nil ||
		findObservedPort(rustDesk, "*", 65158, "udp") == nil {
		t.Fatalf("RustDesk 应用监听端口 = %+v", rustDesk.ListeningPorts)
	}

	if packages := countObserved(collection.Items, models.SoftwareCategorySystemPackage); packages != 87 {
		t.Fatalf("应用与 Homebrew 条目数量 = %d", packages)
	}
	goPackage := findObservedItem(t, collection.Items, models.SoftwareCategorySystemPackage, "go")
	if goPackage.Version != "1.27.3" || goPackage.PackageURL != "pkg:brew/go@1.27.3" {
		t.Fatalf("Homebrew go 条目 = %+v", goPackage)
	}
	openssl4 := findObservedItem(t, collection.Items, models.SoftwareCategorySystemPackage, "openssl@4")
	if openssl4.Version != "4.1.0" || openssl4.PackageURL != "pkg:brew/openssl@4@4.1.0" {
		t.Fatalf("Homebrew 多版本第一项 = %+v", openssl4)
	}
	node := findObservedItem(t, collection.Items, models.SoftwareCategorySystemPackage, "node")
	if port := findObservedPort(node, "127.0.0.1", 8989, "tcp"); port == nil ||
		port.Exposure != models.SoftwareExposureLocal {
		t.Fatalf("Homebrew node 应监听 127.0.0.1:8989：%+v", node.ListeningPorts)
	}
	netbird := findObservedItem(t, collection.Items, models.SoftwareCategoryUnidentified, "netbird")
	if findObservedPort(netbird, "*", 51820, "udp") == nil ||
		netbird.ExecutablePath != "/usr/local/bin/netbird" {
		t.Fatalf("手工安装的 netbird 应为未识别进程：%+v", netbird)
	}
	if images := countObserved(collection.Items, models.SoftwareCategoryContainerImage); images != 10 {
		t.Fatalf("容器镜像数量 = %d", images)
	}
}

// TestParseDarwinNoProcessNamesFixture 覆盖普通用户运行 macOS 采集：输出
// 本身完整，lsof 看不到其他用户进程，因此 root 进程的监听端口不会凭空
// 生成；当前用户的应用与 Homebrew 条目仍保留。
func TestParseDarwinNoProcessNamesFixture(t *testing.T) {
	collection, err := parsePOSIXInventoryOutput(
		posixFixtureOutput(t, "macos-no-process-names.txt"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if collection.Incomplete {
		t.Fatalf("权限不足但命令成功的输出不应标为不完整：%v", collection.Notes)
	}
	if apps := countObservedCollectionApplications(collection.Items); apps != 13 {
		t.Fatalf("应用数量 = %d", apps)
	}
	if packages := countObserved(collection.Items, models.SoftwareCategorySystemPackage); packages != 87 {
		t.Fatalf("应用与 Homebrew 条目数量 = %d", packages)
	}
	for _, hidden := range []struct {
		name string
		port int
	}{
		{name: "launchd", port: 22},
		{name: "sshmgr", port: 7878},
	} {
		for _, item := range collection.Items {
			for _, listening := range item.ListeningPorts {
				if item.ProcessName == hidden.name && listening.Port == hidden.port {
					t.Fatalf("普通用户看不到的 %s 进程不应出现：%+v", hidden.name, item)
				}
			}
		}
	}
}

// TestDarwinBrewStableKeyIgnoresVersion 验证 Homebrew 软件包升级后仍使用
// 同一个稳定键；同名多版本只在这一批输出里冲突时才追加版本。
func TestDarwinBrewStableKeyIgnoresVersion(t *testing.T) {
	oneVersion := buildDarwinBrewItems(parsedSection{lines: []string{
		"formula\topenssh 9.9p1",
	}})
	upgraded := buildDarwinBrewItems(parsedSection{lines: []string{
		"formula\topenssh 10.0p1",
	}})
	if len(oneVersion) != 1 || len(upgraded) != 1 {
		t.Fatalf("Homebrew 条目数量异常：%+v %+v", oneVersion, upgraded)
	}
	if oneVersion[0].StableKey != upgraded[0].StableKey {
		t.Fatalf("同一软件包升级后稳定键变化：%q -> %q",
			oneVersion[0].StableKey, upgraded[0].StableKey)
	}

	multiVersion := buildDarwinBrewItems(parsedSection{lines: []string{
		"formula\tpython 3.12 3.13",
	}})
	if len(multiVersion) != 2 || multiVersion[0].StableKey == multiVersion[1].StableKey {
		t.Fatalf("同名多版本必须用版本区分：%+v", multiVersion)
	}
}

// TestParseDarwinTruncatedFixture 覆盖 apps 节中途截断：系统、监听进程与
// exe 节保留，应用及后续节不生成，整体标为不完整。
func TestParseDarwinTruncatedFixture(t *testing.T) {
	collection, err := parsePOSIXInventoryOutput(posixFixtureOutput(t, "macos-truncated.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !collection.Incomplete {
		t.Fatal("截断输出应标为不完整")
	}
	operatingSystem := findObservedItem(
		t, collection.Items, models.SoftwareCategoryOperatingSystem, "macOS",
	)
	if operatingSystem.Version != "27.0.1" {
		t.Fatalf("已完成的 os 节应保留：%+v", operatingSystem)
	}
	netbird := findObservedItem(t, collection.Items, models.SoftwareCategoryUnidentified, "netbird")
	if findObservedPort(netbird, "*", 51820, "udp") == nil {
		t.Fatalf("截断前的监听端口应保留：%+v", netbird)
	}
	if apps := countObservedCollectionApplications(collection.Items); apps != 10 {
		t.Fatalf("apps 节截断前已读到的 10 条应用应保留，得到 %d 条", apps)
	}
	if packages := countObserved(collection.Items, models.SoftwareCategorySystemPackage); packages != 10 {
		t.Fatalf("未出现的 brew 节不应生成条目，得到 %d 条", packages)
	}
}

func countObservedCollectionApplications(items []models.SoftwareObservedItem) int {
	total := 0
	for _, item := range items {
		if item.Category == models.SoftwareCategorySystemPackage &&
			item.BelongsTo != "" && !strings.HasPrefix(item.BelongsTo, "Homebrew ") {
			total++
		}
	}
	return total
}
