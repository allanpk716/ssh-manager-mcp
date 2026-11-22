package softwareinventory

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// CycloneDXExportOptions 是生成 CycloneDX 文档时需要的文档头信息。
type CycloneDXExportOptions struct {
	GeneratedAt       time.Time
	SSHManagerVersion string
	ProfileName       string
}

type cycloneDXProperty struct {
	Name  string `json:"name"`
	Value string `json:"value,omitempty"`
}

type cycloneDXComponent struct {
	Type       string               `json:"type"`
	BOMRef     string               `json:"bom-ref,omitempty"`
	Name       string               `json:"name"`
	Version    string               `json:"version,omitempty"`
	PackageURL string               `json:"purl,omitempty"`
	CPE        string               `json:"cpe,omitempty"`
	Components []cycloneDXComponent `json:"components,omitempty"`
	Properties []cycloneDXProperty  `json:"properties,omitempty"`
	Hashes     []cycloneDXHash      `json:"hashes,omitempty"`
}

type cycloneDXHash struct {
	Alg     string `json:"alg"`
	Content string `json:"content"`
}

type cycloneDXTools struct {
	Components []cycloneDXComponent `json:"components"`
}

type cycloneDXMetadata struct {
	Timestamp  string              `json:"timestamp"`
	Tools      cycloneDXTools      `json:"tools"`
	Properties []cycloneDXProperty `json:"properties"`
	Component  *cycloneDXComponent `json:"component,omitempty"`
}

type cycloneDXDocument struct {
	Schema      string               `json:"$schema"`
	BOMFormat   string               `json:"bomFormat"`
	SpecVersion string               `json:"specVersion"`
	Version     int                  `json:"version"`
	Metadata    cycloneDXMetadata    `json:"metadata"`
	Components  []cycloneDXComponent `json:"components"`
}

// CycloneDXDeviceDocument 是一台设备独立的一份 CycloneDX 文档。
type CycloneDXDeviceDocument struct {
	DeviceName string
	Document   []byte
}

