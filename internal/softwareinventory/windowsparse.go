package softwareinventory

import (
	"cmp"
	"fmt"
	"strconv"
	"strings"

	"ssh-manager-mcp/internal/models"
)

// parseWindowsInventoryOutput 解析 Windows 固定命令集的输出。输出没有开头
// 标记时返回错误；节缺失、节内命令失败或输出截断时保留已解析条目并标为
// 不完整。
func parseWindowsInventoryOutput(output string) (*ParsedCollection, error) {
	sections, version, sawEnd, err := splitInventorySections(output)
	if err != nil {
		return nil, err
	}
	collection := &ParsedCollection{CommandSetVersion: version}
	for _, name := range windowsV1Sections {
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

	osInfo := parseWindowsOSSection(sectionOrEmpty(sections, "os").lines)
	collection.SystemType = osInfo.systemType
	if collection.SystemType == "" {
		collection.Incomplete = true
		collection.Notes = append(collection.Notes, "系统识别结果为空")
	}
	if osInfo.recognized {
		collection.Items = append(collection.Items, buildWindowsOSItem(osInfo))
	}
	installed, installedIndexes := buildWindowsInstalledItems(sectionOrEmpty(sections, "installed"))
	collection.Items = append(collection.Items, installed...)
	installedByName := map[string]*models.SoftwareObservedItem{}
	installedStart := len(collection.Items) - len(installed)
	for name, index := range installedIndexes {
		installedByName[name] = &collection.Items[installedStart+index]
	}
	collection.Items = append(collection.Items, buildWindowsListeningItems(
		sectionOrEmpty(sections, "listen-tcp"),
		sectionOrEmpty(sections, "listen-udp"),
		sectionOrEmpty(sections, "processes"),
		sectionOrEmpty(sections, "services"),
		installedByName,
	)...)
	collection.Items = append(collection.Items,
		buildWindowsFirmwareItems(sectionOrEmpty(sections, "firmware"))...)
	collection.Items = append(collection.Items,
		buildWindowsGPUDriverItems(sectionOrEmpty(sections, "gpu-driver"))...)
	return collection, nil
}

type windowsOSInfo struct {
	productName      string
	displayVersion   string
	releaseVersion   string
	currentBuild     string
	updateRevision   string
	edition          string
	installationType string
	architecture     string
	recognized       bool
	systemType       string
}

func parseWindowsOSSection(lines []string) windowsOSInfo {
	info := windowsOSInfo{}
	for _, line := range lines {
		key, value, present := strings.Cut(line, "\t")
		if !present || value == "" {
			continue
		}
		info.recognized = true
		switch key {
		case "ProductName":
			info.productName = value
		case "DisplayVersion":
			info.displayVersion = value
		case "ReleaseId":
			info.releaseVersion = value
		case "CurrentBuild":
			info.currentBuild = value
		case "UBR":
			// UBR 是注册表输出里的更新修订号。
			info.updateRevision = value
		case "EditionID":
			info.edition = value
		case "InstallationType":
			info.installationType = value
		case "arch":
			info.architecture = value
		}
	}
	if info.recognized {
		info.systemType = "windows"
	}
	return info
}

func buildWindowsOSItem(info windowsOSInfo) models.SoftwareObservedItem {
	version := cmp.Or(info.displayVersion, info.releaseVersion, info.currentBuild)
	if version == "" {
		version = "unknown"
	}
	name := cmp.Or(info.productName, "Windows")
	return models.SoftwareObservedItem{
		Category:     models.SoftwareCategoryOperatingSystem,
		Name:         name,
		Version:      version,
		Architecture: info.architecture,
	}
}

func buildWindowsInstalledItems(
	section parsedSection,
) ([]models.SoftwareObservedItem, map[string]int) {
	if section.outcome == sectionAbsent {
		return nil, map[string]int{}
	}
	items := make([]models.SoftwareObservedItem, 0, len(section.lines))
	registryKeys := make([]string, 0, len(section.lines))
	indexes := map[string]int{}
	for _, line := range section.lines {
		fields := strings.Split(line, "\t")
		if len(fields) < 5 || fields[1] == "" {
			continue
		}
		view, name, version, publisher, registryKey := fields[0], fields[1], fields[2], fields[3], fields[4]
		item := models.SoftwareObservedItem{
			Category:     models.SoftwareCategorySystemPackage,
			Name:         name,
			Version:      version,
			BelongsTo:    publisher,
			Architecture: view,
			StableKey: strings.Join([]string{
				string(models.SoftwareCategorySystemPackage), strings.ToLower(name),
				publisher, view,
			}, "\x1f"),
		}
		items = append(items, item)
		registryKeys = append(registryKeys, registryKey)
		index := len(items) - 1
		for _, matchName := range []string{name, registryKey} {
			key := normalizeWindowsSoftwareName(matchName)
			if key != "" {
				indexes[key] = index
			}
		}
	}
	disambiguateWindowsStableKeysByRegistryKey(items, registryKeys)
	return items, indexes
}

func disambiguateWindowsStableKeysByRegistryKey(items []models.SoftwareObservedItem, registryKeys []string) {
	counts := make(map[string]int, len(items))
	for _, item := range items {
		counts[item.StableKey]++
	}
	for i := range items {
		if counts[items[i].StableKey] > 1 {
			items[i].StableKey += "\x1f" + registryKeys[i]
		}
	}
}

type windowsProcess struct {
	processID int
	name      string
	path      string
}

type windowsListenLine struct {
	protocol  string
	address   string
	port      int
	processID int
}

func parseWindowsProcesses(section parsedSection) map[int]windowsProcess {
	processes := map[int]windowsProcess{}
	if section.outcome == sectionAbsent {
		return processes
	}
	for _, line := range section.lines {
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			continue
		}
		processID, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		path := ""
		if len(fields) >= 3 {
			path = fields[2]
		}
		processes[processID] = windowsProcess{processID: processID, name: fields[1], path: path}
	}
	return processes
}

