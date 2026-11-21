package softwareinventory

import (
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
)

// 本文件钉住 POSIX 固定命令集第 3 版的解析口径：以容器为单位生成容器
// 条目、未使用镜像只计数、发行版编号与待装更新随结果上交；第 2 版样例
// 仍按原规则解析（回归钉住）。

// TestParsePOSIXV3ContainerFixture 覆盖第 3 版样例的主路径：运行中与已停
// 止容器、公开镜像与内网私有镜像、判断不了能否拉取的镜像站、未使用旧镜
// 像只计数，以及认不出的监听程序带可执行文件路径。
func TestParsePOSIXV3ContainerFixture(t *testing.T) {
	collection, err := parsePOSIXInventoryOutput(
		posixFixtureOutput(t, "ubuntu2204-v3-containers.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if collection.CommandSetVersion != "posix-v3" {
		t.Fatalf("命令集版本 = %q，应为 posix-v3", collection.CommandSetVersion)
	}
	if collection.SystemType != "ubuntu" {
		t.Fatalf("系统识别结果 = %q", collection.SystemType)
	}
	if collection.Incomplete {
		t.Fatalf("完整输出不应标为不完整：%v", collection.Notes)
	}
	if collection.DistroID != "ubuntu" || collection.DistroVersionID != "22.04" {
		t.Fatalf("发行版编号 = %q / 版本编号 = %q，应为 ubuntu / 22.04",
			collection.DistroID, collection.DistroVersionID)
	}
	if collection.UnusedImageCount != 2 {
		t.Fatalf("未使用镜像数量 = %d，应为 2（旧版 nginx 行与本地构建行）", collection.UnusedImageCount)
	}
	if count := countObserved(collection.Items, models.SoftwareCategoryContainer); count != 5 {
		t.Fatalf("容器条目 = %d 条，应为 5：%+v", count, collection.Items)
	}
	if count := countObserved(collection.Items, models.SoftwareCategoryContainerImage); count != 0 {
		t.Fatalf("第 3 版不应再逐条列出镜像，得到 %d 条容器镜像条目", count)
	}

	gateway := findObservedItem(t, collection.Items, models.SoftwareCategoryContainer, "nginx")
	if gateway.ContainerName != "gateway" {
		t.Fatalf("nginx 容器条目应带容器名 gateway：%+v", gateway)
	}
	if gateway.Version != "1.25.3" {
		t.Fatalf("nginx 容器条目版本应取镜像自带的版本标签 1.25.3：%+v", gateway)
	}
	if gateway.ContainerTag != "1.25.3" ||
		gateway.ContainerDigest != "sha256:2f1b0af53c34b7d4c9d1d55a30fb8f0e6f8b2f9b0b9e1f1aaaaaaaaaaaaaaaa0" {
		t.Fatalf("nginx 容器条目应带镜像标签与仓库摘要：%+v", gateway)
	}
	if !gateway.ContainerRunning {
		t.Fatal("nginx 容器应处于运行状态")
	}
	if gateway.ImagePullability != models.SoftwareImagePullabilityPublic {
		t.Fatalf("nginx 官方仓库镜像应判断为可公开拉取：%+v", gateway)
	}
	if len(gateway.ListeningPorts) != 1 || gateway.ListeningPorts[0].Address != "0.0.0.0" ||
		gateway.ListeningPorts[0].Port != 8080 || gateway.ListeningPorts[0].Protocol != "tcp" {
		t.Fatalf("nginx 容器应带对外端口 0.0.0.0:8080/tcp：%+v", gateway.ListeningPorts)
	}
	if gateway.ListeningPorts[0].Exposure != models.SoftwareExposureAllAddresses {
		t.Fatalf("0.0.0.0 上的容器端口暴露级别应为监听所有地址：%+v", gateway.ListeningPorts)
	}

	litellm := findObservedItem(
		t, collection.Items, models.SoftwareCategoryContainer, "ghcr.io/berriai/litellm")
	if !litellm.ContainerRunning || litellm.Version != "v1.74.0" {
		t.Fatalf("ghcr.io 公开镜像容器条目 = %+v", litellm)
	}
	if litellm.ImagePullability != models.SoftwareImagePullabilityPublic {
		t.Fatalf("ghcr.io 公开仓库镜像应判断为可公开拉取：%+v", litellm)
	}
	if len(litellm.ListeningPorts) != 0 {
		t.Fatalf("没有主机端口映射的容器不应带监听端口：%+v", litellm)
	}

	mirror := findObservedItem(
		t, collection.Items, models.SoftwareCategoryContainer, "ghcr.nju.edu.cn/berriai/litellm")
	if mirror.ContainerName != "litellm-mirror" {
		t.Fatalf("镜像站容器条目应带容器名 litellm-mirror：%+v", mirror)
	}
	if mirror.ImagePullability != models.SoftwareImagePullabilityUnknown {
		t.Fatalf("判断不了的镜像仓库地址应为未知：%+v", mirror)
	}

	trainer := findObservedItem(
		t, collection.Items, models.SoftwareCategoryContainer, "10.0.0.47:40070/ml-train")
	if trainer.ContainerName != "trainer" || !trainer.ContainerRunning {
		t.Fatalf("内网镜像容器条目 = %+v", trainer)
	}
	if trainer.ImagePullability != models.SoftwareImagePullabilityNotPublic {
		t.Fatalf("内网地址镜像应判断为不可公开拉取：%+v", trainer)
	}
	if trainer.Version != "" {
		t.Fatalf("镜像没有自带版本标签时版本应为空：%+v", trainer)
	}
	if len(trainer.ListeningPorts) != 1 || trainer.ListeningPorts[0].Address != "127.0.0.1" ||
		trainer.ListeningPorts[0].Port != 6006 {
		t.Fatalf("trainer 容器应带对外端口 127.0.0.1:6006/tcp：%+v", trainer.ListeningPorts)
	}
	if trainer.ListeningPorts[0].Exposure != models.SoftwareExposureLocal {
		t.Fatalf("127.0.0.1 上的容器端口暴露级别应为仅本机：%+v", trainer.ListeningPorts)
	}

	var stopped models.SoftwareObservedItem
	stoppedFound := false
	for _, item := range collection.Items {
		if item.Category == models.SoftwareCategoryContainer && item.ContainerName == "trainer-old" {
			stopped = item
			stoppedFound = true
			break
		}
	}
	if !stoppedFound {
		t.Fatalf("找不到容器名为 trainer-old 的容器条目：%+v", collection.Items)
	}
	if stopped.ContainerRunning {
		t.Fatalf("已停止容器条目不应标记运行状态：%+v", stopped)
	}
	if stopped.Version != "0.9.0" {
		t.Fatalf("已停止容器也应带镜像自带的版本标签：%+v", stopped)
	}

	// 认不出的监听程序带可执行文件路径，不运行程序取版本。
	widget := findObservedItemWithPort(t, collection.Items, "0.0.0.0", 9100, "tcp")
	if widget.Category != models.SoftwareCategoryUnidentified ||
		widget.ExecutablePath != "/opt/custom/bin/widget-server" {
		t.Fatalf("认不出的监听程序应带可执行文件路径：%+v", widget)
	}
}

// TestParsePOSIXV2FixtureStillParsesWithV3Parser 钉住向后兼容：第 2 版样
// 例在支持第 3 版的解析器里仍按原规则解析出容器镜像条目，不产生容器条
// 目，也没有设备级新字段。
func TestParsePOSIXV2FixtureStillParsesWithV3Parser(t *testing.T) {
	collection, err := parsePOSIXInventoryOutput(posixFixtureOutput(t, "ubuntu2404-full.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if collection.CommandSetVersion != "posix-v2" {
		t.Fatalf("第 2 版样例的命令集版本 = %q", collection.CommandSetVersion)
	}
	if count := countObserved(collection.Items, models.SoftwareCategoryContainerImage); count != 9 {
		t.Fatalf("第 2 版样例仍应解析出 9 条容器镜像条目，得到 %d 条", count)
	}
	if count := countObserved(collection.Items, models.SoftwareCategoryContainer); count != 0 {
		t.Fatalf("第 2 版样例不应产生容器条目，得到 %d 条", count)
	}
	if collection.DistroID != "" || collection.DistroVersionID != "" || collection.UnusedImageCount != 0 {
		t.Fatalf("第 2 版结果不应带第 3 版的设备级字段：%+v", collection)
	}
	if collection.PendingUpdates != nil {
		t.Fatalf("第 2 版结果不应带待装更新：%+v", collection.PendingUpdates)
	}
}

// TestParsePOSIXV3PendingUpdatesFromAptFixture 覆盖 apt 本地缓存路径：
// 全部可升级明细保留，安全仓库条目标记出来，当前版本从方括号提示里还原。
func TestParsePOSIXV3PendingUpdatesFromAptFixture(t *testing.T) {
	collection, err := parsePOSIXInventoryOutput(
		posixFixtureOutput(t, "ubuntu2204-v3-containers.txt"))
	if err != nil {
		t.Fatal(err)
	}
	pending := collection.PendingUpdates
	if pending == nil {
		t.Fatalf("apt 缓存可读时应带待装更新：%+v", collection)
	}
	if pending.SecurityCount != 2 || len(pending.Items) != 3 {
		t.Fatalf("待装更新安全来源数量 = %d、明细 = %d 条，应为 2、3：%+v",
			pending.SecurityCount, len(pending.Items), pending)
	}
	wantTime := time.Unix(1795136400, 0).UTC()
	if !pending.CacheUpdatedAt.Equal(wantTime) {
		t.Fatalf("apt 缓存时间 = %v，应为 %v", pending.CacheUpdatedAt, wantTime)
	}
	openssl := pending.Items[0]
	if openssl.Name != "openssl" || openssl.CurrentVersion != "3.0.2-0ubuntu1.15" ||
		openssl.AvailableVersion != "3.0.2-0ubuntu1.18" || !openssl.FromSecurityRepo {
		t.Fatalf("openssl 待装更新字段不全：%+v", openssl)
	}
	curl := pending.Items[2]
	if curl.Name != "curl" || curl.CurrentVersion != "7.81.0-1ubuntu1.4" ||
		curl.AvailableVersion != "7.81.0-1ubuntu1.5" || curl.FromSecurityRepo {
		t.Fatalf("curl 应保留为普通更新明细：%+v", curl)
	}
}

// TestParsePOSIXV3PendingUpdatesFromDnfFixture 覆盖 dnf/yum 本地缓存路
// 径：只保留安全更新，提示行跳过，版本纪元原样保留。
func TestParsePOSIXV3PendingUpdatesFromDnfFixture(t *testing.T) {
	collection, err := parsePOSIXInventoryOutput(
		posixFixtureOutput(t, "rocky9-v3-dnf-security.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if collection.SystemType != "linux" || collection.Incomplete {
		t.Fatalf("Rocky 样例应完整解析为 Linux：%+v", collection)
	}
	pending := collection.PendingUpdates
	if pending == nil {
		t.Fatalf("dnf 缓存可读时应带待装更新：%+v", collection)
	}
	if pending.SecurityCount != 3 || len(pending.Items) != 3 {
		t.Fatalf("待装更新安全来源数量 = %d、明细 = %d 条，应为 3、3：%+v",
			pending.SecurityCount, len(pending.Items), pending)
	}
	wantTime := time.Unix(1795050000, 0).UTC()
	if !pending.CacheUpdatedAt.Equal(wantTime) {
		t.Fatalf("dnf 缓存时间 = %v，应为 %v", pending.CacheUpdatedAt, wantTime)
	}
	kernel := pending.Items[1]
	if kernel.Name != "kernel" || kernel.CurrentVersion != "" ||
		kernel.AvailableVersion != "5.14.0-427.42.1.el9_4" || !kernel.FromSecurityRepo {
		t.Fatalf("kernel 待装更新字段不全：%+v", kernel)
	}
	if pending.Items[2].Name != "NetworkManager" ||
		pending.Items[2].AvailableVersion != "1:1.46.0-13.el9_4" {
		t.Fatalf("版本纪元应原样保留：%+v", pending.Items[2])
	}
}

// TestParsePOSIXV3PendingUpdatesUnknownWithoutPackageManager 钉住未知语
// 义：没有 apt 与 dnf/yum 时待装更新为空指针；#353 时代的旧第 3 版输出
// 缺少这两节也不算不完整。
func TestParsePOSIXV3PendingUpdatesUnknownWithoutPackageManager(t *testing.T) {
	output := posixFixtureOutput(t, "vendor-linux-v3-no-package-manager.txt")
	collection, err := parsePOSIXInventoryOutput(output)
	if err != nil {
		t.Fatal(err)
	}
	if collection.PendingUpdates != nil || collection.Incomplete {
		t.Fatalf("没有包管理器时应为未知且完整：%+v", collection)
	}
	legacyOutput := strings.ReplaceAll(
		strings.ReplaceAll(output,
			"SSHMGR-INVENTORY SECTION pending-updates-apt\nSSHMGR-INVENTORY ABSENT pending-updates-apt\n", ""),
		"SSHMGR-INVENTORY SECTION pending-updates-dnf\nSSHMGR-INVENTORY ABSENT pending-updates-dnf\n", "")
	legacy, err := parsePOSIXInventoryOutput(legacyOutput)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.PendingUpdates != nil || legacy.Incomplete {
		t.Fatalf("旧第 3 版输出缺少待装更新节时应为未知且完整：%+v", legacy)
	}
}

// TestQueryPOSIXV3ContainerItemsThroughStore 全链路：第 3 版样例落库后查
// 询，容器条目默认出现在重点清单、字段齐全、缺版本标志正确、设备级的
// 发行版编号与未使用镜像数量可取，展示分组落在「容器」档。
func TestQueryPOSIXV3ContainerItemsThroughStore(t *testing.T) {
	manager := seedPOSIXSample(t, "容器工作站", "ubuntu2204-v3-containers.txt")

	items := querySampleDevice(t, manager, "容器工作站", QueryOptions{})
	gateway := findItemByNameAndCategory(t, items, CategoryContainer, "nginx")
	if gateway.ContainerName != "gateway" || !gateway.ContainerRunning ||
		gateway.ContainerDigest != "sha256:2f1b0af53c34b7d4c9d1d55a30fb8f0e6f8b2f9b0b9e1f1aaaaaaaaaaaaaaaa0" {
		t.Fatalf("查询结果里的 nginx 容器条目字段不全：%+v", gateway)
	}
	if len(gateway.ListeningPorts) != 1 || gateway.ListeningPorts[0].Port != 8080 {
		t.Fatalf("nginx 容器条目应带对外端口 8080：%+v", gateway.ListeningPorts)
	}
	if ItemDisplayCategory(gateway) != DisplayCategoryContainer {
		t.Fatalf("容器条目应映射到「容器」展示分组：%+v", gateway)
	}
	var trainer QueryItem
	trainerFound := false
	for _, item := range items {
		if item.Category == CategoryContainer && item.ContainerName == "trainer" {
			trainer = item
			trainerFound = true
			break
		}
	}
	if !trainerFound {
		t.Fatalf("查询结果里找不到容器名为 trainer 的容器条目：%+v", items)
	}
	if !trainer.MissingVersion {
		t.Fatalf("镜像没有自带版本标签的容器条目应带缺版本标志：%+v", trainer)
	}
	for _, item := range items {
		if item.Category == CategoryContainerImage {
			t.Fatalf("第 3 版结果的默认查询不应再出现容器镜像条目：%+v", item)
		}
	}

	result, err := manager.Query(QueryOptions{
		Device: "容器工作站", Visibility: AllDevicesVisible(),
	})
	if err != nil {
		t.Fatal(err)
	}
	device := findQueryDevice(result, "容器工作站")
	if device == nil {
		t.Fatal("样例设备不在查询结果里")
	}
	if device.Collection.DistroID != "ubuntu" || device.Collection.DistroVersionID != "22.04" {
		t.Fatalf("查询结果应带发行版编号与版本编号：%+v", device.Collection)
	}
	if device.Collection.UnusedImageCount != 2 {
		t.Fatalf("查询结果应带未使用镜像数量 2：%+v", device.Collection)
	}
}