// MarshalCycloneDX 把统一查询结果序列化成 CycloneDX 1.6 JSON。
func MarshalCycloneDX(result QueryResult, options CycloneDXExportOptions) ([]byte, error) {
	if options.GeneratedAt.IsZero() {
		return nil, fmt.Errorf("CycloneDX 生成时间不能为空")
	}
	if options.SSHManagerVersion == "" || options.ProfileName == "" {
		return nil, fmt.Errorf("CycloneDX 文档头缺少 sshmgr 版本或 profile 名")
	}

	document := cycloneDXDocument{
		Schema:    "http://cyclonedx.org/schema/bom-1.6.schema.json",
		BOMFormat: "CycloneDX", SpecVersion: "1.6", Version: 1,
		Metadata: cycloneDXMetadata{
			Timestamp: options.GeneratedAt.UTC().Format(time.RFC3339),
			Tools: cycloneDXTools{Components: []cycloneDXComponent{{
				Type: "application", Name: "sshmgr", Version: options.SSHManagerVersion,
			}}},
			Properties: []cycloneDXProperty{{
				Name: "sshmgr:profile-name", Value: options.ProfileName,
			}},
		},
		Components: make([]cycloneDXComponent, 0, len(result.Devices)),
	}
	devices := append([]QueryDeviceResult(nil), result.Devices...)
	sort.SliceStable(devices, func(i, j int) bool {
		if devices[i].DeviceName != devices[j].DeviceName {
			return devices[i].DeviceName < devices[j].DeviceName
		}
		return devices[i].DeviceID < devices[j].DeviceID
	})
	for _, device := range devices {
		items := append([]QueryItem(nil), device.Items...)
		sort.SliceStable(items, func(i, j int) bool {
			leftRank, rightRank := cycloneDXCategoryRank(items[i].Category), cycloneDXCategoryRank(items[j].Category)
			if leftRank != rightRank {
				return leftRank < rightRank
			}
			if items[i].Name != items[j].Name {
				return items[i].Name < items[j].Name
			}
			if items[i].Version != items[j].Version {
				return items[i].Version < items[j].Version
			}
			return items[i].ID < items[j].ID
		})
		if len(devices) == 1 {
			for _, item := range items {
				if item.Category != CategoryOperatingSystem {
					continue
				}
				mainComponent := cycloneDXComponent{
					Type: "operating-system", Name: item.Name, Version: item.Version,
				}
				// Trivy 一类扫描工具要求操作系统组件名是小写发行版编号
				// （如 ubuntu）；采集到发行版编号时改用编号与版本编号。
				if device.Collection.DistroID != "" {
					mainComponent.Name = device.Collection.DistroID
					if device.Collection.DistroVersionID != "" {
						mainComponent.Version = device.Collection.DistroVersionID
					}
				}
				if device.Collection.DistroID != "" {
					mainComponent.Properties = append(mainComponent.Properties, cycloneDXProperty{
						Name: "sshmgr:distro-id", Value: device.Collection.DistroID,
					})
				}
				if device.Collection.DistroVersionID != "" {
					mainComponent.Properties = append(mainComponent.Properties, cycloneDXProperty{
						Name: "sshmgr:distro-version-id", Value: device.Collection.DistroVersionID,
					})
				}
				document.Metadata.Component = &mainComponent
				break
			}
		}
		deviceComponent := cycloneDXComponent{
			Type: "device", BOMRef: device.DeviceID, Name: device.DeviceName,
			Components: make([]cycloneDXComponent, 0, len(items)),
			Properties: []cycloneDXProperty{
				{Name: "sshmgr:collection-incomplete", Value: booleanText(device.Collection.Incomplete)},
				{Name: "sshmgr:inventory-stale", Value: booleanText(device.Collection.Stale)},
				{Name: "sshmgr:never-collected", Value: booleanText(device.Collection.NeverCollected)},
			},
		}
		for _, item := range items {
			componentType, err := cycloneDXComponentType(item.Category)
			if err != nil {
				return nil, err
			}
			component := cycloneDXComponent{
				Type: componentType, BOMRef: item.ID, Name: item.Name, Version: item.Version,
				PackageURL: item.PackageURL, CPE: item.CPE,
				Properties: []cycloneDXProperty{
					{Name: "sshmgr:source", Value: item.Source},
				},
			}
			component.PackageURL = cycloneDXPackageURL(item, device.Collection)
			if (item.Category == CategoryContainer || item.Category == CategoryContainerImage) &&
				strings.HasPrefix(item.ContainerDigest, "sha256:") {
				component.Hashes = append(component.Hashes, cycloneDXHash{
					Alg:     "SHA-256",
					Content: strings.TrimPrefix(item.ContainerDigest, "sha256:"),
				})
			}
			if item.Category == CategoryContainer {
				component.Properties = append(component.Properties, cycloneDXProperty{
					Name: "sshmgr:container-running", Value: booleanText(item.ContainerRunning),
				})
			}
			if (item.Category == CategoryContainer || item.Category == CategoryContainerImage) &&
				item.ImagePullability != "" {
				component.Properties = append(component.Properties, cycloneDXProperty{
					Name: "sshmgr:image-pullability", Value: string(item.ImagePullability),
				})
			}
			if item.Category == CategoryUnidentified {
				component.Properties = append(component.Properties, cycloneDXProperty{
					Name: "sshmgr:unidentified-process", Value: "true",
				})
			}
			if len(item.ListeningPorts) > 0 {
				ports, exposures, err := cycloneDXListeningPortText(item.ListeningPorts)
				if err != nil {
					return nil, err
				}
				component.Properties = append(component.Properties,
					cycloneDXProperty{Name: "sshmgr:exposure-level", Value: exposures},
					cycloneDXProperty{Name: "sshmgr:listening-ports", Value: ports},
				)
			}
			if item.CollectedAt != nil {
				component.Properties = append(component.Properties, cycloneDXProperty{
					Name: "sshmgr:collected-at", Value: item.CollectedAt.UTC().Format(time.RFC3339),
				})
			}
			deviceComponent.Components = append(deviceComponent.Components, component)
		}
		document.Components = append(document.Components, deviceComponent)
	}
	documentBytes, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(documentBytes, '\n'), nil
}