func parseWindowsServices(section parsedSection) map[int][]string {
	services := map[int][]string{}
	if section.outcome == sectionAbsent {
		return services
	}
	for _, line := range section.lines {
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			continue
		}
		processID, err := strconv.Atoi(fields[0])
		if err != nil || fields[1] == "" {
			continue
		}
		services[processID] = append(services[processID], fields[1])
	}
	return services
}

func parseWindowsListenSection(section parsedSection, protocol string) []windowsListenLine {
	lines := make([]windowsListenLine, 0, len(section.lines))
	if section.outcome == sectionAbsent {
		return lines
	}
	for _, line := range section.lines {
		fields := strings.Split(line, "\t")
		if len(fields) < 3 {
			continue
		}
		port, portErr := strconv.Atoi(fields[1])
		processID, processErr := strconv.Atoi(fields[2])
		if portErr != nil || processErr != nil || port < 1 || port > 65535 {
			continue
		}
		lines = append(lines, windowsListenLine{
			protocol: protocol, address: fields[0], port: port, processID: processID,
		})
	}
	return lines
}

func buildWindowsListeningItems(
	listenTCP, listenUDP, processesSection, servicesSection parsedSection,
	installedByName map[string]*models.SoftwareObservedItem,
) []models.SoftwareObservedItem {
	if listenTCP.outcome == sectionAbsent && listenUDP.outcome == sectionAbsent {
		return nil
	}
	processes := parseWindowsProcesses(processesSection)
	services := parseWindowsServices(servicesSection)
	serviceItems := map[string]*models.SoftwareObservedItem{}
	unidentified := map[string]*models.SoftwareObservedItem{}
	var serviceOrder []string
	var unidentifiedOrder []string

	appendPort := func(item *models.SoftwareObservedItem, line windowsListenLine) {
		if item.ProcessID == 0 {
			item.ProcessID = line.processID
		}
		for _, existing := range item.ListeningPorts {
			if existing.Address == line.address && existing.Port == line.port &&
				existing.Protocol == line.protocol {
				return
			}
		}
		item.ListeningPorts = append(item.ListeningPorts, models.SoftwareListeningPort{
			Address: line.address, Port: line.port, Protocol: line.protocol,
			Exposure: exposureForListenAddress(line.address),
		})
	}

	for _, line := range append(
		parseWindowsListenSection(listenTCP, "tcp"),
		parseWindowsListenSection(listenUDP, "udp")...,
	) {
		process := processes[line.processID]
		if process.name == "" && process.path == "" {
			unidentifiedOrder = appendWindowsUnidentifiedItem(
				unidentified, unidentifiedOrder, process, line, appendPort,
			)
			continue
		}
		if item := installedByName[normalizeWindowsSoftwareName(process.name)]; item != nil {
			item.ProcessName = process.name
			item.ExecutablePath = process.path
			appendPort(item, line)
			continue
		}
		if names := services[line.processID]; len(names) > 0 {
			for _, serviceName := range names {
				key := strconv.Itoa(line.processID) + "\x1f" + serviceName
				item, present := serviceItems[key]
				if !present {
					item = &models.SoftwareObservedItem{
						Category:       models.SoftwareCategoryServiceProgram,
						Name:           serviceName,
						ProcessName:    process.name,
						ExecutablePath: process.path,
						ProcessID:      line.processID,
						StableKey: strings.Join([]string{
							string(models.SoftwareCategoryServiceProgram),
							strings.ToLower(serviceName), process.path,
						}, "\x1f"),
					}
					serviceItems[key] = item
					serviceOrder = append(serviceOrder, key)
				}
				appendPort(item, line)
			}
			continue
		}
		unidentifiedOrder = appendWindowsUnidentifiedItem(
			unidentified, unidentifiedOrder, process, line, appendPort,
		)
	}

	items := make([]models.SoftwareObservedItem, 0, len(serviceOrder)+len(unidentifiedOrder))
	for _, key := range serviceOrder {
		items = append(items, *serviceItems[key])
	}
	for _, key := range unidentifiedOrder {
		items = append(items, *unidentified[key])
	}
	return items
}

