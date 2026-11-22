package softwareinventory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
		DistroID: collection.DistroID, DistroVersionID: collection.DistroVersionID,
		UnusedImageCount: collection.UnusedImageCount,
		PendingUpdates:   collection.PendingUpdates,
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

// portText 把一个监听端口渲染成断言用的紧凑文本。
func portText(port ListeningPort) string {
	return fmt.Sprintf("%s:%d/%s", port.Address, port.Port, port.Protocol)
}

// assertPortOrder 校验一组监听端口与期望的「地址:端口/协议」序列一致。
func assertPortOrder(t *testing.T, ports []ListeningPort, wanted []string) {
	t.Helper()
	got := make([]string, 0, len(ports))
	for _, port := range ports {
		got = append(got, portText(port))
	}
	if strings.Join(got, "|") != strings.Join(wanted, "|") {
		t.Fatalf("监听端口顺序 = %v，想要 %v", got, wanted)
	}
}

// TestQueryMergesUnidentifiedProcessPortsFromWindowsSample 钉住未识别进程
// 的合并口径：同一程序（可执行文件路径或进程名）的多个端口合成一行，端
// 口按暴露级别从高到低排序；服务程序与纯系统包的取舍不变。
func TestQueryMergesUnidentifiedProcessPortsFromWindowsSample(t *testing.T) {
	manager := seedWindowsSample(t, "样例工作站", "win10-powershell-full.txt")

	items := querySampleDevice(t, manager, "样例工作站", QueryOptions{})
	counts := countItemsByName(items)
	if counts["DeepSeek Harness"] != 1 {
		t.Fatalf("DeepSeek Harness 的两个端口应合并为一行：%+v", items)
	}
	deepSeek := findItemByNameAndCategory(t, items, CategoryUnidentified, "DeepSeek Harness")
	if len(deepSeek.ListeningPorts) != 2 {
		t.Fatalf("DeepSeek Harness 合并后应有 2 个监听端口：%+v", deepSeek.ListeningPorts)
	}
	// 3081 监听所有地址，应排在仅本机监听的 3080 前面。
	assertPortOrder(t, deepSeek.ListeningPorts, []string{"0.0.0.0:3081/tcp", "127.0.0.1:3080/tcp"})

	if counts["dasHost"] != 1 {
		t.Fatalf("dasHost 的三个端口应合并为一行：%+v", items)
	}
	dasHost := findItemByNameAndCategory(t, items, CategoryUnidentified, "dasHost")
	assertPortOrder(t, dasHost.ListeningPorts, []string{
		":::3702/udp", "0.0.0.0:59154/udp", ":::59155/udp",
	})

	if counts["System"] != 1 {
		t.Fatalf("System 的五组端口应合并为一行：%+v", items)
	}
	system := findItemByNameAndCategory(t, items, CategoryUnidentified, "System")
	if len(system.ListeningPorts) != 20 {
		t.Fatalf("System 合并后应有 20 个监听端口（5 组端口号）：%+v", system.ListeningPorts)
	}
	// 445 与 5357 监听所有地址排在前面，137、138、139 监听特定地址排在
	// 后面；同组内按地址排序。
	assertPortOrder(t, system.ListeningPorts[:2], []string{":::445/tcp", ":::5357/tcp"})
	assertPortOrder(t, system.ListeningPorts[2:8], []string{
		"172.27.32.1:137/udp", "172.28.0.1:137/udp", "198.51.100.102:137/udp",
		"198.51.179.1:137/udp", "198.51.240.1:137/udp", "203.0.113.120:137/udp",
	})

	if findItemByNameAndCategory(t, items, CategoryServiceProgram, "RpcSs").Name != "RpcSs" {
		t.Fatalf("服务程序 RpcSs 应保留在默认查询里：%+v", items)
	}
	if countItemsByName(items)["Bitwarden"] != 0 {
		t.Fatalf("默认查询不应返回没有监听端口的系统包 Bitwarden：%+v", items)
	}

	complete := querySampleDevice(t, manager, "样例工作站", QueryOptions{IncludeSystemPackages: true})
	if countItemsByName(complete)["Bitwarden"] != 1 {
		t.Fatalf("完整查询应包含没有监听端口的系统包 Bitwarden：%+v", complete)
	}
	if countItemsByName(complete)["Netbird"] != 1 {
		t.Fatalf("完整查询应包含带监听端口的系统包 Netbird：%+v", complete)
	}
	netbird := findItemByNameAndCategory(t, complete, CategorySystemPackage, "Netbird")
	if len(netbird.ListeningPorts) != 3 {
		t.Fatalf("Netbird 应带 3 个监听端口：%+v", netbird.ListeningPorts)
	}
}

