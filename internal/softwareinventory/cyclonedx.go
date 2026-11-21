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
	if item.Category == CategorySystemPackage &&
		collection.DistroID != "" && collection.DistroVersionID != "" &&
		!strings.Contains(packageURL, "distro=") {
		separator := "?"
		if strings.Contains(packageURL, "?") {
			separator = "&"
		}
		packageURL += separator + "distro=" + collection.DistroID + "-" + collection.DistroVersionID
	}
	if (item.Category == CategoryContainer || item.Category == CategoryContainerImage) &&
		strings.HasPrefix(packageURL, "pkg:docker/") {
		packageURL = strings.Replace(packageURL, "pkg:docker/", "pkg:oci/", 1)
	}
	return packageURL
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
