package softwareinventory

import (
	"testing"

	"ssh-manager-mcp/internal/models"
)

// TestParseSynologyFullFixture 用 DSM 7.1 的完整真实输出覆盖：系统版本、
// 套件版本拆分、监听端口反查套件、未识别进程、容器镜像与运行状态。
func TestParseSynologyFullFixture(t *testing.T) {
	collection, err := parsePOSIXInventoryOutput(posixFixtureOutput(t, "dsm7-full.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if collection.Incomplete {
		t.Fatalf("完整输出不应标为不完整：%v", collection.Notes)
	}
	operatingSystem := findObservedItem(
		t, collection.Items, models.SoftwareCategoryOperatingSystem, "Synology DSM",
	)
	if operatingSystem.Version != "7.1.1" || operatingSystem.Architecture != "x86_64" {
		t.Fatalf("DSM 系统条目 = %+v", operatingSystem)
	}
	if packages := countObserved(collection.Items, models.SoftwareCategorySystemPackage); packages != 32 {
		t.Fatalf("套件数量 = %d", packages)
	}
	node12 := findObservedItem(
		t, collection.Items, models.SoftwareCategorySystemPackage, "Node.js_v12",
	)
	if node12.Version != "12.22.12-2031" {
		t.Fatalf("Node.js_v12 版本应取最后两段：%+v", node12)
	}
	bios := findObservedItem(t, collection.Items, models.SoftwareCategoryDriverFirmware, "BIOS")
	if bios.Version != "M.034" || bios.BelongsTo != "INSYDE Corp." {
		t.Fatalf("DSM 固件条目 = %+v", bios)
	}
	docker := findObservedItem(t, collection.Items, models.SoftwareCategorySystemPackage, "Docker")
	if docker.Version != "20.10.3-1308" ||
		findObservedPort(docker, "0.0.0.0", 1688, "tcp") == nil ||
		findObservedPort(docker, "0.0.0.0", 8000, "tcp") == nil {
		t.Fatalf("Docker 套件监听端口 = %+v", docker.ListeningPorts)
	}
	drive := findObservedItem(
		t, collection.Items, models.SoftwareCategorySystemPackage, "SynologyDrive",
	)
	if port := findObservedPort(drive, "0.0.0.0", 6690, "tcp"); port == nil ||
		port.Exposure != models.SoftwareExposureAllAddresses {
		t.Fatalf("SynologyDrive 监听端口 = %+v", drive.ListeningPorts)
	}
	netbird := findObservedItem(t, collection.Items, models.SoftwareCategoryUnidentified, "netbird")
	if findObservedPort(netbird, "0.0.0.0", 41185, "udp") == nil ||
		netbird.ExecutablePath != "/usr/local/bin/netbird" {
		t.Fatalf("手工安装的 netbird 应为未识别进程：%+v", netbird)
	}
	if images := countObserved(collection.Items, models.SoftwareCategoryContainerImage); images != 346 {
		t.Fatalf("容器镜像数量 = %d", images)
	}
	gitea := findObservedItem(t, collection.Items, models.SoftwareCategoryContainerImage, "gitea/gitea")
	if gitea.Version != "1.22.2" || !gitea.ContainerRunning || gitea.ContainerDigest == "" {
		t.Fatalf("gitea 容器镜像条目 = %+v", gitea)
	}
}

// TestParseSynologyNoProcessNamesFixture 覆盖 DSM 6.2 普通用户输出：看不
// 到进程名的监听端口落成未识别进程，Docker 被拒绝的节保留为空并说明结
// 果不完整，已能读到的系统与套件仍保留。
func TestParseSynologyNoProcessNamesFixture(t *testing.T) {
	collection, err := parsePOSIXInventoryOutput(
		posixFixtureOutput(t, "dsm6-no-process-names.txt"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !collection.Incomplete {
		t.Fatal("Docker 节非零退出码应标为不完整")
	}
	operatingSystem := findObservedItem(
		t, collection.Items, models.SoftwareCategoryOperatingSystem, "Synology DSM",
	)
	if operatingSystem.Version != "6.2.3" {
		t.Fatalf("DSM 系统条目 = %+v", operatingSystem)
	}
	if packages := countObserved(collection.Items, models.SoftwareCategorySystemPackage); packages != 12 {
		t.Fatalf("套件数量 = %d", packages)
	}
	port139 := findObservedItemWithPort(t, collection.Items, "0.0.0.0", 139, "tcp")
	if port139.Category != models.SoftwareCategoryUnidentified ||
		port139.ProcessName != "" || port139.Name != "未识别进程" {
		t.Fatalf("看不到进程名的端口应为未识别进程：%+v", port139)
	}
	if images := countObserved(collection.Items, models.SoftwareCategoryContainerImage); images != 0 {
		t.Fatalf("Docker 被拒时不应生成镜像条目，得到 %d 条", images)
	}
}

// TestNetstatListenSectionDropsInvalidPorts 验证端口 0 与 65536 都被丢弃，
// 不能因为解析函数漏返回错误而被收进清单。
func TestNetstatListenSectionDropsInvalidPorts(t *testing.T) {
	lines := parseNetstatListenSection(parsedSection{lines: []string{
		"tcp 0 0 0.0.0.0:22 0.0.0.0:* LISTEN 100/sshd",
		"tcp 0 0 0.0.0.0:0 0.0.0.0:* LISTEN 101/bad-zero",
		"tcp 0 0 0.0.0.0:65536 0.0.0.0:* LISTEN 102/bad-large",
	}})
	if len(lines) != 1 || lines[0].port != 22 {
		t.Fatalf("越界端口应丢弃，合法端口应保留：%+v", lines)
	}
}

// TestParseSynologyTruncatedFixture 覆盖 container-images 节中途截断：系
// 统、监听、套件与 exe 节保留，容器条目不生成并标为不完整。
func TestParseSynologyTruncatedFixture(t *testing.T) {
	collection, err := parsePOSIXInventoryOutput(posixFixtureOutput(t, "dsm7-truncated.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !collection.Incomplete {
		t.Fatal("截断输出应标为不完整")
	}
	if packages := countObserved(collection.Items, models.SoftwareCategorySystemPackage); packages != 32 {
		t.Fatalf("截断前完成的套件节应保留，得到 %d 条", packages)
	}
	docker := findObservedItem(t, collection.Items, models.SoftwareCategorySystemPackage, "Docker")
	if findObservedPort(docker, "0.0.0.0", 1688, "tcp") == nil {
		t.Fatalf("截断前的监听端口应保留：%+v", docker.ListeningPorts)
	}
	if images := countObserved(collection.Items, models.SoftwareCategoryContainerImage); images != 40 {
		t.Fatalf("镜像节截断前已读到的 40 条应保留，得到 %d 条", images)
	}
}