// TestQueryDefaultReturnsFocusListFromWindowsV2Sample 钉住第 2 版 Windows
// 输出的重点清单口径：系统自带服务不出现，操作系统条目带完整四段版本号
// 与收编的监听端口，第三方程序带文件版本，读不到版本标缺版本。
func TestQueryDefaultReturnsFocusListFromWindowsV2Sample(t *testing.T) {
	manager := seedWindowsSample(t, "样例工作站", "win10-powershell-full-v2-derived.txt")

	items := querySampleDevice(t, manager, "样例工作站", QueryOptions{})
	counts := countItemsByName(items)
	for _, name := range []string{"RpcSs", "CDPSvc", "Spooler", "dasHost", "System"} {
		if counts[name] != 0 {
			t.Fatalf("重点清单不应再出现系统自带服务 %s：%+v", name, items)
		}
	}

	operatingSystem := findItemByNameAndCategory(
		t, items, CategoryOperatingSystem, "Windows 10 Pro",
	)
	if operatingSystem.Version != "10.0.19045.6456" || operatingSystem.MissingVersion {
		t.Fatalf("操作系统条目应为完整四段版本号：%+v", operatingSystem)
	}
	if findQueryPort(operatingSystem, 135, "tcp") == nil ||
		findQueryPort(operatingSystem, 445, "tcp") == nil {
		t.Fatalf("操作系统条目应收编系统自带服务的监听端口 135 与 445：%+v",
			operatingSystem.ListeningPorts)
	}

	deepSeek := findItemByNameAndCategory(t, items, CategoryUnidentified, "DeepSeek Harness")
	if deepSeek.Version != "1.2.3.4" || deepSeek.MissingVersion ||
		deepSeek.ExecutablePath == "" || len(deepSeek.ListeningPorts) != 2 {
		t.Fatalf("DeepSeek Harness 应带文件版本、可执行文件路径与合并端口：%+v", deepSeek)
	}
	antFeeding := findItemByNameAndCategory(t, items, CategoryUnidentified, "ant-feeding-log")
	if !antFeeding.MissingVersion {
		t.Fatalf("读不到文件版本的条目应带缺版本标志：%+v", antFeeding)
	}

	complete := querySampleDevice(t, manager, "样例工作站", QueryOptions{IncludeSystemPackages: true})
	if countItemsByName(complete)["Bitwarden"] != 1 {
		t.Fatalf("完整查询应包含没有监听端口的系统包 Bitwarden：%+v", complete)
	}
}

// findQueryPort 在一条查询条目的监听端口里按端口号与协议找第一个匹配。
func findQueryPort(item QueryItem, port int, protocol string) *ListeningPort {
	for i := range item.ListeningPorts {
		if item.ListeningPorts[i].Port == port && item.ListeningPorts[i].Protocol == protocol {
			return &item.ListeningPorts[i]
		}
	}
	return nil
}

// TestQuerySortsListeningPortsByExposureFromUbuntuSample 钉住端口排序与
// 不误合并的边界：监听所有地址的端口排在仅本机监听的端口前；单端口程
// 序与不满足合并条件（可执行文件路径与进程名都空）的条目保持原样。
func TestQuerySortsListeningPortsByExposureFromUbuntuSample(t *testing.T) {
	manager := seedPOSIXSample(t, "样例工作站", "ubuntu2404-full.txt")

	items := querySampleDevice(t, manager, "样例工作站", QueryOptions{})
	dockerCE := findItemByNameAndCategory(t, items, CategorySystemPackage, "docker-ce")
	assertPortOrder(t, dockerCE.ListeningPorts, []string{
		"0.0.0.0:20508/tcp", "127.0.0.1:18000/tcp", "127.0.0.1:18001/tcp",
	})
	litellm := findItemByNameAndCategory(t, items, CategoryUnidentified, "litellm")
	if len(litellm.ListeningPorts) != 1 || litellm.ListeningPorts[0].Port != 4000 {
		t.Fatalf("litellm 单端口条目应保持不变：%+v", litellm)
	}
	unidentified := findItemByNameAndCategory(t, items, CategoryUnidentified, "未识别进程")
	if len(unidentified.ListeningPorts) != 2 {
		t.Fatalf("可执行文件路径与进程名都空的条目不应合并拆散：%+v", unidentified)
	}
}

// TestQueryMarksMissingVersionFromUbuntuSample 钉住缺版本标志：版本为空的
// 条目带标志，有版本的条目不带。
func TestQueryMarksMissingVersionFromUbuntuSample(t *testing.T) {
	manager := seedPOSIXSample(t, "样例工作站", "ubuntu2404-full.txt")

	items := querySampleDevice(t, manager, "样例工作站", QueryOptions{})
	for _, name := range []string{"litellm", "python", "未识别进程"} {
		item := findItemByNameAndCategory(t, items, CategoryUnidentified, name)
		if !item.MissingVersion {
			t.Fatalf("版本为空的条目 %s 应带缺版本标志：%+v", name, item)
		}
	}
	nginx := findItemByNameAndCategory(t, items, CategorySystemPackage, "nginx")
	dockerCE := findItemByNameAndCategory(t, items, CategorySystemPackage, "docker-ce")
	ubuntu := findItemByNameAndCategory(t, items, CategoryOperatingSystem, "Ubuntu")
	for _, item := range []QueryItem{nginx, dockerCE, ubuntu} {
		if item.MissingVersion {
			t.Fatalf("有版本的条目 %s 不应带缺版本标志：%+v", item.Name, item)
		}
	}
}
