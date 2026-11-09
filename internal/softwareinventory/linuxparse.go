package softwareinventory

import (
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"ssh-manager-mcp/internal/models"
)

// ParsedCollection 是一份固定命令集输出的解析结果。某一节缺失、非零退
// 出码或输出被截断只影响该节：已解析出的条目保留，整体记为不完整。
type ParsedCollection struct {
	CommandSetVersion string
	SystemType        string
	Incomplete        bool
	Notes             []string
	Items             []models.SoftwareObservedItem
}

// sectionOutcome 描述一节输出在协议层的状态。
type sectionOutcome string

const (
	sectionComplete   sectionOutcome = "complete"
	sectionAbsent     sectionOutcome = "absent"
	sectionIncomplete sectionOutcome = "incomplete"
)

type parsedSection struct {
	name    string
	outcome sectionOutcome
	lines   []string
}

// sectionOrEmpty 返回一节的值；节缺失时返回零值，让后续构造函数按“不完整
// 的节”处理。
func sectionOrEmpty(sections map[string]*parsedSection, name string) parsedSection {
	if section := sections[name]; section != nil {
		return *section
	}
	return parsedSection{}
}

// splitInventorySections 按行首标记把输出切成节。返回值依次是：节名到节
// 的映射、开头标记里的命令集版本、是否看到结尾标记。
func splitInventorySections(output string) (map[string]*parsedSection, string, bool, error) {
	sections := map[string]*parsedSection{}
	version := ""
	sawBegin, sawEnd := false, false
	var current *parsedSection
	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimRight(raw, "\r")
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == InventoryMarker {
			switch fields[1] {
			case "BEGIN":
				if len(fields) < 3 {
					continue
				}
				sawBegin = true
				version = fields[2]
			case "END":
				sawEnd = true
				current = nil
			case "SECTION":
				if len(fields) < 3 {
					continue
				}
				name := fields[2]
				sections[name] = &parsedSection{name: name, outcome: sectionIncomplete}
				current = sections[name]
				continue
			case "STATUS":
				if len(fields) >= 4 {
					section, present := sections[fields[2]]
					if !present {
						continue
					}
					if code, err := strconv.Atoi(fields[3]); err == nil && code == 0 {
						section.outcome = sectionComplete
					} else {
						section.outcome = sectionIncomplete
					}
				}
				current = nil
			case "ABSENT":
				if len(fields) < 3 {
					continue
				}
				name := fields[2]
				sections[name] = &parsedSection{name: name, outcome: sectionAbsent}
				current = nil
			}
			continue
		}
		if current != nil {
			current.lines = append(current.lines, line)
		}
	}
	if !sawBegin {
		return nil, "", false, fmt.Errorf("固定命令集输出缺少 %s BEGIN 开头标记", InventoryMarker)
	}
	return sections, version, sawEnd, nil
}

func buildLinuxPackageItems(section parsedSection, systemType string) []models.SoftwareObservedItem {
	if section.outcome == sectionAbsent {
		return nil
	}
	items := make([]models.SoftwareObservedItem, 0, len(section.lines))
	for _, line := range section.lines {
		fields := strings.Split(line, "\t")
		if len(fields) < 4 {
			continue
		}
		// dpkg 的状态缩写第二个字符是当前状态；只有 “已安装” 的包才算
		// 装在这台机器上，“rc”（已删除仅留配置）不算。
		status := fields[3]
		if len(status) < 2 || status[1] != 'i' {
			continue
		}
		name, version, architecture := fields[0], fields[1], fields[2]
		items = append(items, models.SoftwareObservedItem{
			Category:     models.SoftwareCategorySystemPackage,
			Name:         name,
			Version:      version,
			Architecture: architecture,
			PackageURL:   debPackageURL(systemType, name, version, architecture),
		})
	}
	return items
}

func debPackageURL(systemType, name, version, architecture string) string {
	if name == "" || version == "" {
		return ""
	}
	distro := firstNonEmptyString(systemType, "linux")
	purl := fmt.Sprintf("pkg:deb/%s/%s@%s", distro, name, version)
	if architecture != "" {
		purl += "?arch=" + architecture
	}
	return purl
}

var listenProcessPattern = regexp.MustCompile(`\("([^"]*)",pid=(\d+),fd=\d+\)`)

type linuxListenLine struct {
	protocol    string
	address     string
	port        int
	processName string
	processID   int
}

func parseLinuxListenLine(line string) (linuxListenLine, bool) {
	fields := strings.Fields(line)
	if len(fields) < 6 {
		return linuxListenLine{}, false
	}
	listening := linuxListenLine{protocol: strings.TrimSuffix(fields[0], "6")}
	address, port, err := splitListenAddress(fields[4])
	if err != nil {
		return linuxListenLine{}, false
	}
	listening.address, listening.port = address, port
	if matches := listenProcessPattern.FindAllStringSubmatch(line, -1); len(matches) > 0 {
		listening.processName = matches[0][1]
		listening.processID, _ = strconv.Atoi(matches[0][2])
	}
	return listening, true
}

