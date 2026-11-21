package softwareinventory

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	jsonschema "github.com/google/jsonschema-go/jsonschema"
)

// decodeCycloneDX 把导出的 JSON 解成通用对象，测试只看对外文档结构。
func decodeCycloneDX(t *testing.T, document []byte) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(document, &value); err != nil {
		t.Fatalf("CycloneDX 输出不是合法 JSON：%v\n%s", err, document)
	}
	return value
}

func nestedMap(t *testing.T, value any, context string) map[string]any {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s 不是对象：%T", context, value)
	}
	return object
}

func anySlice(t *testing.T, value any, context string) []any {
	t.Helper()
	items, ok := value.([]any)
	if !ok {
		t.Fatalf("%s 不是数组：%T", context, value)
	}
	return items
}

// TestCycloneDXContainerCategoryGetsComponentType 钉住容器条目的最小映
// 射，避免默认重点清单导出时因新类别失败。
func TestCycloneDXContainerCategoryGetsComponentType(t *testing.T) {
	componentType, err := cycloneDXComponentType(CategoryContainer)
	if err != nil {
		t.Fatal(err)
	}
	if componentType != "container" {
		t.Fatalf("容器条目的 CycloneDX 组件类型 = %q", componentType)
	}
}

func TestCycloneDXExportDocumentStructure(t *testing.T) {
	generatedAt := time.Date(2026, 11, 9, 12, 30, 5, 0, time.UTC)
	document, err := MarshalCycloneDX(QueryResult{Devices: []QueryDeviceResult{{
		DeviceID:   "device-1",
		DeviceName: "gpu工作站",
		Collection: CollectionStatus{Incomplete: true, Stale: true},
		Items: []QueryItem{{
			ID: "item-1", DeviceID: "device-1", Category: CategoryServiceProgram,
			CategoryLabel: "服务程序", Name: "nginx", Version: "1.25.3",
			PackageURL: "pkg:generic/nginx@1.25.3",
			CPE:        "cpe:2.3:a:f5:nginx:1.25.3:*:*:*:*:*:*:*",
			ListeningPorts: []ListeningPort{{
				Address: "0.0.0.0", Port: 443, Protocol: "tcp", Exposure: ExposurePublicNetwork,
			}},
			Source:      "实测",
			CollectedAt: &generatedAt,
		}},
	}}}, CycloneDXExportOptions{
		GeneratedAt: generatedAt, SSHManagerVersion: "9.9.9-test", ProfileName: "inventory-profile",
	})
	if err != nil {
		t.Fatal(err)
	}

	value := decodeCycloneDX(t, document)
	if value["$schema"] != "http://cyclonedx.org/schema/bom-1.6.schema.json" ||
		value["bomFormat"] != "CycloneDX" || value["specVersion"] != "1.6" || value["version"] != float64(1) {
		t.Fatalf("文档头 = %+v", value)
	}
	metadata := nestedMap(t, value["metadata"], "metadata")
	if metadata["timestamp"] != "2026-11-09T12:30:05Z" {
		t.Fatalf("生成时间 = %v", metadata["timestamp"])
	}
	tools := nestedMap(t, metadata["tools"], "metadata.tools")
	toolComponents := anySlice(t, tools["components"], "metadata.tools.components")
	tool := nestedMap(t, toolComponents[0], "生成工具")
	if tool["type"] != "application" || tool["name"] != "sshmgr" || tool["version"] != "9.9.9-test" {
		t.Fatalf("生成工具 = %+v", tool)
	}

	components := anySlice(t, value["components"], "顶层组件")
	device := nestedMap(t, components[0], "设备组件")
	if device["type"] != "device" || device["bom-ref"] != "device-1" || device["name"] != "gpu工作站" {
		t.Fatalf("设备组件 = %+v", device)
	}
	software := anySlice(t, device["components"], "设备内软件组件")
	item := nestedMap(t, software[0], "软件组件")
	if item["type"] != "application" || item["bom-ref"] != "item-1" ||
		item["name"] != "nginx" || item["version"] != "1.25.3" ||
		item["purl"] != "pkg:generic/nginx@1.25.3" ||
		item["cpe"] != "cpe:2.3:a:f5:nginx:1.25.3:*:*:*:*:*:*:*" {
		t.Fatalf("软件组件 = %+v", item)
	}
}

