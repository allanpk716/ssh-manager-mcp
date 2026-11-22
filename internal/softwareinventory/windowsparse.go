package softwareinventory

import (
	"cmp"
	"fmt"
	"strconv"
	"strings"

	"ssh-manager-mcp/internal/models"
)

// parseWindowsInventoryOutput 解析 Windows 固定命令集的输出，按开头标记里
// 的命令集版本选择解析规则：第 1 版保持升级前的口径，第 2 版起系统自带服
// 务并入操作系统条目、其余监听进程带文件版本与产品名。输出没有开头标记
// 时返回错误；节缺失、节内命令失败或输出截断时保留已解析条目并标为不完
// 整。
func parseWindowsInventoryOutput(output string) (*ParsedCollection, error) {
	sections, version, sawEnd, err := splitInventorySections(output)
	if err != nil {
		return nil, err
	}
	collection := &ParsedCollection{CommandSetVersion: version}
	for _, name := range windowsCommandSections {
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

	v2 := version != windowsCommandSetV1Version
	osInfo := parseWindowsOSSection(sectionOrEmpty(sections, "os").lines)
	collection.SystemType = osInfo.systemType
	if collection.SystemType == "" {
		collection.Incomplete = true
		collection.Notes = append(collection.Notes, "系统识别结果为空")
	}
	operatingSystemIndex := -1
	if osInfo.recognized {
		operatingSystemIndex = len(collection.Items)
		collection.Items = append(collection.Items, buildWindowsOSItem(osInfo, v2))
	}
	installed, installedIndexes := buildWindowsInstalledItems(sectionOrEmpty(sections, "installed"))
	collection.Items = append(collection.Items, installed...)
	installedByName := map[string]*models.SoftwareObservedItem{}
	installedStart := len(collection.Items) - len(installed)
	for name, index := range installedIndexes {
		installedByName[name] = &collection.Items[installedStart+index]
	}
	listeningItems, operatingSystemPorts := buildWindowsListeningItems(
		sectionOrEmpty(sections, "listen-tcp"),
		sectionOrEmpty(sections, "listen-udp"),
		sectionOrEmpty(sections, "processes"),
		sectionOrEmpty(sections, "services"),
		installedByName, v2,
	)
	collection.Items = append(collection.Items, listeningItems...)
	if len(operatingSystemPorts) > 0 && operatingSystemIndex >= 0 {
		// 系统自带服务的监听端口并入操作系统条目；端口来自多个进程，条
		// 目本身不带进程编号。os 节整节为空时没有可并入的条目，这些端
		// 口随「系统识别结果为空」一起标为不完整。
		operatingSystem := &collection.Items[operatingSystemIndex]
		operatingSystem.ListeningPorts = append(
			operatingSystem.ListeningPorts, operatingSystemPorts...,
		)
	}
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
	majorVersion     string
	minorVersion     string
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
		case "CurrentMajorVersionNumber":
			info.majorVersion = value
		case "CurrentMinorVersionNumber":
			info.minorVersion = value
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

// buildWindowsOSItem 构造操作系统条目。fullVersion 为 true（第 2 版及以后
// 的输出）时，主版本、次版本、构建号与补丁修订号都非空就拼成完整四段版
// 本号，任一为空回退第 1 版口径；Windows 10 之前的注册表没有两个版本号
// 键，输出空值行，自然走回退。
func buildWindowsOSItem(info windowsOSInfo, fullVersion bool) models.SoftwareObservedItem {
	version := ""
	if fullVersion && info.majorVersion != "" && info.minorVersion != "" &&
		info.currentBuild != "" && info.updateRevision != "" {
		version = strings.Join([]string{
			info.majorVersion, info.minorVersion, info.currentBuild, info.updateRevision,
		}, ".")
	} else {
		version = cmp.Or(info.displayVersion, info.releaseVersion, info.currentBuild)
		if version == "" {
			version = "unknown"
		}
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
	processID   int
	name        string
	path        string
	fileVersion string
	productName string
}

type windowsListenLine struct {
	protocol  string
	address   string
	port      int
	processID int
}

// windowsSystemProcess 判定一个监听进程是否属于 Windows 系统自带服务：
// 可执行文件路径在 Windows 系统目录下（形如 <盘符>:\windows\...，盘符不
// 限、windows 段大小写不敏感；svchost.exe、spoolsv.exe、DriverStore 内
// 的驱动服务都在此列），或路径为空且进程名为 System（Windows 内核进程，
// 监听 135、137-139、445 等）。
func windowsSystemProcess(process windowsProcess) bool {
	if process.path == "" {
		return strings.EqualFold(process.name, "system")
	}
	if len(process.path) < 3 || process.path[1] != ':' || process.path[2] != '\\' {
		return false
	}
	return strings.HasPrefix(strings.ToLower(process.path[3:]), "windows\\")
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
		// 第 2 版起第 4、5 列是可执行文件自带的文件版本与产品名；第 1
		// 版只有前三列，两列按空处理。
		fileVersion, productName := "", ""
		if len(fields) >= 4 {
			fileVersion = fields[3]
		}
		if len(fields) >= 5 {
			productName = fields[4]
		}
		path := ""
		if len(fields) >= 3 {
			path = fields[2]
		}
		processes[processID] = windowsProcess{
			processID: processID, name: fields[1], path: path,
			fileVersion: fileVersion, productName: productName,
		}
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

// buildWindowsListeningItems 把监听端口归到条目上。mergeSystemIntoOS 为
// true（第 2 版及以后）时，系统自带服务的宿主进程不单独成条，其端口去重
// 后作为第二个返回值交给调用方并入操作系统条目。服务程序条目与未识别进
// 程条目在第 2 版输出下带文件版本；第 1 版输出没有这两列，取到的是空值，
// 行为与升级前一致。
func buildWindowsListeningItems(
	listenTCP, listenUDP, processesSection, servicesSection parsedSection,
	installedByName map[string]*models.SoftwareObservedItem,
	mergeSystemIntoOS bool,
) ([]models.SoftwareObservedItem, []models.SoftwareListeningPort) {
	if listenTCP.outcome == sectionAbsent && listenUDP.outcome == sectionAbsent {
		return nil, nil
	}
	processes := parseWindowsProcesses(processesSection)
	services := parseWindowsServices(servicesSection)
	serviceItems := map[string]*models.SoftwareObservedItem{}
	unidentified := map[string]*models.SoftwareObservedItem{}
	var serviceOrder []string
	var unidentifiedOrder []string
	var operatingSystemPorts []models.SoftwareListeningPort

	appendPort := func(item *models.SoftwareObservedItem, line windowsListenLine) {
		if item.ProcessID == 0 {
			item.ProcessID = line.processID
		}
		appendListenPort(item, line)
	}
	appendOperatingSystemPort := func(line windowsListenLine) {
		for _, existing := range operatingSystemPorts {
			if existing.Address == line.address && existing.Port == line.port &&
				existing.Protocol == line.protocol {
				return
			}
		}
		operatingSystemPorts = append(operatingSystemPorts, listenPortFromLine(line))
	}

	for _, line := range append(
		parseWindowsListenSection(listenTCP, "tcp"),
		parseWindowsListenSection(listenUDP, "udp")...,
	) {
		process := processes[line.processID]
		if mergeSystemIntoOS && windowsSystemProcess(process) {
			appendOperatingSystemPort(line)
			continue
		}
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
						Version:        process.fileVersion,
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
	return items, operatingSystemPorts
}

// listenPortFromLine 把一条监听记录转换成软件条目的监听端口，暴露级别按
// 本地地址推导。
func listenPortFromLine(line windowsListenLine) models.SoftwareListeningPort {
	return models.SoftwareListeningPort{
		Address: line.address, Port: line.port, Protocol: line.protocol,
		Exposure: exposureForListenAddress(line.address),
	}
}

// appendListenPort 把一条监听记录追加到条目的端口列表末尾；地址、端口与
// 协议都相同的不重复追加。
func appendListenPort(item *models.SoftwareObservedItem, line windowsListenLine) {
	for _, existing := range item.ListeningPorts {
		if existing.Address == line.address && existing.Port == line.port &&
			existing.Protocol == line.protocol {
			return
		}
	}
	item.ListeningPorts = append(item.ListeningPorts, listenPortFromLine(line))
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
		// 第 2 版起条目名优先取产品名（第 1 版输出里恒空，仍取进程名），
		// 版本取文件版本；产品名不进稳定键。
		item = &models.SoftwareObservedItem{
			Category:       models.SoftwareCategoryUnidentified,
			Name:           cmp.Or(process.productName, process.name, "未识别进程"),
			Version:        process.fileVersion,
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