// splitListenAddress 解析 ss 的本地地址列。形如 127.0.0.53%lo:53（带接口
// 范围）、[::]:22（IPv6 字面量）、*:11434（双栈通配）都要能读出地址与端
// 口；接口范围不属于监听地址本身，解析时去掉。
func splitListenAddress(field string) (string, int, error) {
	address, portText, err := net.SplitHostPort(field)
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("端口 %q 不合法", portText)
	}
	if scope := strings.Index(address, "%"); scope >= 0 {
		address = address[:scope]
	}
	if strings.HasPrefix(field, "[") {
		address = "[" + address + "]"
	}
	return address, port, nil
}

func exposureForListenAddress(address string) models.SoftwareExposure {
	address = strings.TrimSuffix(strings.TrimPrefix(address, "["), "]")
	ip := net.ParseIP(address)
	switch {
	case ip == nil:
		if address == "*" {
			return models.SoftwareExposureAllAddresses
		}
		return models.SoftwareExposureSpecificAddress
	case ip.IsLoopback():
		return models.SoftwareExposureLocal
	case ip.IsUnspecified():
		return models.SoftwareExposureAllAddresses
	default:
		return models.SoftwareExposureSpecificAddress
	}
}

func parseLinuxExecutableSection(section parsedSection) map[int]string {
	exes := map[int]string{}
	if section.outcome == sectionAbsent {
		return exes
	}
	for _, line := range section.lines {
		pidText, path, present := strings.Cut(line, "\t")
		if !present {
			continue
		}
		pid, err := strconv.Atoi(pidText)
		if err != nil || !strings.HasPrefix(path, "/") {
			continue
		}
		exes[pid] = path
	}
	return exes
}

func parseLinuxExeOwnerSection(section parsedSection) map[string]string {
	owners := map[string]string{}
	if section.outcome == sectionAbsent {
		return owners
	}
	for _, line := range section.lines {
		path, owner, _ := strings.Cut(line, "\t")
		if strings.HasPrefix(path, "/") && owner != "" {
			owners[path] = owner
		}
	}
	return owners
}

func packageItemIndex(items []models.SoftwareObservedItem) map[string]*models.SoftwareObservedItem {
	index := map[string]*models.SoftwareObservedItem{}
	for i := range items {
		if items[i].Category != models.SoftwareCategorySystemPackage {
			continue
		}
		index[items[i].Name] = &items[i]
	}
	return index
}

func buildLinuxListeningItems(
	listen, exe, exeOwner parsedSection,
	packages map[string]*models.SoftwareObservedItem,
) []models.SoftwareObservedItem {
	if listen.outcome == sectionAbsent && listen.name != "" {
		return nil
	}
	exes := parseLinuxExecutableSection(exe)
	owners := parseLinuxExeOwnerSection(exeOwner)
	unidentified := map[string]*models.SoftwareObservedItem{}
	var order []string
	appendPort := func(item *models.SoftwareObservedItem, line linuxListenLine) {
		if item.ProcessID == 0 && line.processID != 0 {
			item.ProcessID = line.processID
		}
		for _, existing := range item.ListeningPorts {
			if existing.Address == line.address && existing.Port == line.port && existing.Protocol == line.protocol {
				return
			}
		}
		item.ListeningPorts = append(item.ListeningPorts, models.SoftwareListeningPort{
			Address: line.address, Port: line.port, Protocol: line.protocol,
			Exposure: exposureForListenAddress(line.address),
		})
	}
	for _, raw := range listen.lines {
		line, ok := parseLinuxListenLine(raw)
		if !ok {
			continue
		}
		executable := exes[line.processID]
		if owner := owners[executable]; owner != "" {
			if item := packages[owner]; item != nil {
				appendPort(item, line)
				continue
			}
		}
		name := firstNonEmptyString(line.processName, "未识别进程")
		key := unidentifiedStableKey(line.protocol, strconv.Itoa(line.port), executable)
		item, present := unidentified[key]
		if !present {
			item = &models.SoftwareObservedItem{
				Category: models.SoftwareCategoryUnidentified, Name: name,
				ExecutablePath: executable, ProcessName: line.processName,
				ProcessID: line.processID,
			}
			unidentified[key] = item
			order = append(order, key)
		}
		appendPort(item, line)
	}
	items := make([]models.SoftwareObservedItem, 0, len(order))
	for _, key := range order {
		items = append(items, *unidentified[key])
	}
	return items
}