// MarshalCycloneDXPerDevice 为每台设备生成一份独立的 CycloneDX 文档，
// 顺序按设备名与设备编号稳定排序。
func MarshalCycloneDXPerDevice(
	result QueryResult, options CycloneDXExportOptions,
) ([]CycloneDXDeviceDocument, error) {
	devices := append([]QueryDeviceResult(nil), result.Devices...)
	sort.SliceStable(devices, func(i, j int) bool {
		if devices[i].DeviceName != devices[j].DeviceName {
			return devices[i].DeviceName < devices[j].DeviceName
		}
		return devices[i].DeviceID < devices[j].DeviceID
	})
	documents := make([]CycloneDXDeviceDocument, 0, len(devices))
	for _, device := range devices {
		document, err := MarshalCycloneDX(QueryResult{
			ProfileName: result.ProfileName, Devices: []QueryDeviceResult{device},
		}, options)
		if err != nil {
			return nil, err
		}
		documents = append(documents, CycloneDXDeviceDocument{
			DeviceName: device.DeviceName, Document: document,
		})
	}
	return documents, nil
}

func cycloneDXPackageURL(item QueryItem, collection CollectionStatus) string {
	packageURL := item.PackageURL
	if packageURL == "" {
		return packageURL
	}
	if (item.Category == CategoryContainer || item.Category == CategoryContainerImage) &&
		strings.HasPrefix(packageURL, "pkg:docker/") {
		packageURL = ociPackageURL(packageURL)
	}
	if item.Category == CategorySystemPackage &&
		collection.DistroID != "" && collection.DistroVersionID != "" &&
		!strings.Contains(packageURL, "distro=") {
		packageURL = withPurlQualifier(
			packageURL, "distro", collection.DistroID+"-"+collection.DistroVersionID)
	}
	return packageURL
}

// ociPackageURL 把 pkg:docker 形式的容器软件包统一地址改写成符合 purl
// 规范的 oci 类型：oci 不允许命名空间，名称段只留镜像名的最后一段，完
// 整镜像仓库地址挪到 repository_url 限定符，摘要中的冒号按规范转义。存
// 储格式没有仓库地址与摘要时原样返回，不编造信息。
func ociPackageURL(packageURL string) string {
	path, query, _ := strings.Cut(strings.TrimPrefix(packageURL, "pkg:docker/"), "?")
	repository, digest, hasDigest := strings.Cut(path, "@")
	if !hasDigest || repository == "" || digest == "" {
		return packageURL
	}
	name := repository
	qualifiers := parsePurlQualifiers(query)
	if lastSlash := strings.LastIndex(repository, "/"); lastSlash >= 0 {
		name = repository[lastSlash+1:]
		qualifiers = append(qualifiers, purlQualifier{key: "repository_url", value: repository})
	}
	return "pkg:oci/" + name + "@" + percentEncodePURL(digest) + buildPurlQualifiers(qualifiers)
}

// purlQualifier 是软件包统一地址问号后的一个键值对。
type purlQualifier struct {
	key, value string
}

// parsePurlQualifiers 解析软件包统一地址问号后的限定符串。这里读的地址
// 由采集侧生成，键值都是未转义的明文。
func parsePurlQualifiers(query string) []purlQualifier {
	qualifiers := []purlQualifier{}
	for _, part := range strings.Split(query, "&") {
		if part == "" {
			continue
		}
		key, value, _ := strings.Cut(part, "=")
		qualifiers = append(qualifiers, purlQualifier{key: key, value: value})
	}
	return qualifiers
}

// withPurlQualifier 在软件包统一地址上追加一个限定符，按键排序并转义后
// 重建限定符串。
func withPurlQualifier(packageURL, key, value string) string {
	base, query, _ := strings.Cut(packageURL, "?")
	qualifiers := append(
		parsePurlQualifiers(query), purlQualifier{key: key, value: value})
	return base + buildPurlQualifiers(qualifiers)
}

