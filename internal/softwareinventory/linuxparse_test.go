package softwareinventory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// fixtureOutput 读入一份真实机器录得的固定命令集输出。夹具来自
// testdata/posix/README.md 描述的 POSIX 机器。
func fixtureOutput(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "posix", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func findObservedItem(
	t *testing.T,
	items []models.SoftwareObservedItem,
	category models.SoftwareCategory,
	name string,
) *models.SoftwareObservedItem {
	t.Helper()
	for i := range items {
		if items[i].Category == category && items[i].Name == name {
			return &items[i]
		}
	}
	t.Fatalf("找不到条目 %s/%s；全部条目共 %d 条", category, name, len(items))
	return nil
}

func findObservedItemWithPort(
	t *testing.T,
	items []models.SoftwareObservedItem,
	address string,
	port int,
	protocol string,
) *models.SoftwareObservedItem {
	t.Helper()
	for i := range items {
		if findObservedPort(&items[i], address, port, protocol) != nil {
			return &items[i]
		}
	}
	t.Fatalf("找不到监听 %s:%d/%s 的条目", address, port, protocol)
	return nil
}

func findObservedPort(
	item *models.SoftwareObservedItem,
	address string,
	port int,
	protocol string,
) *models.SoftwareListeningPort {
	for i := range item.ListeningPorts {
		listening := &item.ListeningPorts[i]
		if listening.Address == address && listening.Port == port && listening.Protocol == protocol {
			return listening
		}
	}
	return nil
}

func countObserved(items []models.SoftwareObservedItem, category models.SoftwareCategory) int {
	total := 0
	for _, item := range items {
		if item.Category == category {
			total++
		}
	}
	return total
}

// TestParsePOSIXUbuntuFullFixture 用 Ubuntu 24.04 的完整真实输出覆盖：类别与精确
// 版本、软件包统一地址、监听端口反查到系统包、两种未识别进程、暴露级别
// 三档推导、容器镜像的标签摘要与运行状态、BIOS 与显卡驱动。
func TestParsePOSIXUbuntuFullFixture(t *testing.T) {
	collection, err := parsePOSIXInventoryOutput(posixFixtureOutput(t, "ubuntu2404-full.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if collection.CommandSetVersion != POSIXCommandSetVersion {
		t.Fatalf("命令集版本 = %q", collection.CommandSetVersion)
	}
	if collection.SystemType != "ubuntu" {
		t.Fatalf("系统类型应由第一节识别结果决定，得到 %q", collection.SystemType)
	}
	if collection.Incomplete {
		t.Fatalf("完整夹具不应标为不完整：%v", collection.Notes)
	}

	operatingSystem := findObservedItem(t, collection.Items, models.SoftwareCategoryOperatingSystem, "Ubuntu")
	if operatingSystem.Version != "24.04" || operatingSystem.Architecture != "x86_64" {
		t.Fatalf("操作系统条目 = %+v", operatingSystem)
	}
	kernel := findObservedItem(t, collection.Items, models.SoftwareCategoryDriverFirmware, "Linux 内核")
	if kernel.Version != "6.8.0-146-generic" {
		t.Fatalf("内核版本 = %+v", kernel)
	}
	if countObserved(collection.Items, models.SoftwareCategorySystemPackage) != 1041 {
		t.Fatalf("系统包数量 = %d", countObserved(collection.Items, models.SoftwareCategorySystemPackage))
	}

	openssh := findObservedItem(t, collection.Items, models.SoftwareCategorySystemPackage, "openssh-server")
	if openssh.Version != "1:9.6p1-3ubuntu13.20" {
		t.Fatalf("openssh-server 版本应原样保留：%+v", openssh)
	}
	if openssh.PackageURL != "pkg:deb/ubuntu/openssh-server@1:9.6p1-3ubuntu13.20?arch=amd64" {
		t.Fatalf("openssh-server 软件包统一地址 = %q", openssh.PackageURL)
	}
	if port := findObservedPort(openssh, "0.0.0.0", 22, "tcp"); port == nil || port.Exposure != models.SoftwareExposureAllAddresses {
		t.Fatalf("openssh-server 监听 0.0.0.0:22 应为监听所有地址：%+v", openssh.ListeningPorts)
	}
	if port := findObservedPort(openssh, "[::]", 22, "tcp"); port == nil || port.Exposure != models.SoftwareExposureAllAddresses {
		t.Fatalf("openssh-server 监听 [::]:22 应为监听所有地址：%+v", openssh.ListeningPorts)
	}

	dockerCE := findObservedItem(t, collection.Items, models.SoftwareCategorySystemPackage, "docker-ce")
	if port := findObservedPort(dockerCE, "0.0.0.0", 20508, "tcp"); port == nil || port.Exposure != models.SoftwareExposureAllAddresses {
		t.Fatalf("docker-ce 反查应挂 20508 端口：%+v", dockerCE.ListeningPorts)
	}
	if port := findObservedPort(dockerCE, "127.0.0.1", 18001, "tcp"); port == nil || port.Exposure != models.SoftwareExposureLocal {
		t.Fatalf("docker-ce 回环端口应为仅本机：%+v", dockerCE.ListeningPorts)
	}
	python312 := findObservedItem(t, collection.Items, models.SoftwareCategorySystemPackage, "python3.12-minimal")
	if port := findObservedPort(python312, "100.64.0.77", 8018, "tcp"); port == nil ||
		port.Exposure != models.SoftwareExposureSpecificAddress {
		t.Fatalf("监听特定地址推导失败：%+v", python312.ListeningPorts)
	}

	miniconda := findObservedItem(t, collection.Items, models.SoftwareCategoryUnidentified, "python")
	if miniconda.ExecutablePath != "/root/miniconda3/envs/us500-clean/bin/python3.11" ||
		miniconda.ProcessName != "python" || miniconda.Version != "" {
		t.Fatalf("认不出的进程应为未识别进程：%+v", miniconda)
	}
	if miniconda.ProcessID != 2123656 {
		t.Fatalf("监听进程的进程编号应保存：%+v", miniconda)
	}
	if port := findObservedPort(miniconda, "0.0.0.0", 8765, "tcp"); port == nil ||
		port.Exposure != models.SoftwareExposureAllAddresses {
		t.Fatalf("未识别 python 的端口 = %+v", miniconda.ListeningPorts)
	}
	litellm := findObservedItem(t, collection.Items, models.SoftwareCategoryUnidentified, "litellm")
	if litellm.ExecutablePath != "/usr/bin/python3.13" {
		t.Fatalf("litellm 的可执行文件 = %+v", litellm)
	}
	wireguard := findObservedItem(t, collection.Items, models.SoftwareCategoryUnidentified, "未识别进程")
	if findObservedPort(wireguard, "0.0.0.0", 51820, "udp") == nil ||
		findObservedPort(wireguard, "[::]", 51820, "udp") == nil ||
		len(wireguard.ListeningPorts) != 2 {
		t.Fatalf("看不到进程名的两个地址族端口应合并为一条未识别进程：%+v", wireguard)
	}
	if countObserved(collection.Items, models.SoftwareCategoryUnidentified) != 3 {
		t.Fatalf("未识别进程数量 = %d", countObserved(collection.Items, models.SoftwareCategoryUnidentified))
	}

	litellmImage := findObservedItem(t, collection.Items, models.SoftwareCategoryContainerImage, "ghcr.io/berriai/litellm")
	if litellmImage.ContainerTag != "main-stable" || litellmImage.ContainerDigest != "" ||
		!litellmImage.ContainerRunning || litellmImage.BelongsTo != "docker" ||
		litellmImage.PackageURL != "" {
		t.Fatalf("容器镜像条目 = %+v", litellmImage)
	}
	nginxImage := findObservedItem(t, collection.Items, models.SoftwareCategoryContainerImage, "nginx")
	if !nginxImage.ContainerRunning || nginxImage.ContainerTag != "" ||
		nginxImage.ContainerDigest != "sha256:8f029c543423e3eac6b08254718bc31eb75633b1e448026b6616927baa7d4bfe" ||
		nginxImage.PackageURL != "pkg:docker/nginx@sha256:8f029c543423e3eac6b08254718bc31eb75633b1e448026b6616927baa7d4bfe" {
		t.Fatalf("nginx 镜像 = %+v", nginxImage)
	}
	cuda := findObservedItem(t, collection.Items, models.SoftwareCategoryContainerImage, "nvidia/cuda")
	if cuda.ContainerRunning || cuda.PackageURL !=
		"pkg:docker/nvidia/cuda@sha256:80d4d9ac041242f6ae5d05f9be262b3374e0e0b8bb5a49c6c3e94e192cde4a44?tag=12.4.0-base-ubuntu22.04" {
		t.Fatalf("nvidia/cuda 镜像 = %+v", cuda)
	}
	if countObserved(collection.Items, models.SoftwareCategoryContainerImage) != 9 {
		t.Fatalf("容器镜像数量 = %d", countObserved(collection.Items, models.SoftwareCategoryContainerImage))
	}

	bios := findObservedItem(t, collection.Items, models.SoftwareCategoryDriverFirmware, "BIOS")
	if bios.Version != "1703" || bios.BelongsTo != "American Megatrends Inc." {
		t.Fatalf("BIOS 条目 = %+v", bios)
	}
	gpu := findObservedItem(t, collection.Items, models.SoftwareCategoryDriverFirmware, "NVIDIA 显卡驱动")
	if gpu.Version != "595.99.02" {
		t.Fatalf("显卡驱动条目 = %+v", gpu)
	}
}

// TestParseLinuxNoProcessNamesFixture 用 Ubuntu 22.04 普通用户的真实输出覆
// 盖权限不足的情况：大部分监听端口看不到进程名，只能生成未识别进程。
func TestParsePOSIXUbuntuNoProcessNamesFixture(t *testing.T) {
	collection, err := parsePOSIXInventoryOutput(posixFixtureOutput(t, "ubuntu2204-no-process-names.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if collection.Incomplete {
		t.Fatalf("权限不足夹具本身是完整输出：%v", collection.Notes)
	}
	if countObserved(collection.Items, models.SoftwareCategorySystemPackage) != 1239 {
		t.Fatalf("系统包数量 = %d", countObserved(collection.Items, models.SoftwareCategorySystemPackage))
	}
	// 看不到进程名的端口按“端口+协议+可执行文件路径”合并：0.0.0.0 与
	// [::] 上的同一端口算同一条未识别进程，因此 10 行监听合并成 7 条。
	if unidentified := countObserved(collection.Items, models.SoftwareCategoryUnidentified); unidentified != 7 {
		t.Fatalf("未识别进程数量 = %d", unidentified)
	}
	// `*` 是 ss 对双栈通配地址的另一种写法，应推导为监听所有地址。
	for _, expected := range []struct {
		address  string
		port     int
		exposure models.SoftwareExposure
	}{
		{"*", 11434, models.SoftwareExposureAllAddresses},
		{"0.0.0.0", 8824, models.SoftwareExposureAllAddresses},
		{"[::]", 22, models.SoftwareExposureAllAddresses},
		{"127.0.0.53", 53, models.SoftwareExposureLocal},
	} {
		item := findObservedItemWithPort(t, collection.Items, expected.address, expected.port, "tcp")
		if item.ListeningPorts[0].Exposure != expected.exposure {
			t.Fatalf("%s:%d 暴露级别 = %+v", expected.address, expected.port, item.ListeningPorts)
		}
	}
	vscode := findObservedItem(t, collection.Items, models.SoftwareCategoryUnidentified, "code-2c33b50b16")
	if vscode.ExecutablePath != "/home/user/.vscode-server/code-2c33b50b16c472efe58357d1d2a8a5414bc23fbf" {
		t.Fatalf("看得到进程名但反查不到包： %+v", vscode)
	}
	training := findObservedItem(t, collection.Items, models.SoftwareCategoryContainerImage, "sl1000_image")
	if !training.ContainerRunning || training.ContainerTag != "v1" {
		t.Fatalf("运行中的容器镜像 = %+v", training)
	}
}

// TestParseLinuxTruncatedFixture 覆盖输出中途被截断的情况：已解析的节保
// 留，未出现的节与没有 STATUS 行的节都记为不完整。
func TestParsePOSIXUbuntuTruncatedFixture(t *testing.T) {
	collection, err := parsePOSIXInventoryOutput(posixFixtureOutput(t, "ubuntu2404-truncated.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !collection.Incomplete {
		t.Fatal("截断输出必须标为不完整")
	}
	if collection.SystemType != "ubuntu" {
		t.Fatalf("系统类型 = %q", collection.SystemType)
	}
	operatingSystem := findObservedItem(t, collection.Items, models.SoftwareCategoryOperatingSystem, "Ubuntu")
	if operatingSystem.Version != "24.04" {
		t.Fatalf("已完成的节应保留：%+v", operatingSystem)
	}
	packages := countObserved(collection.Items, models.SoftwareCategorySystemPackage)
	if packages == 0 || packages >= 1041 {
		t.Fatalf("截断前的系统包行应保留且不应被当成完整结果，得到 %d", packages)
	}
	for _, category := range []models.SoftwareCategory{
		models.SoftwareCategoryUnidentified, models.SoftwareCategoryContainerImage,
	} {
		if countObserved(collection.Items, category) != 0 {
			t.Fatalf("截断后未出现的节不应有条目：%s", category)
		}
	}
}

// TestParsePOSIXProtocolEdges 用合成输出钉住输出协议的边界：工具不存在
// （ABSENT）不算不完整，非零退出码与缺 END 行都算不完整，完全没有开头标
// 记则无法解析。
func TestParsePOSIXProtocolEdges(t *testing.T) {
	output := "SSHMGR-INVENTORY BEGIN posix-v2\n" +
		"SSHMGR-INVENTORY SECTION os\n" +
		"kernel-name=Linux\n" +
		"file=/etc/os-release\n" +
		"PRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\"\n" +
		"NAME=\"Debian GNU/Linux\"\n" +
		"VERSION_ID=\"12\"\n" +
		"ID=debian\n" +
		"kernel=6.1.0-1-amd64\n" +
		"machine=x86_64\n" +
		"SSHMGR-INVENTORY STATUS os 0\n" +
		"SSHMGR-INVENTORY ABSENT container-images\n" +
		"SSHMGR-INVENTORY SECTION packages\n" +
		"bash\t5.2.15-2+b7\tamd64\tii\n" +
		"SSHMGR-INVENTORY STATUS packages 0\n"
	output += "SSHMGR-INVENTORY ABSENT listen\n" +
		"SSHMGR-INVENTORY ABSENT listen-netstat\n" +
		"SSHMGR-INVENTORY ABSENT listen-lsof\n" +
		"SSHMGR-INVENTORY ABSENT exe\n" +
		"SSHMGR-INVENTORY ABSENT exe-owner\n" +
		"SSHMGR-INVENTORY ABSENT apps\n" +
		"SSHMGR-INVENTORY ABSENT brew\n" +
		"SSHMGR-INVENTORY ABSENT synology-packages\n" +
		"SSHMGR-INVENTORY ABSENT firmware\n" +
		"SSHMGR-INVENTORY ABSENT gpu-driver\n"
	collection, err := parsePOSIXInventoryOutput(output)
	if err != nil {
		t.Fatal(err)
	}
	if !collection.Incomplete {
		t.Fatal("缺少 END 行应标为不完整")
	}
	if collection.SystemType != "debian" {
		t.Fatalf("系统类型 = %q", collection.SystemType)
	}
	bash := findObservedItem(t, collection.Items, models.SoftwareCategorySystemPackage, "bash")
	if bash.PackageURL != "pkg:deb/debian/bash@5.2.15-2+b7?arch=amd64" {
		t.Fatalf("Debian 包统一地址 = %q", bash.PackageURL)
	}
	if countObserved(collection.Items, models.SoftwareCategoryContainerImage) != 0 {
		t.Fatal("ABSENT 节没有条目")
	}

	failed := strings.Replace(output, "STATUS packages 0", "STATUS packages 1", 1) + "SSHMGR-INVENTORY END\n"
	collection, err = parsePOSIXInventoryOutput(failed)
	if err != nil {
		t.Fatal(err)
	}
	if !collection.Incomplete {
		t.Fatal("非零退出码的节应标为不完整")
	}

	if _, err := parsePOSIXInventoryOutput("SSHMGR-INVENTORY END\n"); err == nil {
		t.Fatal("没有开头标记的输出应无法解析")
	}
}

// TestParsePOSIXMissingOSSection 覆盖整节缺失的边界：os 节完全没出现时，
// 不能凭空造出操作系统条目，只把结果标为不完整。
func TestParsePOSIXMissingOSSection(t *testing.T) {
	output := "SSHMGR-INVENTORY BEGIN posix-v2\n" +
		"SSHMGR-INVENTORY SECTION packages\n" +
		"bash\t5.2.15-2+b7\tamd64\tii\n" +
		"SSHMGR-INVENTORY STATUS packages 0\n" +
		"SSHMGR-INVENTORY END\n"
	collection, err := parsePOSIXInventoryOutput(output)
	if err != nil {
		t.Fatal(err)
	}
	if !collection.Incomplete {
		t.Fatal("缺少 os 节应标为不完整")
	}
	if collection.SystemType != "" {
		t.Fatalf("系统类型 = %q", collection.SystemType)
	}
	if countObserved(collection.Items, models.SoftwareCategoryOperatingSystem) != 0 ||
		countObserved(collection.Items, models.SoftwareCategoryDriverFirmware) != 0 {
		t.Fatalf("os 节缺失时不应生成系统条目：%+v", collection.Items)
	}
	if countObserved(collection.Items, models.SoftwareCategorySystemPackage) != 1 {
		t.Fatalf("其他节应照常解析：%+v", collection.Items)
	}
}