func propertiesByName(t *testing.T, value map[string]any) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, raw := range anySlice(t, value["properties"], "properties") {
		property := nestedMap(t, raw, "property")
		name, _ := property["name"].(string)
		propertyValue, _ := property["value"].(string)
		out[name] = propertyValue
	}
	return out
}

func TestCycloneDXExportCategoriesAndSSHManagerProperties(t *testing.T) {
	generatedAt := time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC)
	cases := []struct {
		category Category
		wantType string
	}{
		{CategoryOperatingSystem, "operating-system"},
		{CategoryServiceProgram, "application"},
		{CategoryContainerImage, "container"},
		{CategoryDriverFirmware, "firmware"},
		{CategorySystemPackage, "library"},
		{CategoryUnidentified, "application"},
	}
	items := make([]QueryItem, 0, len(cases))
	for i, testCase := range cases {
		item := QueryItem{
			ID: fmt.Sprintf("item-%d", i+1), DeviceID: "device-1",
			Category: testCase.category, CategoryLabel: testCase.category.Label(),
			Name: fmt.Sprintf("软件-%02d", i), Version: "1.0", Source: "实测",
		}
		if testCase.category == CategoryUnidentified {
			item.Name = "未识别进程-443"
			item.ListeningPorts = []ListeningPort{{
				Address: "0.0.0.0", Port: 443, Protocol: "tcp", Exposure: ExposureAllAddresses,
			}}
		}
		if i == 0 {
			item.CollectedAt = &generatedAt
			item.ListeningPorts = []ListeningPort{{
				Address: "192.0.2.10", Port: 8443, Protocol: "tcp", Exposure: ExposureLocalAreaNetwork,
			}, {
				Address: "0.0.0.0", Port: 443, Protocol: "tcp", Exposure: ExposureAllAddresses,
			}}
		}
		items = append(items, item)
	}

	document, err := MarshalCycloneDX(QueryResult{Devices: []QueryDeviceResult{{
		DeviceID: "device-1", DeviceName: "gpu工作站",
		Collection: CollectionStatus{Incomplete: true, Stale: true}, Items: items,
	}}}, CycloneDXExportOptions{
		GeneratedAt: generatedAt, SSHManagerVersion: "9.9.9-test", ProfileName: "inventory-profile",
	})
	if err != nil {
		t.Fatal(err)
	}
	value := decodeCycloneDX(t, document)
	if got := propertiesByName(t, nestedMap(t, value["metadata"], "metadata"))["sshmgr:profile-name"]; got != "inventory-profile" {
		t.Fatalf("profile 名属性 = %q", got)
	}
	device := nestedMap(t, anySlice(t, value["components"], "顶层组件")[0], "设备组件")
	deviceProperties := propertiesByName(t, device)
	if deviceProperties["sshmgr:collection-incomplete"] != "true" ||
		deviceProperties["sshmgr:inventory-stale"] != "true" {
		t.Fatalf("设备采集状态属性 = %+v", deviceProperties)
	}

	software := anySlice(t, device["components"], "设备内软件组件")
	for i, testCase := range cases {
		component := nestedMap(t, software[i], "软件组件")
		if component["type"] != testCase.wantType {
			t.Fatalf("类别 %s 导出为 %q，应为 %q", testCase.category, component["type"], testCase.wantType)
		}
	}

	first := propertiesByName(t, nestedMap(t, software[0], "第一个软件组件"))
	wantPorts := "0.0.0.0:443/tcp=all_addresses;192.0.2.10:8443/tcp=local_area_network"
	if first["sshmgr:listening-ports"] != wantPorts {
		t.Fatalf("监听端口属性 = %q，应为 %q", first["sshmgr:listening-ports"], wantPorts)
	}
	if first["sshmgr:exposure-level"] != "all_addresses;local_area_network" {
		t.Fatalf("暴露级别属性 = %q", first["sshmgr:exposure-level"])
	}
	if first["sshmgr:source"] != "实测" || first["sshmgr:collected-at"] != "2026-11-09T08:00:00Z" {
		t.Fatalf("来源或采集时间属性 = %+v", first)
	}
	unidentified := propertiesByName(t, nestedMap(t, software[len(software)-1], "未识别进程组件"))
	if unidentified["sshmgr:unidentified-process"] != "true" {
		t.Fatalf("未识别进程标记属性 = %+v", unidentified)
	}
}

