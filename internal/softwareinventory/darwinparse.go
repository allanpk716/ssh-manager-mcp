package softwareinventory

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"ssh-manager-mcp/internal/models"
)

// buildDarwinApplicationItems 解析应用程序目录的版本与包标识。
func buildDarwinApplicationItems(section parsedSection) []models.SoftwareObservedItem {
	if section.outcome == sectionAbsent {
		return nil
	}
	items := make([]models.SoftwareObservedItem, 0, len(section.lines))
	for _, line := range section.lines {
		fields := strings.Split(line, "\t")
		if len(fields) != 3 || !strings.HasSuffix(fields[0], ".app") || fields[1] == "" {
			continue
		}
		items = append(items, models.SoftwareObservedItem{
			Category:       models.SoftwareCategoryServiceProgram,
			Name:           strings.TrimSuffix(filepath.Base(fields[0]), ".app"),
			Version:        fields[1],
			BelongsTo:      fields[2],
			ExecutablePath: fields[0],
		})
	}
	return items
}

// buildDarwinBrewItems 解析 Homebrew 的 formula 与 cask。一行里可能有多个
// 版本；每个版本是一条实测条目，稳定键因此额外包含版本，避免同名多版本
// 在同一台设备上冲突。
func buildDarwinBrewItems(section parsedSection) []models.SoftwareObservedItem {
	if section.outcome == sectionAbsent {
		return nil
	}
	items := make([]models.SoftwareObservedItem, 0, len(section.lines))
	for _, line := range section.lines {
		fields := strings.Split(line, "\t")
		if len(fields) != 2 {
			continue
		}
		parts := strings.Fields(fields[1])
		if len(parts) < 2 || (fields[0] != "formula" && fields[0] != "cask") {
			continue
		}
		for _, version := range parts[1:] {
			items = append(items, models.SoftwareObservedItem{
				Category:   models.SoftwareCategorySystemPackage,
				Name:       parts[0],
				Version:    version,
				BelongsTo:  "Homebrew " + fields[0],
				PackageURL: fmt.Sprintf("pkg:brew/%s@%s", parts[0], version),
				StableKey: strings.Join([]string{
					string(models.SoftwareCategorySystemPackage),
					strings.ToLower(parts[0]), "Homebrew " + fields[0], version,
				}, "\x1f"),
			})
		}
	}
	return items
}

type darwinListenLine struct {
	processID   int
	processName string
	protocol    string
	address     string
	port        int
}

// parseDarwinLsofSection 解析 lsof 的字段输出。进程编号与命令名会先记
// 录，地址行到达后再补齐命令名，兼容同一进程被 lsof 输出多次的情况。
func parseDarwinLsofSection(section parsedSection) []darwinListenLine {
	if section.outcome == sectionAbsent {
		return nil
	}
	processNames := map[int]string{}
	var order []darwinListenLine
	currentPID := 0
	currentProtocol := ""
	appendLine := func(addressText string) {
		address, port, err := splitListenAddress(addressText)
		if err != nil || port == 0 || currentProtocol == "" || currentPID == 0 {
			return
		}
		line := darwinListenLine{
			processID: currentPID, processName: processNames[currentPID],
			protocol: currentProtocol, address: address, port: port,
		}
		for _, existing := range order {
			if existing == line {
				return
			}
		}
		order = append(order, line)
	}
	for _, line := range section.lines {
		if value, present := strings.CutPrefix(line, "p"); present {
			pid, err := strconv.Atoi(value)
			if err != nil {
				currentPID = 0
				continue
			}
			currentPID = pid
			continue
		}
		if value, present := strings.CutPrefix(line, "c"); present {
			if currentPID != 0 {
				processNames[currentPID] = value
			}
			continue
		}
		if value, present := strings.CutPrefix(line, "P"); present {
			currentProtocol = strings.ToLower(value)
			continue
		}
		if value, present := strings.CutPrefix(line, "n"); present {
			appendLine(value)
		}
	}
	for i := range order {
		order[i].processName = processNames[order[i].processID]
	}
	return order
}