// unidentifiedStableKey 是未识别进程的去重键：同一端口、同一协议、同一可
// 执行文件路径算同一条；两个地址族上的同一端口（例如 0.0.0.0 与 [::] 的
// 51820/udp）合并到一条，端口列表各自保留。
func unidentifiedStableKey(protocol, port, executable string) string {
	return string(models.SoftwareCategoryUnidentified) + "\x1f" + port + "\x1f" + protocol + "\x1f" + executable
}

func buildLinuxContainerItems(images, containers parsedSection) []models.SoftwareObservedItem {
	if images.outcome == sectionAbsent {
		return nil
	}
	runningImages := map[string]bool{}
	if containers.outcome != sectionAbsent {
		for _, line := range containers.lines {
			fields := strings.Split(line, "\t")
			if len(fields) >= 2 {
				runningImages[fields[1]] = true
			}
		}
	}
	items := make([]models.SoftwareObservedItem, 0, len(images.lines))
	for _, line := range images.lines {
		fields := strings.Split(line, "\t")
		if len(fields) < 4 {
			continue
		}
		repository, tag, digest := fields[0], fields[1], fields[2]
		if tag == "<none>" {
			tag = ""
		}
		if digest == "<none>" {
			digest = ""
		}
		item := models.SoftwareObservedItem{
			Category: models.SoftwareCategoryContainerImage, Name: repository,
			Version: firstNonEmptyString(tag, digest), BelongsTo: "docker",
			ContainerTag: tag, ContainerDigest: digest,
		}
		for imageRef := range runningImages {
			if containerRefMatchesImage(imageRef, repository, tag, digest) {
				item.ContainerRunning = true
				break
			}
		}
		if digest != "" {
			item.PackageURL = dockerPackageURL(repository, digest, tag)
		}
		items = append(items, item)
	}
	return items
}

func containerRefMatchesImage(imageRef, repository, tag, digest string) bool {
	if imageRef == repository {
		return true
	}
	if tag != "" && imageRef == repository+":"+tag {
		return true
	}
	if digest != "" && imageRef == repository+"@"+digest {
		return true
	}
	return tag != "" && digest != "" && imageRef == repository+":"+tag+"@"+digest
}

func dockerPackageURL(repository, digest, tag string) string {
	purl := "pkg:docker/" + repository + "@" + digest
	if tag != "" {
		purl += "?tag=" + tag
	}
	return purl
}

func buildLinuxFirmwareItems(section parsedSection) []models.SoftwareObservedItem {
	if section.outcome == sectionAbsent {
		return nil
	}
	values := map[string]string{}
	for _, line := range section.lines {
		key, value, present := strings.Cut(line, "\t")
		if present && !strings.HasPrefix(value, "cat:") && value != "" {
			values[key] = value
		}
	}
	if values["bios_version"] == "" {
		return nil
	}
	return []models.SoftwareObservedItem{{
		Category: models.SoftwareCategoryDriverFirmware, Name: "BIOS",
		Version: values["bios_version"], BelongsTo: values["bios_vendor"],
	}}
}

var nvidiaVersionPattern = regexp.MustCompile(`NVRM version:[^\d]*(\d+(?:\.\d+)+)`)
var nvidiaSmiPattern = regexp.MustCompile(`^[^,]+,\s*(\d+(?:\.\d+)+)$`)

func buildLinuxGPUDriverItems(section parsedSection) []models.SoftwareObservedItem {
	if section.outcome == sectionAbsent {
		return nil
	}
	for _, line := range section.lines {
		if match := nvidiaVersionPattern.FindStringSubmatch(line); match != nil {
			return []models.SoftwareObservedItem{{
				Category: models.SoftwareCategoryDriverFirmware,
				Name:     "NVIDIA 显卡驱动", Version: match[1],
			}}
		}
	}
	for _, line := range section.lines {
		if match := nvidiaSmiPattern.FindStringSubmatch(line); match != nil {
			return []models.SoftwareObservedItem{{
				Category: models.SoftwareCategoryDriverFirmware,
				Name:     "NVIDIA 显卡驱动", Version: match[1],
			}}
		}
	}
	return nil
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// sortObservedPortsByName 给监听端口一个稳定顺序，避免同一份数据两次采集
// 因为端口顺序不同而被当成变化。
func sortObservedPortsByName(ports []models.SoftwareListeningPort) {
	sort.Slice(ports, func(i, j int) bool {
		if ports[i].Port != ports[j].Port {
			return ports[i].Port < ports[j].Port
		}
		if ports[i].Protocol != ports[j].Protocol {
			return ports[i].Protocol < ports[j].Protocol
		}
		return ports[i].Address < ports[j].Address
	})
}