func TestCycloneDXExportIsStable(t *testing.T) {
	generatedAt := time.Date(2026, 11, 9, 9, 15, 0, 0, time.UTC)
	result := QueryResult{Devices: []QueryDeviceResult{
		{
			DeviceID: "device-2", DeviceName: "工作站", Items: []QueryItem{
				{ID: "z", Category: CategorySystemPackage, Name: "zlib", Version: "1.3"},
				{ID: "a", Category: CategoryOperatingSystem, Name: "macOS", Version: "15.1"},
			},
		},
		{
			DeviceID: "device-1", DeviceName: "gpu工作站", Items: []QueryItem{
				{ID: "n2", Category: CategoryServiceProgram, Name: "nginx", Version: "1.27.0"},
				{ID: "n1", Category: CategoryServiceProgram, Name: "nginx", Version: "1.25.3"},
			},
		},
	}}
	options := CycloneDXExportOptions{
		GeneratedAt: generatedAt, SSHManagerVersion: "9.9.9-test", ProfileName: "inventory-profile",
	}
	first, err := MarshalCycloneDX(result, options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := MarshalCycloneDX(result, options)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("同一份数据两次导出不一致：\n第一次：\n%s\n第二次：\n%s", first, second)
	}

	value := decodeCycloneDX(t, first)
	devices := anySlice(t, value["components"], "顶层组件")
	if nestedMap(t, devices[0], "第一台设备")["name"] != "gpu工作站" ||
		nestedMap(t, devices[1], "第二台设备")["name"] != "工作站" {
		t.Fatalf("设备顺序不稳定：%+v", value["components"])
	}
	gpuItems := anySlice(t, nestedMap(t, devices[0], "gpu工作站")["components"], "gpu工作站软件")
	if nestedMap(t, gpuItems[0], "gpu第一条")["version"] != "1.25.3" ||
		nestedMap(t, gpuItems[1], "gpu第二条")["version"] != "1.27.0" {
		t.Fatalf("同名字软件没有按版本稳定排序：%+v", gpuItems)
	}
	workstationItems := anySlice(t, nestedMap(t, devices[1], "工作站")["components"], "工作站软件")
	if nestedMap(t, workstationItems[0], "工作站第一条")["name"] != "macOS" ||
		nestedMap(t, workstationItems[1], "工作站第一条")["name"] != "zlib" {
		t.Fatalf("软件类别顺序不稳定：%+v", workstationItems)
	}
}

