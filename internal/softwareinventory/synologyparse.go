package softwareinventory

import (
	"net"
	"strconv"
	"strings"

	"ssh-manager-mcp/internal/models"
)

// buildSynologyPackageItems 解析套件中心输出。套件名本身可以包含连字符，
// 因此版本取最后两段，而不是最后一段。
func buildSynologyPackageItems(section parsedSection) []models.SoftwareObservedItem {
	if section.outcome == sectionAbsent {
		return nil
	}
	items := make([]models.SoftwareObservedItem, 0, len(section.lines))
	for _, line := range section.lines {
		packageText, _, present := strings.Cut(line, ":")
		if !present {
			packageText = line
		}
		first := strings.LastIndex(packageText, "-")
		if first < 0 {
			continue
		}
		second := strings.LastIndex(packageText[:first], "-")
		if second < 0 {
			continue
		}
		name, version := packageText[:second], packageText[second+1:]
		if name == "" || version == "" {
			continue
		}
		items = append(items, models.SoftwareObservedItem{
			Category: models.SoftwareCategorySystemPackage,
			Name:     name, Version: version,
		})
	}
	return items
}

type netstatListenLine struct {
	protocol    string
	address     string
	port        int
	processName string
	processID   int
}

// parseNetstatListenSection 解析传统 netstat 与 busybox netstat 的监听
// 行；表头和权限提示会被协议字段过滤掉。
func parseNetstatListenSection(section parsedSection) []netstatListenLine {
	if section.outcome == sectionAbsent {
		return nil
	}
	lines := make([]netstatListenLine, 0, len(section.lines))
	for _, raw := range section.lines {
		fields := strings.Fields(raw)
		if len(fields) < 6 {
			continue
		}
		protocol := strings.ToLower(fields[0])
		if protocol != "tcp" && protocol != "tcp6" && protocol != "udp" && protocol != "udp6" {
			continue
		}
		if protocol == "tcp6" || protocol == "udp6" {
			protocol = protocol[:3]
		}
		address, port, err := splitNetstatListenAddress(fields[3])
		if err != nil {
			continue
		}
		processName, processID := parseNetstatProcess(fields[len(fields)-1])
		lines = append(lines, netstatListenLine{
			protocol: protocol, address: address, port: port,
			processName: processName, processID: processID,
		})
	}
	return lines
}

// splitNetstatListenAddress 解析本地地址列，并把 IPv6 地址统一写成带方
// 括号的形式。
func splitNetstatListenAddress(field string) (string, int, error) {
	address, portText, err := net.SplitHostPort(field)
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, err
	}
	if strings.Contains(address, ":") {
		address = "[" + address + "]"
	}
	return address, port, nil
}

func parseNetstatProcess(field string) (string, int) {
	if field == "-" {
		return "", 0
	}
	pidText, name, present := strings.Cut(field, "/")
	if !present {
		return "", 0
	}
	pid, err := strconv.Atoi(pidText)
	if err != nil {
		return "", 0
	}
	return name, pid
}

// buildNetstatListeningItems 把 netstat 监听端口挂回可选的软件条目列
// 表；没有软件条目时全部按未识别进程保留，供基础采集系统使用。
func buildNetstatListeningItems(
	listen, executable parsedSection,
	items []models.SoftwareObservedItem,
) []models.SoftwareObservedItem {
	if listen.outcome == sectionAbsent {
		return nil
	}
	exes := parseLinuxExecutableSection(executable)
	unidentified := map[string]*models.SoftwareObservedItem{}
	var order []string
	appendPort := func(item *models.SoftwareObservedItem, line netstatListenLine) {
		if item.ProcessID == 0 {
			item.ProcessID = line.processID
		}
		if item.ProcessName == "" {
			item.ProcessName = line.processName
		}
		if item.ExecutablePath == "" {
			item.ExecutablePath = exes[line.processID]
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
	for _, line := range parseNetstatListenSection(listen) {
		executablePath := exes[line.processID]
		if item := synologyItemForExecutable(items, executablePath); item != nil {
			appendPort(item, line)
			continue
		}
		key := unidentifiedStableKey(line.protocol, strconv.Itoa(line.port), executablePath)
		item, present := unidentified[key]
		if !present {
			item = &models.SoftwareObservedItem{
				Category:    models.SoftwareCategoryUnidentified,
				Name:        firstNonEmptyString(line.processName, "未识别进程"),
				ProcessName: line.processName, ExecutablePath: executablePath,
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

// synologyItemForExecutable 从群晖套件安装路径里反查套件条目。
func synologyItemForExecutable(
	items []models.SoftwareObservedItem,
	executablePath string,
) *models.SoftwareObservedItem {
	marker := "/@appstore/"
	position := strings.Index(executablePath, marker)
	if position < 0 {
		return nil
	}
	rest := executablePath[position+len(marker):]
	packageName, _, _ := strings.Cut(rest, "/")
	if packageName == "" {
		return nil
	}
	for i := range items {
		if items[i].Category == models.SoftwareCategorySystemPackage && items[i].Name == packageName {
			return &items[i]
		}
	}
	return nil
}