// buildPurlQualifiers 把限定符按键排序、按键值转义后拼成带问号前缀的限
// 定符串；没有限定符时返回空串。
func buildPurlQualifiers(qualifiers []purlQualifier) string {
	if len(qualifiers) == 0 {
		return ""
	}
	sort.SliceStable(qualifiers, func(i, j int) bool {
		return qualifiers[i].key < qualifiers[j].key
	})
	parts := make([]string, 0, len(qualifiers))
	for _, qualifier := range qualifiers {
		parts = append(parts,
			percentEncodePURL(qualifier.key)+"="+percentEncodePURL(qualifier.value))
	}
	return "?" + strings.Join(parts, "&")
}

// percentEncodePURL 转义软件包统一地址里需要编码的段：只保留字母数字与
// 连字符、点、下划线、波浪线，其余字节写成大写十六进制百分号转义（镜像
// 摘要的冒号写成 %3A，仓库地址的斜杠写成 %2F）。
func percentEncodePURL(value string) string {
	needsEncoding := false
	for i := 0; i < len(value); i++ {
		if !isPURLUnreserved(value[i]) {
			needsEncoding = true
			break
		}
	}
	if !needsEncoding {
		return value
	}
	var builder strings.Builder
	for i := 0; i < len(value); i++ {
		if isPURLUnreserved(value[i]) {
			builder.WriteByte(value[i])
			continue
		}
		fmt.Fprintf(&builder, "%%%02X", value[i])
	}
	return builder.String()
}

func isPURLUnreserved(char byte) bool {
	switch {
	case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z',
		char >= '0' && char <= '9':
		return true
	case char == '-', char == '.', char == '_', char == '~':
		return true
	default:
		return false
	}
}

func cycloneDXCategoryRank(category Category) int {
	switch category {
	case CategoryOperatingSystem:
		return 1
	case CategoryServiceProgram:
		return 2
	case CategoryContainer:
		return 3
	case CategoryContainerImage:
		return 4
	case CategoryDriverFirmware:
		return 5
	case CategorySystemPackage:
		return 6
	case CategoryUnidentified:
		return 7
	default:
		return 8
	}
}

func cycloneDXComponentType(category Category) (string, error) {
	switch category {
	case CategoryOperatingSystem:
		return "operating-system", nil
	case CategoryServiceProgram, CategoryUnidentified:
		return "application", nil
	case CategoryContainer:
		return "container", nil
	case CategoryContainerImage:
		return "container", nil
	case CategoryDriverFirmware:
		return "firmware", nil
	case CategorySystemPackage:
		return "library", nil
	default:
		return "", fmt.Errorf("软件类别不能导出为 CycloneDX 类别：%q", string(category))
	}
}

func booleanText(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func cycloneDXListeningPortText(ports []ListeningPort) (string, string, error) {
	sorted := append([]ListeningPort(nil), ports...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Port != sorted[j].Port {
			return sorted[i].Port < sorted[j].Port
		}
		if sorted[i].Protocol != sorted[j].Protocol {
			return sorted[i].Protocol < sorted[j].Protocol
		}
		return sorted[i].Address < sorted[j].Address
	})

	portText := make([]string, 0, len(sorted))
	exposureSeen := map[Exposure]bool{}
	exposures := make([]Exposure, 0)
	for _, port := range sorted {
		if exposureRank(port.Exposure) == 0 {
			return "", "", fmt.Errorf("监听端口的暴露级别不合法：%q", string(port.Exposure))
		}
		portText = append(portText, fmt.Sprintf(
			"%s:%d/%s=%s", port.Address, port.Port, port.Protocol, string(port.Exposure),
		))
		if !exposureSeen[port.Exposure] {
			exposureSeen[port.Exposure] = true
			exposures = append(exposures, port.Exposure)
		}
	}
	sort.SliceStable(exposures, func(i, j int) bool {
		return exposureRank(exposures[i]) < exposureRank(exposures[j])
	})
	exposureValues := make([]string, 0, len(exposures))
	for _, exposure := range exposures {
		exposureValues = append(exposureValues, string(exposure))
	}
	return strings.Join(portText, ";"), strings.Join(exposureValues, ";"), nil
}