// TestCycloneDXPerDeviceFromPOSIXV3Sample 钉住第 3 版 POSIX 样例到每台
// 设备一份 CycloneDX 文档的完整导出面：操作系统做主组件，系统包软件包
// 统一地址带发行版限定，容器组件换用 oci 类型并带镜像摘要。
func TestCycloneDXPerDeviceFromPOSIXV3Sample(t *testing.T) {
	manager := seedPOSIXSample(t, "容器工作站", "ubuntu2204-v3-containers.txt")
	result, err := manager.Query(QueryOptions{
		Device: "容器工作站", IncludeSystemPackages: true, Visibility: AllDevicesVisible(),
	})
	if err != nil {
		t.Fatal(err)
	}
	documents, err := MarshalCycloneDXPerDevice(result, CycloneDXExportOptions{
		GeneratedAt:       time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC),
		SSHManagerVersion: "9.9.9-test", ProfileName: "inventory-profile",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 1 || documents[0].DeviceName != "容器工作站" {
		t.Fatalf("每台设备导出应为一份文档：%+v", documents)
	}

	value := decodeCycloneDX(t, documents[0].Document)
	metadata := nestedMap(t, value["metadata"], "metadata")
	mainComponent := nestedMap(t, metadata["component"], "metadata.component")
	if mainComponent["type"] != "operating-system" || mainComponent["name"] != "Ubuntu" ||
		mainComponent["version"] != "22.04" {
		t.Fatalf("操作系统主组件 = %+v", mainComponent)
	}
	mainProperties := propertiesByName(t, mainComponent)
	if mainProperties["sshmgr:distro-id"] != "ubuntu" ||
		mainProperties["sshmgr:distro-version-id"] != "22.04" {
		t.Fatalf("操作系统主组件的发行版属性 = %+v", mainProperties)
	}

	device := nestedMap(t, anySlice(t, value["components"], "顶层组件")[0], "设备组件")
	components := anySlice(t, device["components"], "设备内软件组件")
	var libc6, nginx map[string]any
	for _, raw := range components {
		component := nestedMap(t, raw, "软件组件")
		switch {
		case component["name"] == "libc6":
			libc6 = component
		case component["purl"] == "pkg:oci/nginx@sha256:2f1b0af53c34b7d4c9d1d55a30fb8f0e6f8b2f9b0b9e1f1aaaaaaaaaaaaaaaa0?tag=1.25.3":
			nginx = component
		}
	}
	if libc6 == nil {
		t.Fatalf("文档缺少 libc6 组件：%+v", components)
	}
	if got := libc6["purl"]; got != "pkg:deb/ubuntu/libc6@2.35-0ubuntu3.8?arch=amd64&distro=ubuntu-22.04" {
		t.Fatalf("libc6 的软件包统一地址 = %v", got)
	}
	if nginx == nil {
		t.Fatalf("文档缺少 nginx 容器组件：%+v", components)
	}
	hashes := anySlice(t, nginx["hashes"], "nginx 镜像摘要")
	firstHash := nestedMap(t, hashes[0], "nginx 镜像摘要")
	if firstHash["alg"] != "SHA-256" ||
		firstHash["content"] != "2f1b0af53c34b7d4c9d1d55a30fb8f0e6f8b2f9b0b9e1f1aaaaaaaaaaaaaaaa0" {
		t.Fatalf("nginx 镜像摘要 = %+v", firstHash)
	}
	nginxProperties := propertiesByName(t, nginx)
	if nginxProperties["sshmgr:container-running"] != "true" {
		t.Fatalf("nginx 容器运行状态属性 = %+v", nginxProperties)
	}

	var instance any
	if err := json.Unmarshal(documents[0].Document, &instance); err != nil {
		t.Fatal(err)
	}
	if err := loadCycloneDXSchemaForTest(t).Validate(instance); err != nil {
		t.Fatalf("CycloneDX 输出不符合 1.6 Schema：%v\n%s", err, documents[0].Document)
	}
}

// TestCycloneDXPerDeviceFromWindowsV2Sample 钉住 Windows 样例的主组件：
// 操作系统带完整四段版本号；Windows 采集没有发行版编号，因此不带发行版
// 属性。
func TestCycloneDXPerDeviceFromWindowsV2Sample(t *testing.T) {
	manager := seedWindowsSample(t, "win工作站", "win10-powershell-full-v2-derived.txt")
	result, err := manager.Query(QueryOptions{
		Device: "win工作站", IncludeSystemPackages: true, Visibility: AllDevicesVisible(),
	})
	if err != nil {
		t.Fatal(err)
	}
	documents, err := MarshalCycloneDXPerDevice(result, CycloneDXExportOptions{
		GeneratedAt:       time.Date(2026, 11, 21, 8, 5, 0, 0, time.UTC),
		SSHManagerVersion: "9.9.9-test", ProfileName: "inventory-profile",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 1 || documents[0].DeviceName != "win工作站" {
		t.Fatalf("每台设备导出应为一份文档：%+v", documents)
	}

	metadata := nestedMap(t, decodeCycloneDX(t, documents[0].Document)["metadata"], "metadata")
	mainComponent := nestedMap(t, metadata["component"], "metadata.component")
	if mainComponent["type"] != "operating-system" ||
		mainComponent["name"] != "Windows 10 Pro" ||
		mainComponent["version"] != "10.0.19045.6456" {
		t.Fatalf("Windows 操作系统主组件 = %+v", mainComponent)
	}
	if rawProperties, exists := mainComponent["properties"]; exists {
		properties := propertiesByName(t, mainComponent)
		if properties["sshmgr:distro-id"] != "" {
			t.Fatalf("Windows 主组件不应带发行版编号属性：%+v", rawProperties)
		}
	}
}

// TestCycloneDXPerDeviceReturnsOneDocumentPerDevice 钉住多设备导出的拆分
// 规则：按设备名稳定排序，一台设备一份文档，主组件与条目都不串到另一台
// 设备。
func TestCycloneDXPerDeviceReturnsOneDocumentPerDevice(t *testing.T) {
	result := QueryResult{Devices: []QueryDeviceResult{
		{
			DeviceID: "device-2", DeviceName: "z工作站",
			Collection: CollectionStatus{DistroID: "ubuntu", DistroVersionID: "24.04"},
			Items: []QueryItem{
				{ID: "z系统", DeviceID: "device-2", Category: CategoryOperatingSystem, Name: "Ubuntu", Version: "24.04"},
				{ID: "z软件", DeviceID: "device-2", Category: CategoryServiceProgram, Name: "z服务", Version: "1.0"},
			},
		},
		{
			DeviceID: "device-1", DeviceName: "a工作站",
			Items: []QueryItem{
				{ID: "a系统", DeviceID: "device-1", Category: CategoryOperatingSystem, Name: "Windows 11 Pro", Version: "10.0.26100.1"},
				{ID: "a软件", DeviceID: "device-1", Category: CategoryServiceProgram, Name: "a服务", Version: "2.0"},
			},
		},
	}}
	documents, err := MarshalCycloneDXPerDevice(result, CycloneDXExportOptions{
		GeneratedAt:       time.Date(2026, 11, 21, 8, 10, 0, 0, time.UTC),
		SSHManagerVersion: "9.9.9-test", ProfileName: "inventory-profile",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 2 || documents[0].DeviceName != "a工作站" ||
		documents[1].DeviceName != "z工作站" {
		t.Fatalf("多设备文档拆分或排序不正确：%+v", documents)
	}

	wanted := []struct {
		deviceName  string
		mainName    string
		mainVersion string
		itemName    string
	}{{
		deviceName: "a工作站", mainName: "Windows 11 Pro", mainVersion: "10.0.26100.1", itemName: "a服务",
	}, {
		deviceName: "z工作站", mainName: "Ubuntu", mainVersion: "24.04", itemName: "z服务",
	}}
	for i, want := range wanted {
		value := decodeCycloneDX(t, documents[i].Document)
		mainComponent := nestedMap(
			t, nestedMap(t, value["metadata"], "metadata")["component"], "metadata.component",
		)
		if mainComponent["name"] != want.mainName || mainComponent["version"] != want.mainVersion {
			t.Fatalf("设备 %s 的主组件 = %+v", want.deviceName, mainComponent)
		}
		topComponents := anySlice(t, value["components"], "顶层组件")
		if len(topComponents) != 1 {
			t.Fatalf("设备 %s 的文档应只包含本设备：%+v", want.deviceName, topComponents)
		}
		deviceComponent := nestedMap(t, topComponents[0], "设备组件")
		if deviceComponent["name"] != want.deviceName {
			t.Fatalf("文档 %d 的设备名 = %v，应为 %s", i, deviceComponent["name"], want.deviceName)
		}
		items := anySlice(t, deviceComponent["components"], "设备内软件组件")
		found := false
		for _, raw := range items {
			if nestedMap(t, raw, "软件组件")["name"] == want.itemName {
				found = true
			}
		}
		if !found {
			t.Fatalf("设备 %s 的文档缺少本设备条目 %s：%+v", want.deviceName, want.itemName, items)
		}
	}
}

func loadCycloneDXSchemaForTest(t *testing.T) *jsonschema.Resolved {
	t.Helper()
	load := func(uri *url.URL) (*jsonschema.Schema, error) {
		blob, err := os.ReadFile(filepath.Join(
			"testdata", "cyclonedx", path.Base(uri.Path),
		))
		if err != nil {
			return nil, err
		}
		var schema jsonschema.Schema
		if err := json.Unmarshal(blob, &schema); err != nil {
			return nil, fmt.Errorf("读取 %s 失败：%w", uri, err)
		}
		return &schema, nil
	}
	blob, err := os.ReadFile(filepath.Join("testdata", "cyclonedx", "bom-1.6.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	blob, err = normalizeDraftSevenSchemaForTest(t, blob)
	if err != nil {
		t.Fatal(err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(blob, &schema); err != nil {
		t.Fatalf("读取主 Schema 失败：%v", err)
	}
	// 这份校验库只接受 2020-12 的版本标记；CycloneDX 1.6 的这份
	// draft-07 Schema 在这里使用的校验关键词与 2020-12 兼容，去掉版本
	// 标记后仍按同一份结构校验实例。
	schema.Schema = ""
	resolved, err := schema.Resolve(&jsonschema.ResolveOptions{Loader: load})
	if err != nil {
		t.Fatalf("解析 CycloneDX 1.6 Schema 失败：%v", err)
	}
	return resolved
}

func normalizeDraftSevenSchemaForTest(t *testing.T, blob []byte) ([]byte, error) {
	t.Helper()
	var value any
	if err := json.Unmarshal(blob, &value); err != nil {
		return nil, err
	}
	var normalize func(any) any
	normalize = func(current any) any {
		object, ok := current.(map[string]any)
		if !ok {
			if items, ok := current.([]any); ok {
				for index, child := range items {
					items[index] = normalize(child)
				}
			}
			return current
		}
		if items, ok := object["items"].([]any); ok {
			object["prefixItems"] = items
			delete(object, "items")
		}
		for key, child := range object {
			object[key] = normalize(child)
		}
		return object
	}
	return json.Marshal(normalize(value))
}

func TestCycloneDXExportValidatesAgainstSchema(t *testing.T) {
	generatedAt := time.Date(2026, 11, 9, 7, 5, 0, 0, time.UTC)
	document, err := MarshalCycloneDX(QueryResult{Devices: []QueryDeviceResult{{
		DeviceID: "device-1", DeviceName: "gpu工作站",
		Collection: CollectionStatus{Incomplete: true, Stale: true},
		Items: []QueryItem{{
			ID: "item-1", DeviceID: "device-1", Category: CategoryServiceProgram,
			CategoryLabel: "服务程序", Name: "nginx", Version: "1.25.3",
			PackageURL: "pkg:generic/nginx@1.25.3",
			CPE:        "cpe:2.3:a:f5:nginx:1.25.3:*:*:*:*:*:*:*",
			ListeningPorts: []ListeningPort{{
				Address: "0.0.0.0", Port: 443, Protocol: "tcp", Exposure: ExposureAllAddresses,
			}},
			Source: "实测", CollectedAt: &generatedAt,
		}},
	}}}, CycloneDXExportOptions{
		GeneratedAt: generatedAt, SSHManagerVersion: "9.9.9-test", ProfileName: "inventory-profile",
	})
	if err != nil {
		t.Fatal(err)
	}
	var instance any
	if err := json.Unmarshal(document, &instance); err != nil {
		t.Fatal(err)
	}
	if err := loadCycloneDXSchemaForTest(t).Validate(instance); err != nil {
		t.Fatalf("CycloneDX 输出不符合 1.6 Schema：%v\n%s", err, document)
	}
	delete(instance.(map[string]any), "bomFormat")
	if err := loadCycloneDXSchemaForTest(t).Validate(instance); err == nil {
		t.Fatal("删掉 bomFormat 后仍通过 Schema 校验，说明校验没有真正执行")
	}
}
