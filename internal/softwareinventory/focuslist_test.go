package softwareinventory

import (
	"os"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/store"
)

// seedPOSIXSample 读一份 POSIX 固定命令集输出样例，经现有解析器解析后落
// 到一台全新设备的实测清单里，返回挂在临时存储上的领域模块。
func seedPOSIXSample(t *testing.T, deviceName, fixture string) *Manager {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "posix", fixture))
	if err != nil {
		t.Fatal(err)
	}
	collection, err := parsePOSIXInventoryOutput(string(data))
	if err != nil {
		t.Fatal(err)
	}
	return seedParsedSample(t, deviceName, collection)
}

// seedWindowsSample 读一份 Windows 固定命令集输出样例，作用同
// seedPOSIXSample。
func seedWindowsSample(t *testing.T, deviceName, fixture string) *Manager {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "windows", fixture))
	if err != nil {
		t.Fatal(err)
	}
	collection, err := parseWindowsInventoryOutput(string(data))
	if err != nil {
		t.Fatal(err)
	}
	return seedParsedSample(t, deviceName, collection)
}

func seedParsedSample(t *testing.T, deviceName string, collection *ParsedCollection) *Manager {
	t.Helper()
	manager, st := newTestManager(t)
	mustDevice(t, st, deviceName, true)
	if err := st.ReplaceSoftwareObservedItems(deviceName, store.SoftwareObservedWrite{
		Items: collection.Items, CommandSetVersion: collection.CommandSetVersion,
		SystemType: collection.SystemType, Incomplete: collection.Incomplete,
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	return manager
}

// querySampleDevice 对样例设备做一次查询并返回它的条目，供重点清单断言
// 使用。
func querySampleDevice(t *testing.T, manager *Manager, deviceName string, options QueryOptions) []QueryItem {
	t.Helper()
	options.Device = deviceName
	options.Visibility = AllDevicesVisible()
	result, err := manager.Query(options)
	if err != nil {
		t.Fatal(err)
	}
	device := findQueryDevice(result, deviceName)
	if device == nil {
		t.Fatalf("样例设备 %s 不在查询结果里：%+v", deviceName, result.Devices)
	}
	return device.Items
}

// countItemsByName 统计条目名出现次数，名字大小写保持原样。
func countItemsByName(items []QueryItem) map[string]int {
	counts := make(map[string]int, len(items))
	for _, item := range items {
		counts[item.Name]++
	}
	return counts
}

// findItemByNameAndCategory 在条目里按类别与名字找第一条，找不到时终止
// 测试。同名条目可能跨类别（如 nginx 既是系统包又是容器镜像），断言时必
// 须锁定类别。
func findItemByNameAndCategory(t *testing.T, items []QueryItem, category Category, name string) QueryItem {
	t.Helper()
	for _, item := range items {
		if item.Category == category && item.Name == name {
			return item
		}
	}
	t.Fatalf("查询结果里找不到 %s 类别的条目 %s：%+v", category, name, items)
	return QueryItem{}
}

// TestQueryDefaultReturnsFocusListFromUbuntuSample 钉住重点清单的默认查询
// 口径：没有监听端口的系统包被排除，带监听端口的系统包（监听端口的程
// 序）、操作系统、内核、驱动固件、容器镜像与未识别进程全部保留。
func TestQueryDefaultReturnsFocusListFromUbuntuSample(t *testing.T) {
	manager := seedPOSIXSample(t, "样例工作站", "ubuntu2404-full.txt")

	items := querySampleDevice(t, manager, "样例工作站", QueryOptions{})
	counts := countItemsByName(items)
	if counts["libc6"] != 0 {
		t.Fatalf("默认查询不应返回没有监听端口的系统包 libc6：%+v", items)
	}
	for _, name := range []string{
		"Ubuntu", "Linux 内核", "BIOS", "NVIDIA 显卡驱动",
		"nginx", "docker-ce", "openssh-server", "python3.12-minimal", "systemd-resolved",
		"netbird", "未识别进程", "python", "litellm",
	} {
		if counts[name] == 0 {
			t.Fatalf("默认查询缺少重点条目 %s：%+v", name, items)
		}
	}
	containerImages := 0
	for _, item := range items {
		if item.Category == CategoryContainerImage {
			containerImages++
		}
	}
	if containerImages != 9 {
		t.Fatalf("容器镜像条目 = %d，应为 9：%+v", containerImages, items)
	}
	nginx := findItemByNameAndCategory(t, items, CategorySystemPackage, "nginx")
	if nginx.Category != CategorySystemPackage || len(nginx.ListeningPorts) != 3 {
		t.Fatalf("nginx 应是带 3 个监听端口的系统包条目：%+v", nginx)
	}
	dockerCE := findItemByNameAndCategory(t, items, CategorySystemPackage, "docker-ce")
	if dockerCE.Category != CategorySystemPackage || len(dockerCE.ListeningPorts) != 3 {
		t.Fatalf("docker-ce 应是带 3 个监听端口的系统包条目：%+v", dockerCE)
	}

	complete := querySampleDevice(t, manager, "样例工作站", QueryOptions{IncludeSystemPackages: true})
	if countItemsByName(complete)["libc6"] != 1 {
		t.Fatalf("包含系统包的完整查询应出现 libc6 恰好一次：%+v", complete)
	}
}