func appendWindowsUnidentifiedItem(
	unidentified map[string]*models.SoftwareObservedItem,
	order []string,
	process windowsProcess,
	line windowsListenLine,
	appendPort func(item *models.SoftwareObservedItem, line windowsListenLine),
) []string {
	key := unidentifiedStableKey(line.protocol, strconv.Itoa(line.port), process.path)
	item, present := unidentified[key]
	if !present {
		item = &models.SoftwareObservedItem{
			Category:       models.SoftwareCategoryUnidentified,
			Name:           cmp.Or(process.name, "未识别进程"),
			ProcessName:    process.name,
			ExecutablePath: process.path,
			ProcessID:      line.processID,
			StableKey:      key,
		}
		unidentified[key] = item
		order = append(order, key)
	}
	appendPort(item, line)
	return order
}

func normalizeWindowsSoftwareName(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".exe")
}

func buildWindowsFirmwareItems(section parsedSection) []models.SoftwareObservedItem {
	if section.outcome == sectionAbsent {
		return nil
	}
	values := map[string]string{}
	for _, line := range section.lines {
		key, value, present := strings.Cut(line, "\t")
		if present && value != "" {
			values[key] = value
		}
	}
	if values["bios_version"] == "" {
		return nil
	}
	return []models.SoftwareObservedItem{{
		Category:  models.SoftwareCategoryDriverFirmware,
		Name:      "BIOS",
		Version:   values["bios_version"],
		BelongsTo: values["bios_vendor"],
	}}
}

func buildWindowsGPUDriverItems(section parsedSection) []models.SoftwareObservedItem {
	if section.outcome == sectionAbsent {
		return nil
	}
	items := make([]models.SoftwareObservedItem, 0, len(section.lines))
	for _, line := range section.lines {
		fields := strings.Split(line, "\t")
		if len(fields) < 3 || fields[0] == "" || fields[1] == "" {
			continue
		}
		items = append(items, models.SoftwareObservedItem{
			Category: models.SoftwareCategoryDriverFirmware,
			Name:     fields[0],
			Version:  fields[1],
		})
	}
	return items
}
