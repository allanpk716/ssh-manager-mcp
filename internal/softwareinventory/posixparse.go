package softwareinventory

import (
	"fmt"
	"strings"

	"ssh-manager-mcp/internal/models"
)

// posixV2Sections 是 POSIX 固定命令集按顺序输出的全部节名。解析器用它
// 发现整节缺失的情况；命令声明 ABSENT 的节不算缺失。
var posixV2Sections = []string{
	"os", "packages", "listen", "listen-netstat", "listen-lsof", "exe",
	"exe-owner", "apps", "brew", "synology-packages", "container-images",
	"containers", "firmware", "gpu-driver",
}

// parsePOSIXInventoryOutput 解析 posix-v2 固定命令集输出。输出没有开头
// 标记时返回错误；节缺失、节内命令失败或输出截断时保留已解析条目并标为
// 不完整。
func parsePOSIXInventoryOutput(output string) (*ParsedCollection, error) {
	sections, version, sawEnd, err := splitInventorySections(output)
	if err != nil {
		return nil, err
	}
	collection := &ParsedCollection{CommandSetVersion: version}
	osInfo := parsePOSIXOSSection(sectionOrEmpty(sections, "os").lines)
	for _, name := range requiredPOSIXSections(osInfo.systemType) {
		section, present := sections[name]
		if !present {
			collection.Incomplete = true
			collection.Notes = append(collection.Notes, fmt.Sprintf("缺少 %s 节", name))
			continue
		}
		if section.outcome == sectionIncomplete {
			collection.Incomplete = true
			collection.Notes = append(collection.Notes, fmt.Sprintf("%s 节未完成", name))
		}
	}
	if !sawEnd {
		collection.Incomplete = true
		collection.Notes = append(collection.Notes, "缺少结尾标记")
	}
	collection.SystemType = osInfo.systemType
	if collection.SystemType == "" {
		collection.Incomplete = true
		collection.Notes = append(collection.Notes, "系统识别结果为空")
	}
	if sectionOrEmpty(sections, "os").outcome != sectionAbsent && osInfo.systemType != "" {
		collection.Items = append(collection.Items, buildPOSIXOSItems(osInfo)...)
	}
	switch osInfo.systemType {
	case "ubuntu", "debian":
		collection.Items = append(collection.Items,
			buildLinuxPackageItems(sectionOrEmpty(sections, "packages"), osInfo.systemType)...)
		collection.Items = append(collection.Items, buildLinuxListeningItems(
			sectionOrEmpty(sections, "listen"), sectionOrEmpty(sections, "exe"),
			sectionOrEmpty(sections, "exe-owner"),
			packageItemIndex(collection.Items))...)
		collection.Items = append(collection.Items, buildLinuxContainerItems(
			sectionOrEmpty(sections, "container-images"),
			sectionOrEmpty(sections, "containers"))...)
		collection.Items = append(collection.Items,
			buildLinuxFirmwareItems(sectionOrEmpty(sections, "firmware"))...)
		collection.Items = append(collection.Items,
			buildLinuxGPUDriverItems(sectionOrEmpty(sections, "gpu-driver"))...)
	case "darwin":
		collection.Items = append(collection.Items,
			buildDarwinApplicationItems(sectionOrEmpty(sections, "apps"))...)
		collection.Items = append(collection.Items,
			buildDarwinBrewItems(sectionOrEmpty(sections, "brew"))...)
		collection.Items = append(collection.Items, buildLinuxContainerItems(
			sectionOrEmpty(sections, "container-images"),
			sectionOrEmpty(sections, "containers"))...)
		collection.Items = append(collection.Items, buildDarwinListeningItems(
			sectionOrEmpty(sections, "listen-lsof"), sectionOrEmpty(sections, "exe"),
			collection.Items)...)
	case "synology-dsm":
		collection.Items = append(collection.Items,
			buildSynologyPackageItems(sectionOrEmpty(sections, "synology-packages"))...)
		collection.Items = append(collection.Items, buildLinuxContainerItems(
			sectionOrEmpty(sections, "container-images"),
			sectionOrEmpty(sections, "containers"))...)
		collection.Items = append(collection.Items,
			buildLinuxFirmwareItems(sectionOrEmpty(sections, "firmware"))...)
		collection.Items = append(collection.Items, buildNetstatListeningItems(
			sectionOrEmpty(sections, "listen-netstat"), sectionOrEmpty(sections, "exe"),
			collection.Items)...)
	case "istoreos", "openwrt", "linux":
		collection.Items = append(collection.Items, buildNetstatListeningItems(
			sectionOrEmpty(sections, "listen-netstat"), sectionOrEmpty(sections, "exe"), nil)...)
	default:
		collection.Items = append(collection.Items,
			buildLinuxPackageItems(sectionOrEmpty(sections, "packages"), osInfo.systemType)...)
		collection.Items = append(collection.Items, buildLinuxListeningItems(
			sectionOrEmpty(sections, "listen"), sectionOrEmpty(sections, "exe"),
			sectionOrEmpty(sections, "exe-owner"),
			packageItemIndex(collection.Items))...)
		collection.Items = append(collection.Items, buildLinuxContainerItems(
			sectionOrEmpty(sections, "container-images"),
			sectionOrEmpty(sections, "containers"))...)
	}
	return collection, nil
}