// parseDarwinExecutableSection 解析 macOS 的 ps 命令输出；它可能是绝对
// 路径，也可能只有命令名。
func parseDarwinExecutableSection(section parsedSection) map[int]string {
	exes := map[int]string{}
	if section.outcome == sectionAbsent {
		return exes
	}
	for _, line := range section.lines {
		pidText, command, present := strings.Cut(line, "\t")
		if !present || command == "" {
			continue
		}
		pid, err := strconv.Atoi(pidText)
		if err != nil {
			continue
		}
		exes[pid] = command
	}
	return exes
}

// buildDarwinListeningItems 把监听端口挂回应用或 Homebrew 条目；两边都
// 认不出的进程按未识别进程保留。
func buildDarwinListeningItems(
	listen, executable parsedSection,
	items []models.SoftwareObservedItem,
) []models.SoftwareObservedItem {
	if listen.outcome == sectionAbsent {
		return nil
	}
	exes := parseDarwinExecutableSection(executable)
	unidentified := map[string]*models.SoftwareObservedItem{}
	var order []string
	appendPort := func(item *models.SoftwareObservedItem, line darwinListenLine) {
		if item.ProcessID == 0 {
			item.ProcessID = line.processID
		}
		if item.ProcessName == "" {
			item.ProcessName = line.processName
		}
		if item.ExecutablePath == "" || strings.HasPrefix(item.ExecutablePath, "/Applications/") {
			if executable := exes[line.processID]; executable != "" && strings.HasPrefix(executable, "/") {
				item.ExecutablePath = executable
			}
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
	for _, line := range parseDarwinLsofSection(listen) {
		executable := exes[line.processID]
		if item := darwinItemForProcess(items, line.processName, executable); item != nil {
			appendPort(item, line)
			continue
		}
		key := unidentifiedStableKey(line.protocol, strconv.Itoa(line.port), executable)
		item, present := unidentified[key]
		if !present {
			item = &models.SoftwareObservedItem{
				Category:    models.SoftwareCategoryUnidentified,
				Name:        firstNonEmptyString(line.processName, "未识别进程"),
				ProcessName: line.processName, ExecutablePath: executable,
				ProcessID: line.processID,
			}
			unidentified[key] = item
			order = append(order, key)
		}
		appendPort(item, line)
	}
	result := make([]models.SoftwareObservedItem, 0, len(order))
	for _, key := range order {
		result = append(result, *unidentified[key])
	}
	return result
}

// darwinItemForProcess 用应用程序包路径或 Homebrew 安装路径反查软件条目。
func darwinItemForProcess(
	items []models.SoftwareObservedItem,
	processName, executable string,
) *models.SoftwareObservedItem {
	processLower := strings.ToLower(processName)
	executableBase := strings.ToLower(filepath.Base(executable))
	for i := range items {
		item := &items[i]
		if item.Category == models.SoftwareCategoryServiceProgram && strings.HasPrefix(executable, "/Applications/") {
			if appDirectory := darwinApplicationDirectory(executable); appDirectory != "" &&
				strings.TrimSuffix(filepath.Base(appDirectory), ".app") == item.Name {
				return item
			}
		}
	}
	for i := range items {
		item := &items[i]
		if item.Category != models.SoftwareCategorySystemPackage ||
			!strings.Contains(item.BelongsTo, "Homebrew") {
			continue
		}
		name := strings.ToLower(item.Name)
		if name == processLower || name == executableBase ||
			strings.Contains(executable, "/Cellar/"+item.Name+"/") ||
			strings.Contains(executable, "/opt/"+item.Name+"/") {
			return item
		}
	}
	return nil
}

func darwinApplicationDirectory(executable string) string {
	marker := "/Contents/"
	position := strings.Index(executable, marker)
	if position < 0 {
		return ""
	}
	return executable[:position]
}