// requiredPOSIXSections 返回一种系统实际采集时必须完整得到的节。基础采
// 集系统忽略其余节，因此那些节的失败不影响完整标记。
func requiredPOSIXSections(systemType string) []string {
	switch systemType {
	case "ubuntu", "debian":
		return []string{
			"os", "packages", "listen", "exe", "exe-owner", "container-images",
			"containers", "firmware", "gpu-driver",
		}
	case "darwin":
		return []string{
			"os", "listen-lsof", "exe", "apps", "brew", "container-images", "containers",
		}
	case "synology-dsm":
		return []string{
			"os", "listen-netstat", "exe", "synology-packages", "container-images",
			"containers", "firmware",
		}
	case "istoreos", "openwrt", "linux":
		return []string{"os", "listen-netstat"}
	default:
		return posixV2Sections
	}
}

type posixOSInfo struct {
	kernelName string
	kernel     string
	machine    string
	systemType string

	osRelease map[string]string
	openwrt   map[string]string
	synology  map[string]string
	darwin    map[string]string
}

// parsePOSIXOSSection 解析 os 节。固定命令集用 file 行分隔各个系统版本来
// 源；同一台机器可能有多个来源，系统识别按 README 中的优先级读取它们。
func parsePOSIXOSSection(lines []string) posixOSInfo {
	info := posixOSInfo{}
	current := map[string]string(nil)
	for _, line := range lines {
		if after, present := strings.CutPrefix(line, "file="); present {
			switch strings.TrimSpace(after) {
			case "/etc/os-release":
				info.osRelease = map[string]string{}
				current = info.osRelease
			case "/etc/openwrt_release":
				info.openwrt = map[string]string{}
				current = info.openwrt
			case "/etc.defaults/VERSION":
				info.synology = map[string]string{}
				current = info.synology
			case "sw_vers":
				info.darwin = map[string]string{}
				current = info.darwin
			}
			continue
		}
		key, value, present := strings.Cut(line, "=")
		if present {
			if current != nil {
				current[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"'`)
				continue
			}
			switch strings.TrimSpace(key) {
			case "kernel-name":
				info.kernelName = strings.TrimSpace(value)
			case "kernel":
				info.kernel = strings.TrimSpace(value)
			case "machine":
				info.machine = strings.TrimSpace(value)
			}
			continue
		}
		key, value, present = strings.Cut(line, ":")
		if present && current != nil {
			current[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	info.systemType = identifyPOSIXSystem(info)
	return info
}

// identifyPOSIXSystem 按固定命令集 README 的规则识别系统。
func identifyPOSIXSystem(info posixOSInfo) string {
	if strings.EqualFold(info.kernelName, "Darwin") || info.darwin != nil {
		return "darwin"
	}
	if info.synology != nil {
		return "synology-dsm"
	}
	for _, field := range []string{"ID", "ID_LIKE"} {
		for _, value := range strings.Fields(info.osRelease[field]) {
			switch strings.ToLower(value) {
			case "istoreos":
				return "istoreos"
			case "openwrt", "lede":
				return "openwrt"
			case "ubuntu":
				return "ubuntu"
			case "debian":
				return "debian"
			}
		}
	}
	if info.openwrt != nil {
		return "openwrt"
	}
	if info.kernelName != "" {
		return "linux"
	}
	return ""
}

// buildPOSIXOSItems 生成操作系统条目；全量采集系统另外保留内核版本，
// 基础采集系统只保留操作系统版本。
func buildPOSIXOSItems(info posixOSInfo) []models.SoftwareObservedItem {
	name, version := posixOSNameAndVersion(info)
	items := []models.SoftwareObservedItem{{
		Category:     models.SoftwareCategoryOperatingSystem,
		Name:         firstNonEmptyString(name, "Unknown"),
		Version:      firstNonEmptyString(version, "unknown"),
		Architecture: info.machine,
	}}
	if info.kernel != "" && info.systemType != "istoreos" && info.systemType != "openwrt" && info.systemType != "linux" {
		items = append(items, models.SoftwareObservedItem{
			Category:     models.SoftwareCategoryDriverFirmware,
			Name:         firstNonEmptyString(info.kernelName+" 内核", "Linux 内核"),
			Version:      info.kernel,
			Architecture: info.machine,
		})
	}
	return items
}

// posixOSNameAndVersion 从该系统自己的版本来源读出显示名与版本。
func posixOSNameAndVersion(info posixOSInfo) (string, string) {
	switch info.systemType {
	case "darwin":
		return info.darwin["ProductName"], firstNonEmptyString(
			info.darwin["ProductVersion"], info.darwin["BuildVersion"])
	case "synology-dsm":
		return "Synology DSM", firstNonEmptyString(
			info.synology["productversion"], strings.Join(nonEmptyStrings(
				info.synology["majorversion"], info.synology["minorversion"]), "."))
	case "istoreos", "openwrt":
		return firstNonEmptyString(info.osRelease["NAME"], info.openwrt["DISTRIB_ID"]),
			firstNonEmptyString(info.osRelease["VERSION_ID"], info.openwrt["DISTRIB_RELEASE"])
	default:
		return info.osRelease["NAME"], info.osRelease["VERSION_ID"]
	}
}

func nonEmptyStrings(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}
